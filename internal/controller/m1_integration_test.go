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

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kcp-dev/sdk/apis/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// apiExportName is the M1 APIExport name; kcp auto-creates a default
// APIExportEndpointSlice of the same name for it.
const apiExportName = "kubernetesclusters.krop.opendefense.cloud"

// instanceGVK is the hand-published M1 instance kind.
var instanceGVK = schema.GroupVersionKind{
	Group:   "krop.opendefense.cloud",
	Version: "v1alpha1",
	Kind:    "KubernetesCluster",
}

// loadFixture reads a YAML fixture, substitutes ${VAR} placeholders, and
// unmarshals it into the correct typed object (so kcp validates it on create).
func loadFixture(path string, replacements map[string]string) client.Object {
	raw, err := os.ReadFile(path)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "reading fixture %s", path)

	content := string(raw)
	for k, v := range replacements {
		content = strings.ReplaceAll(content, "${"+k+"}", v)
	}
	data := []byte(content)

	var meta metav1.TypeMeta
	ExpectWithOffset(1, yaml.Unmarshal(data, &meta)).To(Succeed(), "parsing kind from %s", path)

	switch meta.Kind {
	case "APIResourceSchema":
		o := &apisv1alpha1.APIResourceSchema{}
		ExpectWithOffset(1, yaml.Unmarshal(data, o)).To(Succeed(), "unmarshaling %s", path)
		return o
	case "APIExport":
		o := &apisv1alpha2.APIExport{}
		ExpectWithOffset(1, yaml.Unmarshal(data, o)).To(Succeed(), "unmarshaling %s", path)
		return o
	case "APIBinding":
		o := &apisv1alpha2.APIBinding{}
		ExpectWithOffset(1, yaml.Unmarshal(data, o)).To(Succeed(), "unmarshaling %s", path)
		return o
	default:
		o := &unstructured.Unstructured{}
		ExpectWithOffset(1, yaml.Unmarshal(data, &o.Object)).To(Succeed(), "unmarshaling %s", path)
		return o
	}
}

// applyFixture loads a YAML fixture with ${VAR} substitution and creates it in
// the given workspace.
func applyFixture(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, path string, replacements map[string]string) {
	obj := loadFixture(path, replacements)
	ExpectWithOffset(1, cli.Cluster(wsPath).Create(ctx, obj)).To(Succeed(), "creating fixture %s in %s", path, wsPath)
}

var _ = Describe("M1 instance reconcile", Ordered, func() {
	var (
		ctx          context.Context
		cancel       context.CancelFunc
		cli          clusterclient.ClusterClient
		providerPath logicalcluster.Path
		consumerPath logicalcluster.Path
	)

	BeforeAll(func() {
		ctx, cancel = context.WithCancel(context.Background())

		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// --- Provider workspace: publish the ARS + APIExport. ---
		_, providerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(),
			envtest.WithNamePrefix("krop-provider"))
		applyFixture(ctx, cli, providerPath,
			"../../config/kcp/apiresourceschema-kubernetesclusters.krop.opendefense.cloud.yaml", nil)
		applyFixture(ctx, cli, providerPath,
			"../../config/kcp/apiexport-krop-m1.yaml", nil)

		// Wait until the APIExport's endpoint slice has endpoints (virtual ws up).
		// kcp auto-creates a default APIExportEndpointSlice named after the export;
		// its status.endpoints are populated by kcp's apiexportendpointslice-urls
		// controller once it resolves shard virtual-workspace URLs.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			slice := &apisv1alpha1.APIExportEndpointSlice{}
			if err := cli.Cluster(providerPath).Get(ctx, client.ObjectKey{Name: apiExportName}, slice); err != nil {
				return false, fmt.Sprintf("get endpoint slice: %v", err)
			}
			if len(slice.Status.APIExportEndpoints) > 0 {
				return true, ""
			}
			sb, _ := yaml.Marshal(slice.Status)
			return false, fmt.Sprintf("endpoint slice has no endpoints yet; status:\n%s", sb)
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "APIExport virtual workspace not ready")

		// --- Start the controller in-process against the APIExport virtual ws. ---
		exportCfg := rest.CopyConfig(kcpConfig)
		exportCfg.Host += providerPath.RequestPath()

		// §16.3 validation: build the graph via graph.NewBuilder against the REAL
		// provider-workspace discovery/OpenAPI endpoint (not a fake resolver).
		graphSource, err := kropengine.NewEndpointGraphSource(exportCfg)
		Expect(err).NotTo(HaveOccurred())
		rgd, err := kropengine.LoadExampleBlueprint()
		Expect(err).NotTo(HaveOccurred())
		compiled, err := graphSource.Build(rgd)
		Expect(err).NotTo(HaveOccurred(), "graph.NewBuilder against live kcp (§16.3)")

		provider, err := apiexport.New(exportCfg, apiExportName, apiexport.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		mgr, err := mcmanager.New(exportCfg, provider, manager.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		eng := kropengine.New()
		reconciler := mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			cl, err := mgr.GetCluster(ctx, req.ClusterName)
			if err != nil {
				return ctrl.Result{}, err
			}
			consumer := cl.GetClient()

			inst := &unstructured.Unstructured{}
			inst.SetGroupVersionKind(instanceGVK)
			if err := consumer.Get(ctx, req.NamespacedName, inst); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}

			rt, err := kroruntime.FromGraph(compiled, inst, krograph.RGDConfig{
				MaxCollectionSize:          1000,
				MaxCollectionDimensionSize: 1000,
			})
			if err != nil {
				return ctrl.Result{}, err
			}

			res, err := eng.Reconcile(ctx, rt, map[kropengine.Target]kropengine.Applier{
				kropengine.TargetConsumer: kropengine.NewSSAApplier(consumer),
			})
			if err != nil {
				return ctrl.Result{}, err
			}

			if di, perr := kropengine.ProjectStatus(rt); perr == nil {
				if status, found, _ := unstructured.NestedMap(di.Object, "status"); found {
					_ = unstructured.SetNestedMap(inst.Object, status, "status")
					_ = consumer.Status().Update(ctx, inst)
				}
			}
			return ctrl.Result{Requeue: res.Requeue}, nil
		})

		watch := &unstructured.Unstructured{}
		watch.SetGroupVersionKind(instanceGVK)
		Expect(mcbuilder.ControllerManagedBy(mgr).
			Named("krop-instance").
			For(watch).
			Complete(reconciler)).To(Succeed())

		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(ctx)).To(Succeed())
		}()

		// --- Consumer workspace: bind the export. ---
		_, consumerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(),
			envtest.WithNamePrefix("krop-consumer"))
		applyFixture(ctx, cli, consumerPath,
			"../../test/fixtures/apibinding-kubernetescluster.yaml",
			map[string]string{"PROVIDER_PATH": providerPath.String()})

		// Wait for the binding to reach Bound.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			b := &apisv1alpha2.APIBinding{}
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Name: "kubernetesclusters"}, b); err != nil {
				return false, fmt.Sprintf("get binding: %v", err)
			}
			return b.Status.Phase == apisv1alpha2.APIBindingPhaseBound, fmt.Sprintf("phase: %s", b.Status.Phase)
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "APIBinding not bound")

		// Wait until the bound instance kind is List-able in the consumer ws.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(schema.GroupVersionKind{
				Group: instanceGVK.Group, Version: instanceGVK.Version, Kind: instanceGVK.Kind + "List",
			})
			if err := cli.Cluster(consumerPath).List(ctx, list); err != nil {
				return false, err.Error()
			}
			return true, ""
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "bound KubernetesCluster kind not served yet")
	})

	AfterAll(func() {
		if cancel != nil {
			cancel()
		}
	})

	It("materializes the consumer-target ConfigMap and projects status", func() {
		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "krop.opendefense.cloud/v1alpha1",
			"kind":       "KubernetesCluster",
			"metadata":   map[string]interface{}{"name": "demo", "namespace": "default"},
			"spec":       map[string]interface{}{"region": "eu"},
		}}
		Expect(cli.Cluster(consumerPath).Create(ctx, instance)).To(Succeed())

		// The reconciler should create eu-cluster-config in the consumer ws.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := cli.Cluster(consumerPath).Get(ctx,
				client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm); err != nil {
				return false, err.Error()
			}
			region, _, _ := unstructured.NestedString(cm.Object, "data", "region")
			return region == "eu", "configmap data.region=" + region
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "consumer-target ConfigMap not materialized")

		// And project status.configMapName back onto the instance.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instanceGVK)
			if err := cli.Cluster(consumerPath).Get(ctx,
				client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			name, _, _ := unstructured.NestedString(got.Object, "status", "configMapName")
			return name == "eu-cluster-config", "status.configMapName=" + name
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance status not projected")
	})
})
