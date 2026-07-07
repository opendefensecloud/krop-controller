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

package registrar_test

// M4b full dynamic auto-publication e2e — the M4 capstone.
//
// This proves the ENTIRE dynamic path with NO hand-written APIExport/ARS/claim
// fixtures: a provider authors a blueprint → the Registrar auto-publishes an
// APIExport (+ ARS + auto-derived permissionClaims) → the Supervisor auto-starts
// an instance-serving manager → a consumer binds the auto-published export
// (accepting the auto-derived configmaps claim) → creating an instance
// materializes the dual-target + cross-target children.
//
// The wiring lives in dynamicHarness (dynamic_harness_test.go), which MIRRORS
// cmd/controller/main.go. The only difference from production is that the Registrar
// is direct-reconciled once (instead of run under a full controller-runtime
// manager) — but that direct call drives the real OnPublished → supervisor.Ensure
// → startFn chain, so the instance manager is auto-started by the Supervisor.

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kcp-dev/logicalcluster/v3"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const bindingPath = "../../test/fixtures/apibinding-kubernetescluster.yaml"

// servedGraph is what the Registrar records on publish and the per-export manager
// needs to serve: the compiled graph and the generated instance kind. It mirrors
// cmd/controller/main.go's servedBlueprint.
type servedGraph struct {
	graph   *krograph.Graph
	gvk     schema.GroupVersionKind
	routing map[string]kropengine.Target
}

// graphRegistry is a thread-safe registry keyed by export name (mirrors main.go's
// published): OnPublished writes it, the supervisor's startFn reads it.
type graphRegistry struct {
	mu sync.Mutex
	m  map[string]servedGraph
}

func (g *graphRegistry) Set(export string, sg servedGraph) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.m[export] = sg
}

func (g *graphRegistry) Get(export string) (servedGraph, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	sg, ok := g.m[export]

	return sg, ok
}

// applyFileSubst reads a YAML file, substitutes ${KEY} placeholders from vars, and
// creates the resulting object in the given workspace (the APIBinding needs the
// provider path substituted, which the plain applyFile helper does not do).
func applyFileSubst(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, file string, vars map[string]string) {
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

// kubernetesClusterHarnessOptions is the shared harness config for the
// KubernetesCluster blueprint (capstone, orphan-sweep, spec-change specs).
func kubernetesClusterHarnessOptions() harnessOptions {
	return harnessOptions{
		blueprintFile: blueprintPath,
		blueprintObj:  "kubernetescluster",
		exportName:    expectedExportName,
		instanceGVK:   schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesCluster"},
		instanceList:  schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesClusterList"},
		bindingFile:   bindingPath,
		bindingName:   "kubernetesclusters",
	}
}

var _ = Describe("M4b full dynamic auto-publication", Ordered, func() {
	var (
		ctx = context.Background()
		h   *dynamicHarness
	)

	BeforeAll(func() {
		h = startDynamicHarness(ctx, kubernetesClusterHarnessOptions())
	})

	AfterAll(func() {
		if h != nil {
			h.cancel()
		}
	})

	It("materializes the cross-target children through the auto-published export", func() {
		// Create the instance in the consumer workspace.
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "KubernetesCluster",
			"metadata": map[string]any{"name": "demo", "namespace": "default"},
			"spec":     map[string]any{"region": "eu"},
		}}
		Expect(h.cli.Cluster(h.consumerPath).Create(ctx, instance)).To(Succeed())

		// The provider-target AgentRequest is created in the provider ws (collision-named).
		agentName := kropengine.ProviderChildName(h.consumerWS.Spec.Cluster, "demo", "eu-agent")
		agentKey := client.ObjectKey{Namespace: "default", Name: agentName}
		agentGVK := schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"}
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			ar := &unstructured.Unstructured{}
			ar.SetGroupVersionKind(agentGVK)
			err := h.cli.Cluster(h.providerPath).Get(ctx, agentKey, ar)

			return err == nil, "agentrequest not created yet"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "provider AgentRequest not created (instance manager not serving the consumer cluster?)")

		// The consumer ConfigMap must PEND until the AgentRequest status.token is set.
		Consistently(func() bool {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := h.cli.Cluster(h.consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm)

			return err != nil
		}, 2*time.Second, 200*time.Millisecond).Should(BeTrue(), "consumer child must pend until the provider status is set")

		// Simulate the downstream fulfilment controller: patch the AgentRequest status.
		ar := &unstructured.Unstructured{}
		ar.SetGroupVersionKind(agentGVK)
		Expect(h.cli.Cluster(h.providerPath).Get(ctx, agentKey, ar)).To(Succeed())
		Expect(unstructured.SetNestedField(ar.Object, "tok-xyz789", "status", "token")).To(Succeed())
		Expect(h.cli.Cluster(h.providerPath).Status().Update(ctx, ar)).To(Succeed())

		// The consumer ConfigMap now appears with the propagated token — written
		// THROUGH the auto-published vw, authorized by the auto-derived claim.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := h.cli.Cluster(h.consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(cm.Object, "data", "token")

			return tok == "tok-xyz789", "consumer cm data.token=" + tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "cross-target token did not propagate to the consumer child")

		// The instance status maps agentToken from the provider child status.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := h.cli.Cluster(h.consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(got.Object, "status", "agentToken")

			return tok == "tok-xyz789", "status.agentToken=" + tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance status.agentToken not mapped")

		// --- M5: delete the instance and assert cross-workspace GC ---
		Expect(h.cli.Cluster(h.consumerPath).Delete(ctx, instance)).To(Succeed())

		// Provider-target AgentRequest is deleted from the provider workspace.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			ar := &unstructured.Unstructured{}
			ar.SetGroupVersionKind(agentGVK)
			err := h.cli.Cluster(h.providerPath).Get(ctx, agentKey, ar)

			return apierrors.IsNotFound(err), "agentrequest still present"
		}, wait.ForeverTestTimeout, 300*time.Millisecond, "provider AgentRequest not GC'd on instance delete")

		// Consumer-target ConfigMap is deleted from the consumer workspace.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := h.cli.Cluster(h.consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm)

			return apierrors.IsNotFound(err), "consumer ConfigMap still present"
		}, wait.ForeverTestTimeout, 300*time.Millisecond, "consumer ConfigMap not GC'd on instance delete")

		// The instance itself is gone (finalizer removed).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			err := h.cli.Cluster(h.consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got)

			return apierrors.IsNotFound(err), "instance still present (finalizer not removed)"
		}, wait.ForeverTestTimeout, 300*time.Millisecond, "instance not GC'd after finalizer removal")
	})
})

// newInstanceObj returns a fresh unstructured object typed to the instance GVK, so
// the multicluster builder derives the watched type from the embedded GVK.
func newInstanceObj(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)

	return u
}
