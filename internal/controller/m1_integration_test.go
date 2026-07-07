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

// M1 walking-skeleton e2e — direct-reconcile design.
//
// WHY NOT THE VIRTUAL-WORKSPACE FAN-IN HERE:
//
// The production controller (cmd/controller/main.go) runs a multicluster manager
// against an APIExport *virtual workspace*, driven by the APIExportEndpointSlice
// the mc-provider apiexport.Provider watches. That wiring is compile-verified in
// main.go and is the intended production path.
//
// Under our pinned stack — multicluster-provider v0.8.0 + kcp 0.30.0 booted via
// the provider's envtest harness — kcp's `apiexportendpointslice-urls` controller
// never populates `APIExportEndpointSlice.status.endpoints`: the slice reaches
// APIExportValid=True / PartitionValid=True but zero endpoints (empirically
// confirmed: no endpoints after >100s, while the -urls controller loops on
// `apiexports.apis.kcp.io "root:topology.kcp.io" not found` for the system
// exports and cannot resolve the shard virtual-workspace URL in this envtest).
// Without endpoints the virtual workspace never comes up, so an in-suite manager
// against the vw cannot engage any cluster. This is an envtest/mc-provider
// limitation, not a krop-engine bug.
//
// So this suite proves everything M1 needs WITHOUT the vw fan-in, mirroring the
// proven direct-reconcile pattern used by the sibling access-operator e2e on the
// identical stack: it installs the instance type into a single real kcp
// workspace, builds the kro graph against that workspace's live discovery/OpenAPI
// (design §16.3), then drives the exact reconcile logic from main.go's closure
// once, directly, against a workspace-scoped client. The vw fan-in stays wired in
// main.go and is exercised end-to-end in a later, cluster-backed environment.

import (
	"context"
	"time"

	"github.com/kcp-dev/logicalcluster/v3"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	"github.com/kcp-dev/sdk/apis/core"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// instanceGVK is the hand-published M1 instance kind.
var instanceGVK = schema.GroupVersionKind{
	Group:   "krop.opendefense.cloud",
	Version: "v1alpha1",
	Kind:    "KubernetesCluster",
}

// instanceCRD returns the KubernetesCluster CRD (namespaced, with a status
// subresource) so the type is served — and its status writable — in a single
// workspace. It mirrors config/kcp/apiresourceschema-*.yaml, which is the
// production shape published via the APIExport; installing the plain CRD directly
// is the isolated-workspace equivalent (see access-operator's installAccessCRDs).
func instanceCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubernetesclusters.krop.opendefense.cloud",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "krop.opendefense.cloud",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "KubernetesCluster",
				ListKind: "KubernetesClusterList",
				Plural:   "kubernetesclusters",
				Singular: "kubernetescluster",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"region": {Type: "string"},
								},
								Required: []string{"region"},
							},
							"status": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"configMapName": {Type: "string"},
								},
								// Status is projected as an opaque map; tolerate extra keys.
								XPreserveUnknownFields: &preserve,
							},
						},
					},
				},
			}},
		},
	}
}

// established reports whether the CRD has reached the Established condition.
func established(crd *apiextensionsv1.CustomResourceDefinition) bool {
	for _, c := range crd.Status.Conditions {
		if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}
	return false
}

var _ = Describe("M1 instance reconcile (direct, against real kcp)", Ordered, func() {
	var (
		ctx      context.Context
		wsPath   logicalcluster.Path
		wc       client.Client // workspace-scoped client the reconciler operates with
		compiled *krograph.Graph
	)

	BeforeAll(func() {
		// A suite-lived context: the SpecContext handed to BeforeAll is canceled
		// once the node returns, which would break the later It block.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)

		cli, err := clusterclient.New(kcpConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// A fresh universal workspace standing in for one tenant workspace. Both
		// the instance and its materialized ConfigMap live here (M1 routes every
		// child to the consumer target).
		_, wsPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(),
			envtest.WithNamePrefix("krop"))
		wc = cli.Cluster(wsPath)

		// Serve the instance type in the workspace and wait until it's Established.
		crd := instanceCRD()
		Expect(client.IgnoreAlreadyExists(wc.Create(ctx, crd))).To(Succeed())
		Eventually(func(g Gomega) {
			got := &apiextensionsv1.CustomResourceDefinition{}
			g.Expect(wc.Get(ctx, client.ObjectKey{Name: crd.Name}, got)).To(Succeed())
			g.Expect(established(got)).To(BeTrue(), "instance CRD not Established")
		}).WithTimeout(60 * time.Second).WithPolling(time.Second).Should(Succeed())

		// Ensure the target namespace exists for the instance + its ConfigMap.
		Expect(client.IgnoreAlreadyExists(wc.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
		}))).To(Succeed())

		// §16.3 validation: build the kro graph via graph.NewBuilder against the
		// REAL workspace discovery/OpenAPI endpoint (NewCombinedResolver over live
		// kcp discovery), not a fake resolver. This is the residual §16.3 risk the
		// M0 findings flagged.
		wsRestConfig := rest.CopyConfig(kcpConfig)
		wsRestConfig.Host += wsPath.RequestPath()
		graphSource, err := kropengine.NewEndpointGraphSource(wsRestConfig)
		Expect(err).NotTo(HaveOccurred())
		rgd, err := kropengine.LoadExampleBlueprint()
		Expect(err).NotTo(HaveOccurred())
		compiled, err = graphSource.Build(rgd)
		Expect(err).NotTo(HaveOccurred(), "graph.NewBuilder against live kcp discovery (§16.3)")
	})

	It("materializes the consumer-target ConfigMap and projects status", func() {
		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "krop.opendefense.cloud/v1alpha1",
			"kind":       "KubernetesCluster",
			"metadata":   map[string]interface{}{"name": "demo", "namespace": "default"},
			"spec":       map[string]interface{}{"region": "eu"},
		}}
		Expect(wc.Create(ctx, instance)).To(Succeed())

		// Drive the exact reconcile logic from cmd/controller/main.go's closure
		// once, directly, against the workspace-scoped client (which stands in for
		// the per-cluster client the multicluster manager would hand the closure).
		reconcileOnce(ctx, wc, compiled)

		// The reconcile must have materialized eu-cluster-config (data.region=eu)
		// in the workspace.
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
		Expect(wc.Get(ctx,
			client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm)).To(Succeed())
		region, _, _ := unstructured.NestedString(cm.Object, "data", "region")
		Expect(region).To(Equal("eu"), "consumer-target ConfigMap data.region")

		// And projected status.configMapName back onto the instance.
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(instanceGVK)
		Expect(wc.Get(ctx,
			client.ObjectKey{Namespace: "default", Name: "demo"}, got)).To(Succeed())
		name, _, _ := unstructured.NestedString(got.Object, "status", "configMapName")
		Expect(name).To(Equal("eu-cluster-config"), "projected status.configMapName")
	})
})

// reconcileOnce runs a single pass of the M1 reconcile logic — identical to the
// closure in cmd/controller/main.go, minus the per-cluster client lookup (the
// caller supplies the workspace-scoped client directly). Keeping it in lockstep
// with main.go is what lets this suite retire §16.3 for the real engine path.
func reconcileOnce(ctx context.Context, consumer client.Client, compiled *krograph.Graph) {
	GinkgoHelper()

	inst := &unstructured.Unstructured{}
	inst.SetGroupVersionKind(instanceGVK)
	Expect(consumer.Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, inst)).To(Succeed())

	rt, err := kroruntime.FromGraph(compiled, inst, krograph.RGDConfig{
		MaxCollectionSize:          1000,
		MaxCollectionDimensionSize: 1000,
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = kropengine.New().Reconcile(ctx, rt, map[kropengine.Target]kropengine.Applier{
		kropengine.TargetConsumer: kropengine.NewSSAApplier(consumer),
	})
	Expect(err).NotTo(HaveOccurred())

	di, err := kropengine.ProjectStatus(rt)
	Expect(err).NotTo(HaveOccurred())
	status, found, err := unstructured.NestedMap(di.Object, "status")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue(), "blueprint status not resolved")
	Expect(unstructured.SetNestedMap(inst.Object, status, "status")).To(Succeed())
	Expect(consumer.Status().Update(ctx, inst)).To(Succeed())
}
