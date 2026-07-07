// Copyright 2026 opendefense contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller_test

// M2 dual-target e2e — the real production path.
//
// An in-process multicluster-runtime manager runs against the APIExport virtual
// workspace (the same wiring cmd/controller/main.go uses). A consumer creates a
// KubernetesCluster{region: eu}; the shared Reconciler materializes:
//   1. a consumer-target ConfigMap in the CONSUMER workspace, written THROUGH the
//      vw as the APIExport identity (requires the configmaps permissionClaim to
//      be accepted by the consumer's APIBinding);
//   2. a collision-free-named provider-target ConfigMap in the PROVIDER workspace,
//      via a direct provider-workspace client;
//   3. the instance status.configMapName projection.
//
// Load-bearing ordering (from the vw spike): kcp only advertises the APIExport vw
// URL once at least one APIBinding consumes the export, so the consumer workspace
// + APIBinding are created BEFORE awaiting APIExportEndpointSlice endpoints.

import (
	"context"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kcp-dev/sdk/apis/core"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

const m2ExportName = "kubernetesclusters.krop.opendefense.cloud"

// applyFixtureFromFile reads a YAML fixture, substitutes ${KEY} placeholders from
// vars, and creates the resulting object in the given workspace.
func applyFixtureFromFile(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, file string, vars map[string]string) {
	GinkgoHelper()
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred())
	text := string(raw)
	for k, v := range vars {
		text = strings.ReplaceAll(text, "${"+k+"}", v)
	}
	u := &unstructured.Unstructured{}
	Expect(yaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096).Decode(u)).To(Succeed())
	Expect(cli.Cluster(wsPath).Create(ctx, u)).To(Succeed())
}

var _ = Describe("M2 dual-target reconcile", Ordered, func() {
	var (
		ctx          = context.Background()
		cli          clusterclient.ClusterClient
		providerPath logicalcluster.Path
		consumerPath logicalcluster.Path
		consumerWS   *tenancyv1alpha1.Workspace
		cancel       context.CancelFunc
	)

	BeforeAll(func() {
		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// Provider workspace: ARS + APIExport (with the configmaps claim).
		_, providerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-provider"))
		applyFixtureFromFile(ctx, cli, providerPath, "../../config/kcp/apiresourceschema-kubernetesclusters.krop.opendefense.cloud.yaml", nil)
		applyFixtureFromFile(ctx, cli, providerPath, "../../config/kcp/apiexport-krop-m1.yaml", nil)

		// Consumer workspace + APIBinding (accepting the claim) — BEFORE awaiting
		// endpoints. kcp only advertises the vw URL once a binding exists.
		consumerWS, consumerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-consumer"))
		applyFixtureFromFile(ctx, cli, consumerPath, "../../test/fixtures/apibinding-kubernetescluster.yaml",
			map[string]string{"PROVIDER_PATH": providerPath.String()})

		// Await the endpoint slice URL (the default slice is named after the export).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			slice := &apisv1alpha1.APIExportEndpointSlice{}
			if err := cli.Cluster(providerPath).Get(ctx, client.ObjectKey{Name: m2ExportName}, slice); err != nil {
				return false, err.Error()
			}
			if len(slice.Status.APIExportEndpoints) == 0 {
				return false, "no endpoints yet"
			}
			return true, ""
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "APIExport vw endpoints not populated")

		// Wait for the consumer APIBinding to bind, then for the bound instance type
		// to be served in the consumer workspace (this also refreshes the client's
		// RESTMapper so the later instance Create resolves the kind).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			binding := &apisv1alpha2.APIBinding{}
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Name: "kubernetesclusters"}, binding); err != nil {
				return false, err.Error()
			}
			if binding.Status.Phase != apisv1alpha2.APIBindingPhaseBound {
				return false, "binding phase " + string(binding.Status.Phase)
			}
			return true, ""
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "consumer APIBinding not Bound")

		envtest.Eventually(GinkgoT(), func() (bool, string) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesClusterList"})
			if err := cli.Cluster(consumerPath).List(ctx, list); err != nil {
				return false, err.Error()
			}
			return true, ""
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "KubernetesCluster type not served in consumer workspace")

		// Build the compiled graph against the provider workspace's live discovery.
		providerConfig := rest.CopyConfig(kcpConfig)
		providerConfig.Host += providerPath.RequestPath()
		graphSource, err := kropengine.NewEndpointGraphSource(providerConfig)
		Expect(err).NotTo(HaveOccurred())
		rgd, err := kropengine.LoadExampleBlueprint()
		Expect(err).NotTo(HaveOccurred())
		compiled, err := graphSource.Build(rgd)
		Expect(err).NotTo(HaveOccurred())

		// Provider-target children go into the provider workspace directly.
		providerClient, err := client.New(providerConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		instGVK := schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesCluster"}
		reconciler := &kropctrl.Reconciler{Graph: compiled, ProviderClient: providerClient, InstanceGVK: instGVK}

		// In-process manager against the APIExport virtual workspace.
		p, err := apiexport.New(providerConfig, m2ExportName, apiexport.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		mgr, err := mcmanager.New(providerConfig, p, mcmanager.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		watch := &unstructured.Unstructured{}
		watch.SetGroupVersionKind(instGVK)
		Expect(mcbuilder.ControllerManagedBy(mgr).Named("krop-instance-e2e").For(watch).Complete(
			mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
				cl, err := mgr.GetCluster(ctx, req.ClusterName)
				if err != nil {
					return ctrl.Result{}, err
				}
				_, err = reconciler.Reconcile(ctx, cl.GetClient(), string(req.ClusterName), req.NamespacedName)
				return ctrl.Result{}, err
			}),
		)).To(Succeed())

		var runCtx context.Context
		runCtx, cancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(runCtx)).To(Succeed())
		}()
	})

	AfterAll(func() {
		if cancel != nil {
			cancel()
		}
	})

	It("materializes a consumer child (through the vw) AND a collision-named provider child", func() {
		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "krop.opendefense.cloud/v1alpha1",
			"kind":       "KubernetesCluster",
			"metadata":   map[string]interface{}{"name": "demo", "namespace": "default"},
			"spec":       map[string]interface{}{"region": "eu"},
		}}
		Expect(cli.Cluster(consumerPath).Create(ctx, instance)).To(Succeed())

		// Consumer-target ConfigMap in the consumer workspace (claim-authorized write via vw).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm); err != nil {
				return false, err.Error()
			}
			region, _, _ := unstructured.NestedString(cm.Object, "data", "region")
			return region == "eu", "consumer cm data.region=" + region
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "consumer-target ConfigMap not materialized")

		// Provider-target ConfigMap in the provider workspace, collision-free name.
		// consumerWS.Spec.Cluster is the consumer's logical-cluster name — exactly
		// what mc-runtime yields as req.ClusterName inside the reconciler, so the
		// expected provider-child name matches what the reconciler computed.
		wantProvider := kropengine.ProviderChildName(consumerWS.Spec.Cluster, "demo", "eu-provider-record")
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := cli.Cluster(providerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: wantProvider}, cm); err != nil {
				return false, err.Error()
			}
			region, _, _ := unstructured.NestedString(cm.Object, "data", "region")
			return region == "eu", "provider cm data.region=" + region
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "provider-target ConfigMap not materialized as "+wantProvider)

		// Instance status projected.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			name, _, _ := unstructured.NestedString(got.Object, "status", "configMapName")
			return name == "eu-cluster-config", "status.configMapName=" + name
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance status not projected")
	})
})
