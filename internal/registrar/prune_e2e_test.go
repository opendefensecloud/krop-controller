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

// 5c-1 prune e2e — apply-set prune across both target workspaces.
//
// Proves Reconciler.pruneChildren end-to-end on real kcp: an includeWhen-gated
// provider child that is materialized while its gate is true must be DELETED once
// the gate flips false and a complete pass re-runs — while the always-present
// consumer child survives. This is the exact mechanism that reclaims
// includeWhen-excluded / forEach-shrunk / dropped-node children; before this spec
// only unit-level coverage existed for it on the live dynamic path.

import (
	"context"
	"time"

	"github.com/kcp-dev/multicluster-provider/envtest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const pruneBlueprintPath = "../../test/fixtures/blueprint-prune-rgd.yaml"
const pruneBindingPath = "../../test/fixtures/apibinding-prunedemo.yaml"

func pruneHarnessOptions() harnessOptions {
	return harnessOptions{
		blueprintFile: pruneBlueprintPath,
		blueprintObj:  "gatedcluster",
		exportName:    "gatedclusters.krop.opendefense.cloud",
		instanceGVK:   schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "GatedCluster"},
		instanceList:  schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "GatedClusterList"},
		bindingFile:   pruneBindingPath,
		bindingName:   "gatedclusters",
	}
}

var _ = Describe("5c-1 apply-set prune across targets", Ordered, func() {
	var (
		ctx = context.Background()
		h   *dynamicHarness

		agentGVK   = schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"}
		baseCfgKey = client.ObjectKey{Namespace: "default", Name: "eu-base-config"}
		agentName  string
		agentKey   client.ObjectKey
	)

	BeforeAll(func() {
		h = startDynamicHarness(ctx, pruneHarnessOptions())
	})

	AfterAll(func() {
		if h != nil {
			h.cancel()
		}
	})

	It("materializes the gated child then prunes it when the gate flips false", func() {
		// 1. Create the instance with the gate TRUE.
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "GatedCluster",
			"metadata": map[string]any{"name": "demo", "namespace": "default"},
			"spec":     map[string]any{"region": "eu", "withAgent": true},
		}}
		Expect(h.cli.Cluster(h.consumerPath).Create(ctx, instance)).To(Succeed())

		// The always-present consumer child materializes.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := h.cli.Cluster(h.consumerPath).Get(ctx, baseCfgKey, cm)

			return err == nil, "base config not created yet"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "always-present consumer child never materialized")

		// The gated provider child materializes in the provider workspace.
		agentName = kropengine.ProviderChildName(h.consumerWS.Spec.Cluster, "demo", "eu-agent")
		agentKey = client.ObjectKey{Namespace: "default", Name: agentName}
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			ar := &unstructured.Unstructured{}
			ar.SetGroupVersionKind(agentGVK)
			err := h.cli.Cluster(h.providerPath).Get(ctx, agentKey, ar)

			return err == nil, "gated provider child not created yet"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "includeWhen-gated provider child never materialized (gate true)")

		// 2. Flip the gate to FALSE.
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(instance.GroupVersionKind())
		Expect(h.cli.Cluster(h.consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got)).To(Succeed())
		Expect(unstructured.SetNestedField(got.Object, false, "spec", "withAgent")).To(Succeed())
		Expect(h.cli.Cluster(h.consumerPath).Update(ctx, got)).To(Succeed())

		// The gated provider child is PRUNED (dropped from the desired set).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			ar := &unstructured.Unstructured{}
			ar.SetGroupVersionKind(agentGVK)
			err := h.cli.Cluster(h.providerPath).Get(ctx, agentKey, ar)

			return apierrors.IsNotFound(err), "gated provider child still present (not pruned)"
		}, wait.ForeverTestTimeout, 300*time.Millisecond, "includeWhen-gated provider child was not pruned after gate flipped false")

		// The always-present consumer child SURVIVES the prune, in its own workspace.
		Consistently(func() bool {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := h.cli.Cluster(h.consumerPath).Get(ctx, baseCfgKey, cm)

			return err == nil
		}, 2*time.Second, 300*time.Millisecond).Should(BeTrue(), "always-present consumer child must survive the prune")
	})
})
