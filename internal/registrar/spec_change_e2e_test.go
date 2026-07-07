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

// 5c-3 spec-change restart e2e — proves 5a.
//
// A live blueprint edit must take effect WITHOUT a process restart. With the
// dynamic path running and an instance materialized, we edit the served blueprint
// (add a new `data.extra` key to the consumer ConfigMap child template) and
// re-drive the Registrar. The edit mints a new specHash, so OnPublished fires with
// changed=true → the Supervisor Stops and re-Ensures the export's instance manager
// with the NEW compiled graph. The already-materialized instance's ConfigMap must
// then gain the new field — proving the manager restarted with the new graph, all
// in the same process.

import (
	"context"
	"time"

	"github.com/kcp-dev/multicluster-provider/envtest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("5c-3 live blueprint spec change without restart", Ordered, func() {
	var (
		ctx = context.Background()
		h   *dynamicHarness

		agentGVK = schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"}
		cmGVK    = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		rgdGVK   = schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "ResourceGraphDefinition"}
		cmKey    = client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}
	)

	BeforeAll(func() {
		h = startDynamicHarness(ctx, kubernetesClusterHarnessOptions())
	})

	AfterAll(func() {
		if h != nil {
			h.cancel()
		}
	})

	It("re-materializes children from the edited graph after a live republish", func() {
		// 1. Materialize the instance fully: create it, set the provider token so the
		//    consumer ConfigMap converges (it must EXIST so we can observe the edit).
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "KubernetesCluster",
			"metadata": map[string]any{"name": "demo", "namespace": "default"},
			"spec":     map[string]any{"region": "eu"},
		}}
		Expect(h.cli.Cluster(h.consumerPath).Create(ctx, instance)).To(Succeed())

		agentName := kropengine.ProviderChildName(h.consumerWS.Spec.Cluster, "demo", "eu-agent")
		agentKey := client.ObjectKey{Namespace: "default", Name: agentName}
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			return h.providerChildExists(ctx, agentGVK, agentName), "provider AgentRequest not created yet"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "provider AgentRequest never created")

		ar := &unstructured.Unstructured{}
		ar.SetGroupVersionKind(agentGVK)
		Expect(h.cli.Cluster(h.providerPath).Get(ctx, agentKey, ar)).To(Succeed())
		Expect(unstructured.SetNestedField(ar.Object, "tok-xyz789", "status", "token")).To(Succeed())
		Expect(h.cli.Cluster(h.providerPath).Status().Update(ctx, ar)).To(Succeed())

		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(cmGVK)
			if err := h.cli.Cluster(h.consumerPath).Get(ctx, cmKey, cm); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(cm.Object, "data", "token")

			return tok == "tok-xyz789", "consumer cm data.token=" + tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "consumer ConfigMap never converged")

		// The consumer ConfigMap must NOT yet carry the not-yet-authored `extra` key.
		cmBefore := &unstructured.Unstructured{}
		cmBefore.SetGroupVersionKind(cmGVK)
		Expect(h.cli.Cluster(h.consumerPath).Get(ctx, cmKey, cmBefore)).To(Succeed())
		_, hadExtra, _ := unstructured.NestedString(cmBefore.Object, "data", "extra")
		Expect(hadExtra).To(BeFalse(), "precondition: config must not have the `extra` key before the edit")

		// 2. EDIT the served blueprint: add data.extra to the ConfigMap child template.
		rgd := &unstructured.Unstructured{}
		rgd.SetGroupVersionKind(rgdGVK)
		Expect(h.providerClient.Get(ctx, client.ObjectKey{Name: "kubernetescluster"}, rgd)).To(Succeed())
		resources, found, err := unstructured.NestedSlice(rgd.Object, "spec", "resources")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		edited := false
		for i := range resources {
			rm, ok := resources[i].(map[string]any)
			if !ok || rm["id"] != "config" {
				continue
			}
			tmpl, ok := rm["template"].(map[string]any)
			Expect(ok).To(BeTrue(), "config template must be a map")
			data, ok := tmpl["data"].(map[string]any)
			Expect(ok).To(BeTrue(), "config template.data must be a map")
			data["extra"] = "edited-live"
			edited = true
		}
		Expect(edited).To(BeTrue(), "config resource not found in blueprint")
		Expect(unstructured.SetNestedSlice(rgd.Object, resources, "spec", "resources")).To(Succeed())
		Expect(h.providerClient.Update(ctx, rgd)).To(Succeed())

		// 3. Re-drive the Registrar for the edited blueprint: new specHash → OnPublished
		//    with changed=true → Supervisor Stop + Ensure restarts the manager with the
		//    NEW graph. Same process throughout — no restart.
		h.republish(ctx)
		Expect(h.sup.Running(h.opts.exportName)).To(BeTrue(),
			"export manager must be running again after the live republish")

		// 4. The already-materialized ConfigMap gains the new field — proving the
		//    restarted manager applied the EDITED graph (the token/region survive too).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(cmGVK)
			if err := h.cli.Cluster(h.consumerPath).Get(ctx, cmKey, cm); err != nil {
				return false, err.Error()
			}
			extra, _, _ := unstructured.NestedString(cm.Object, "data", "extra")
			tok, _, _ := unstructured.NestedString(cm.Object, "data", "token")

			return extra == "edited-live" && tok == "tok-xyz789",
				"data.extra=" + extra + " data.token=" + tok
		}, 90*time.Second, 500*time.Millisecond, "edited blueprint did not take effect on the live instance")
	})
})
