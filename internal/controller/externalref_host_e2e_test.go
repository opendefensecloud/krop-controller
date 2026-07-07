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

// M4 external-ref + host-target e2e — proves, against real kcp, the two branches
// added by this milestone:
//
//   A. externalRef read → observe → funnel: the engine Gets an existing Vpc it did
//      not create (routed to the consumer plane), reads ${vpc.status.vpcId}, and
//      funnels it via CEL into a materialized consumer ConfigMap + the instance
//      status. The Vpc is never labeled/owned/pruned and survives instance delete.
//
//   B. host target write + prune: a `target: host` child is written to a SEPARATE
//      host-cluster client with a collision-free ProviderChildName and GC labels,
//      then pruned when includeWhen drops it from the desired set.
//
// HARNESS CHOICE (direct-reconcile, not the vw manager): unlike dualtarget_test.go
// — which drives the SHARED KubernetesCluster blueprint through the APIExport
// virtual workspace — these specs each need a DISTINCT instance type + blueprint
// graph (externalRef / host nodes), which cannot be grafted onto the shared
// embedded blueprint without breaking that suite. Standing up a second full vw
// (APIExport + ARS + APIBinding + endpoint slice + read-only claims) per spec is
// disproportionate, so we drive Reconciler.Reconcile directly with real
// workspace-scoped clients. This still exercises the real compiled graph, the real
// engine external-read/host-apply branches, and real kcp GC/prune — only the vw
// identity/claim plumbing (unit-tested in Task 4, e2e-proven in dualtarget) is out
// of frame. Driving reconcile directly also lets Spec A read a CONSUMER external
// ref faithfully with no permissionClaim (a direct client has full workspace
// access), matching the design's preferred consumer-external-ref case.
//
// HOST STAND-IN (separate workspace): Spec B points Reconciler.HostClient at a
// dedicated `krop-host` workspace (distinct from the provider workspace holding
// liveness records), so an assertion that the child lands in the host client — and
// NOT the provider — actually distinguishes the two write planes.

import (
	"context"
	"os"
	"time"

	"github.com/kcp-dev/logicalcluster/v3"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	"github.com/kcp-dev/sdk/apis/core"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// workspaceClient builds a workspace-scoped controller-runtime client for path.
func workspaceClient(path logicalcluster.Path) client.Client {
	GinkgoHelper()
	cfg := rest.CopyConfig(kcpConfig)
	cfg.Host += path.RequestPath()
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	return c
}

// workspaceConfig returns a rest.Config scoped to path (for graph-building
// discovery against that workspace's served types).
func workspaceConfig(path logicalcluster.Path) *rest.Config {
	cfg := rest.CopyConfig(kcpConfig)
	cfg.Host += path.RequestPath()

	return cfg
}

// awaitCRDEstablished blocks until the named CRD reports Established=True in path.
func awaitCRDEstablished(ctx context.Context, cli clusterclient.ClusterClient, path logicalcluster.Path, name string) {
	GinkgoHelper()
	envtest.Eventually(GinkgoT(), func() (bool, string) {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		if err := cli.Cluster(path).Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
			return false, err.Error()
		}
		conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
		for _, c := range conds {
			cm, _ := c.(map[string]any)
			if cm["type"] == "Established" && cm["status"] == "True" {
				return true, ""
			}
		}

		return false, "CRD " + name + " not Established"
	}, wait.ForeverTestTimeout, 200*time.Millisecond, "CRD "+name+" not established")
}

// loadBlueprint parses a wrapper ResourceGraphDefinition fixture and returns the
// kro-native RGD (for the graph builder) plus the routing map — mirroring exactly
// what the Registrar derives from the wrapper spec's ToKro at publish time.
func loadBlueprint(file string) (*krov1alpha1.ResourceGraphDefinition, map[string]kropengine.Target) {
	GinkgoHelper()
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred())
	bp := &kropv1alpha1.ResourceGraphDefinition{}
	Expect(sigsyaml.Unmarshal(raw, bp)).To(Succeed())
	kroSpec, routingRaw := bp.Spec.ToKro()
	routing := make(map[string]kropengine.Target, len(routingRaw))
	for id, rawT := range routingRaw {
		t, terr := kropengine.ParseTarget(rawT)
		Expect(terr).NotTo(HaveOccurred())
		routing[id] = t
	}

	return &krov1alpha1.ResourceGraphDefinition{Spec: kroSpec}, routing
}

// buildGraphRaw compiles rgd against a workspace's live discovery, retrying to
// absorb discovery-cache lag right after a CRD becomes Established.
func buildGraphRaw(cfg *rest.Config, rgd *krov1alpha1.ResourceGraphDefinition) *krograph.Graph {
	GinkgoHelper()
	var g *krograph.Graph
	envtest.Eventually(GinkgoT(), func() (bool, string) {
		gs, err := kropengine.NewEndpointGraphSource(cfg)
		if err != nil {
			return false, err.Error()
		}
		built, err := gs.Build(rgd)
		if err != nil {
			return false, err.Error()
		}
		g = built

		return true, ""
	}, wait.ForeverTestTimeout, 500*time.Millisecond, "blueprint graph did not compile against workspace discovery")

	return g
}

var _ = Describe("M4 externalRef read + funnel", Ordered, func() {
	var (
		ctx          = context.Background()
		cli          clusterclient.ClusterClient
		consumerWS   *tenancyv1alpha1.Workspace
		consumerPath logicalcluster.Path
		providerPath logicalcluster.Path
		reconciler   *kropctrl.Reconciler
		instGVK      = schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "ExternalApp"}
		vpcGVK       = schema.GroupVersionKind{Group: "example.krop.opendefense.cloud", Version: "v1alpha1", Kind: "Vpc"}
		key          = client.ObjectKey{Namespace: "default", Name: "app-a"}
	)

	BeforeAll(func() {
		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// Consumer workspace holds the instance, the external Vpc, and the child.
		consumerWS, consumerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-consumer"))
		// Provider workspace holds only the liveness record here (no provider child).
		_, providerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-provider"))

		// Install the Vpc CRD (the externalRef type) and the ExternalApp instance
		// CRD in the consumer workspace; both must be Established before the graph
		// build type-checks ${vpc.status.vpcId} and before instances are created.
		applyFixtureFromFile(ctx, cli, consumerPath, "../../test/fixtures/crd-vpcs.example.krop.opendefense.cloud.yaml", nil)
		applyFixtureFromFile(ctx, cli, consumerPath, "../../test/fixtures/crd-instance-generic.yaml", map[string]string{
			"GROUP": "krop.opendefense.cloud", "PLURAL": "externalapps", "SINGULAR": "externalapp",
			"KIND": "ExternalApp", "LISTKIND": "ExternalAppList",
		})
		awaitCRDEstablished(ctx, cli, consumerPath, "vpcs.example.krop.opendefense.cloud")
		awaitCRDEstablished(ctx, cli, consumerPath, "externalapps.krop.opendefense.cloud")

		rgd, routing := loadBlueprint("../../test/fixtures/blueprint-externalref-rgd.yaml")
		compiled := buildGraphRaw(workspaceConfig(consumerPath), rgd)

		reconciler = &kropctrl.Reconciler{
			Graph:          compiled,
			ProviderClient: workspaceClient(providerPath),
			InstanceGVK:    instGVK,
			BlueprintName:  "externalapp",
			Routing:        routing,
		}
	})

	It("funnels an external Vpc's status into a materialized consumer child and never GCs the Vpc", func() {
		consumerClient := workspaceClient(consumerPath)
		clusterName := consumerWS.Spec.Cluster

		// Pre-create the external Vpc and set its status.vpcId (status subresource).
		vpc := &unstructured.Unstructured{}
		vpc.SetGroupVersionKind(vpcGVK)
		vpc.SetNamespace("default")
		vpc.SetName("app-a")
		Expect(unstructured.SetNestedField(vpc.Object, "10.0.0.0/16", "spec", "cidr")).To(Succeed())
		Expect(consumerClient.Create(ctx, vpc)).To(Succeed())
		Expect(unstructured.SetNestedField(vpc.Object, "vpc-xyz789", "status", "vpcId")).To(Succeed())
		Expect(consumerClient.Status().Update(ctx, vpc)).To(Succeed())

		// Create the instance; spec.vpcName selects the external Vpc by name.
		inst := &unstructured.Unstructured{}
		inst.SetGroupVersionKind(instGVK)
		inst.SetNamespace("default")
		inst.SetName("app-a")
		Expect(unstructured.SetNestedField(inst.Object, "app-a", "spec", "vpcName")).To(Succeed())
		Expect(consumerClient.Create(ctx, inst)).To(Succeed())

		// Drive reconcile directly (finalizer pass, then apply passes) until the
		// consumer child materializes with the funneled vpcId.
		reconcileOnce := func() {
			_, err := reconciler.Reconcile(ctx, consumerClient, clusterName, key)
			Expect(err).NotTo(HaveOccurred())
		}
		Eventually(func() string {
			reconcileOnce()
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := consumerClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "app-a-config"}, cm); err != nil {
				return "child not created: " + err.Error()
			}
			v, _, _ := unstructured.NestedString(cm.Object, "data", "vpcId")

			return v
		}, 30*time.Second, 250*time.Millisecond).Should(Equal("vpc-xyz789"), "external Vpc status did not funnel into the consumer child")

		// The CEL-projected instance status also carries the funneled value.
		Eventually(func() string {
			reconcileOnce()
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instGVK)
			if err := consumerClient.Get(ctx, key, got); err != nil {
				return err.Error()
			}
			v, _, _ := unstructured.NestedString(got.Object, "status", "vpcId")

			return v
		}, 30*time.Second, 250*time.Millisecond).Should(Equal("vpc-xyz789"), "external Vpc status did not funnel into the instance status")

		// The external Vpc must NOT carry GC labels (it is read, never owned).
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(vpcGVK)
		Expect(consumerClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "app-a"}, got)).To(Succeed())
		Expect(got.GetLabels()).NotTo(HaveKey(kropengine.LabelInstanceUID))

		// Delete the instance and drive the deletion (GC) pass to completion.
		Expect(consumerClient.Delete(ctx, inst)).To(Succeed())
		Eventually(func() bool {
			reconcileOnce()
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := consumerClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "app-a-config"}, cm)

			return err != nil // child GC'd
		}, 30*time.Second, 250*time.Millisecond).Should(BeTrue(), "consumer child was not GC'd on instance delete")

		// External refs are NEVER GC'd: the Vpc must survive the instance deletion.
		Expect(consumerClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "app-a"}, got)).To(Succeed())
		vpcID, _, _ := unstructured.NestedString(got.Object, "status", "vpcId")
		Expect(vpcID).To(Equal("vpc-xyz789"), "external Vpc was mutated/deleted by GC")
	})
})

var _ = Describe("M4 host target write + prune", Ordered, func() {
	var (
		ctx          = context.Background()
		cli          clusterclient.ClusterClient
		consumerWS   *tenancyv1alpha1.Workspace
		consumerPath logicalcluster.Path
		providerPath logicalcluster.Path
		hostPath     logicalcluster.Path
		hostClient   client.Client
		reconciler   *kropctrl.Reconciler
		instGVK      = schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "HostApp"}
		cmGVK        = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		key          = client.ObjectKey{Namespace: "default", Name: "host-a"}
	)

	BeforeAll(func() {
		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		consumerWS, consumerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-consumer"))
		_, providerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-provider"))
		// A SEPARATE host workspace stands in for the physical host cluster, so a
		// child landing here is distinguishable from a provider-plane write.
		_, hostPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-host"))

		applyFixtureFromFile(ctx, cli, consumerPath, "../../test/fixtures/crd-instance-generic.yaml", map[string]string{
			"GROUP": "krop.opendefense.cloud", "PLURAL": "hostapps", "SINGULAR": "hostapp",
			"KIND": "HostApp", "LISTKIND": "HostAppList",
		})
		awaitCRDEstablished(ctx, cli, consumerPath, "hostapps.krop.opendefense.cloud")

		// The host child is a core ConfigMap; the graph builds against the host
		// workspace discovery (ConfigMap is core there too).
		rgd, routing := loadBlueprint("../../test/fixtures/blueprint-hosttarget-rgd.yaml")
		compiled := buildGraphRaw(workspaceConfig(hostPath), rgd)

		hostClient = workspaceClient(hostPath)
		reconciler = &kropctrl.Reconciler{
			Graph:          compiled,
			ProviderClient: workspaceClient(providerPath),
			HostClient:     hostClient,
			InstanceGVK:    instGVK,
			BlueprintName:  "hostapp",
			Routing:        routing,
		}
	})

	It("writes a host-target child with a collision-free name + GC labels, then prunes it", func() {
		consumerClient := workspaceClient(consumerPath)
		clusterName := consumerWS.Spec.Cluster

		inst := &unstructured.Unstructured{}
		inst.SetGroupVersionKind(instGVK)
		inst.SetNamespace("default")
		inst.SetName("host-a")
		Expect(unstructured.SetNestedField(inst.Object, "host-a", "spec", "name")).To(Succeed())
		Expect(unstructured.SetNestedField(inst.Object, true, "spec", "enabled")).To(Succeed())
		Expect(consumerClient.Create(ctx, inst)).To(Succeed())

		// The instance UID stamps the GC labels; read it back after creation.
		created := &unstructured.Unstructured{}
		created.SetGroupVersionKind(instGVK)
		Expect(consumerClient.Get(ctx, key, created)).To(Succeed())
		instUID := string(created.GetUID())

		reconcileOnce := func() {
			_, err := reconciler.Reconcile(ctx, consumerClient, clusterName, key)
			Expect(err).NotTo(HaveOccurred())
		}

		// The host child is renamed via ProviderChildName (collision-free) and
		// written to the HOST client — not the provider or consumer.
		hostChildName := kropengine.ProviderChildName(clusterName, "host-a", "host-a-hostcm")
		Eventually(func() string {
			reconcileOnce()
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(cmGVK)
			if err := hostClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: hostChildName}, cm); err != nil {
				return "host child not created: " + err.Error()
			}

			return cm.GetLabels()[kropengine.LabelInstanceUID]
		}, 30*time.Second, 250*time.Millisecond).Should(Equal(instUID), "host child missing or lacks instance-uid GC label")

		// Full GC-label + blueprint-label assertions on the created host child.
		hostCM := &unstructured.Unstructured{}
		hostCM.SetGroupVersionKind(cmGVK)
		Expect(hostClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: hostChildName}, hostCM)).To(Succeed())
		Expect(hostCM.GetLabels()).To(HaveKeyWithValue(kropengine.LabelConsumerCluster, clusterName))
		Expect(hostCM.GetLabels()).To(HaveKeyWithValue(kropengine.LabelBlueprint, "hostapp"))

		// The child must NOT exist under its ORIGINAL name (naming was qualified).
		orig := &unstructured.Unstructured{}
		orig.SetGroupVersionKind(cmGVK)
		Expect(hostClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "host-a-hostcm"}, orig)).NotTo(Succeed())

		// Toggle the child out of the desired set (includeWhen ${spec.enabled}) and
		// re-reconcile: a complete pass with no host child desired prunes it.
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(instGVK)
		Expect(consumerClient.Get(ctx, key, live)).To(Succeed())
		Expect(unstructured.SetNestedField(live.Object, false, "spec", "enabled")).To(Succeed())
		Expect(consumerClient.Update(ctx, live)).To(Succeed())

		Eventually(func() bool {
			reconcileOnce()
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(cmGVK)
			err := hostClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: hostChildName}, cm)

			return err != nil // pruned
		}, 30*time.Second, 250*time.Millisecond).Should(BeTrue(), "host child was not pruned after leaving the desired set")
	})
})
