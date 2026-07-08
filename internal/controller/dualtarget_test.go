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

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kcp-dev/sdk/apis/core"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

var _ = Describe("M3 async cross-target reconcile", Ordered, func() {
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
		applyFixtureFromFile(ctx, cli, providerPath, "../../test/fixtures/apiresourceschema-kubernetesclusters.krop.opendefense.cloud.yaml", nil)
		applyFixtureFromFile(ctx, cli, providerPath, "../../test/fixtures/apiexport-krop-m1.yaml", nil)

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

		// Install the status-bearing AgentRequest CRD in the provider workspace
		// BEFORE building the graph: kro type-checks ${agentRequest.status.token}
		// against the CRD's OpenAPI schema at graph-build time, so it must exist.
		applyFixtureFromFile(ctx, cli, providerPath, "../../test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml", nil)
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
			if err := cli.Cluster(providerPath).Get(ctx, client.ObjectKey{Name: "agentrequests.fulfil.krop.opendefense.cloud"}, crd); err != nil {
				return false, err.Error()
			}
			conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
			for _, c := range conds {
				cm, _ := c.(map[string]any)
				if cm["type"] == "Established" && cm["status"] == "True" {
					return true, ""
				}
			}

			return false, "AgentRequest CRD not Established"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "AgentRequest CRD not established")

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
		// LoadExampleBlueprint parses the embedded blueprint as a kro-native RGD,
		// which drops our per-resource target field, so supply the routing map that
		// the Registrar would derive from the wrapper spec's ToKro.
		routing := map[string]kropengine.Target{"agentRequest": kropengine.TargetProvider, "config": kropengine.TargetConsumer}
		reconciler := &kropctrl.Reconciler{Graph: compiled, ProviderClient: providerClient, InstanceGVK: instGVK, BlueprintName: m2ExportName, Routing: routing}

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
				res, err := reconciler.Reconcile(ctx, cl.GetClient(), string(req.ClusterName), req.NamespacedName)
				if err != nil {
					return ctrl.Result{}, err
				}
				if res.Requeue {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}

				return ctrl.Result{}, nil
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

	It("pends the consumer child until the provider AgentRequest status is set, then propagates it", func() {
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "KubernetesCluster",
			"metadata": map[string]any{"name": "demo", "namespace": "default"},
			"spec":     map[string]any{"region": "eu"},
		}}
		Expect(cli.Cluster(consumerPath).Create(ctx, instance)).To(Succeed())

		// The provider-target AgentRequest is created in the provider ws (collision-named).
		agentName := kropengine.ProviderChildName(consumerWS.Spec.Cluster, "demo", "eu-agent")
		agentKey := client.ObjectKey{Namespace: "default", Name: agentName}
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			ar := &unstructured.Unstructured{}
			ar.SetGroupVersionKind(schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"})
			err := cli.Cluster(providerPath).Get(ctx, agentKey, ar)

			return err == nil, "agentrequest not created"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "provider AgentRequest not created")

		// The consumer ConfigMap must NOT exist yet — it pends on the absent status.token.
		Consistently(func() bool {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm)

			return err != nil // still not found
		}, 2*time.Second, 200*time.Millisecond).Should(BeTrue(), "consumer child must pend until provider status is set")

		// C1 regression: at this point the provider AgentRequest exists but the
		// consumer child still pends on the absent status.token, so the reconcile
		// pass is INCOMPLETE (res.Complete==false). The liveness record MUST already
		// have been written during this pending window — otherwise, if the consumer
		// unbound the APIExport now, the orphan sweeper would have no record to act on
		// and the provider AgentRequest would orphan forever (idea.md §11).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			recName := kropengine.LivenessRecordName(consumerWS.Spec.Cluster, string(got.GetUID()))
			rec := &unstructured.Unstructured{}
			rec.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := cli.Cluster(providerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: recName}, rec); err != nil {
				return false, "liveness record absent during pending window: " + err.Error()
			}

			return rec.GetLabels()[kropengine.LabelLiveness] == "true", "liveness record present but missing liveness label"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "liveness record not written during pending (pre-Complete) pass")

		// Simulate the downstream fulfilment controller: patch the AgentRequest status.
		ar := &unstructured.Unstructured{}
		ar.SetGroupVersionKind(schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"})
		Expect(cli.Cluster(providerPath).Get(ctx, agentKey, ar)).To(Succeed())
		Expect(unstructured.SetNestedField(ar.Object, "tok-xyz789", "status", "token")).To(Succeed())
		Expect(cli.Cluster(providerPath).Status().Update(ctx, ar)).To(Succeed())

		// Now (on a requeue reconcile) the consumer ConfigMap appears with the propagated token.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(cm.Object, "data", "token")

			return tok == "tok-xyz789", "consumer cm data.token=" + tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "cross-target token did not propagate to the consumer child")

		// Instance status maps agentToken from the provider child status.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(got.Object, "status", "agentToken")

			return tok == "tok-xyz789", "status.agentToken=" + tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance status.agentToken not mapped")

		// I2: the reconciler projects the engine Result as a Ready condition. This
		// persists only if status.conditions is in the served schema (kcp prunes
		// otherwise — the M3 agentToken bug). Asserting it here proves the served
		// ARS carries conditions end-to-end, not just the in-memory set.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
			for _, c := range conds {
				if cm, ok := c.(map[string]any); ok && cm["type"] == "Ready" {
					return true, ""
				}
			}

			return false, "no Ready condition on instance status"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance Ready condition not persisted (served schema pruned status.conditions?)")
	})
})
