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

// 5c-2 orphan-sweep e2e — mid-life unbind reclaimed by the Sweeper.
//
// This is the scenario that would have caught C1 and C2. A consumer instance
// materializes a provider AgentRequest + a liveness record in the provider
// workspace. The consumer then UNBINDS the APIExport mid-life: the virtual
// workspace disengages that logical cluster, so the instance reconciler stops
// touching the record and the provider child would orphan forever. The Sweeper,
// running against the real provider-workspace client, finds the stale record and
// reclaims the provider child + record.
//
// C1 is directly exercised: the consumer ConfigMap PENDS forever here (its token
// is never set), so every reconcile pass is INCOMPLETE — yet the liveness record
// must still exist to sweep against (C1: the record is written even on pending
// passes). C2 (startup grace) is exercised by the Sweeper's real-time grace window.
//
// NEGATIVE: a second, still-BOUND instance's record is kept fresh by the running
// manager's heartbeat, so it must survive the same sweeps that reclaim the orphan.

import (
	"context"
	"time"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/envtest"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("5c-2 orphan sweep on mid-life unbind", Ordered, func() {
	var (
		ctx = context.Background()
		h   *dynamicHarness

		agentGVK = schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"}
		cmGVK    = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

		orphanPath logicalcluster.Path
		orphanWS   *tenancyv1alpha1.Workspace
		freshPath  logicalcluster.Path
		freshWS    *tenancyv1alpha1.Workspace
	)

	// createKC creates a region-eu KubernetesCluster instance in the given consumer.
	createKC := func(path logicalcluster.Path, name string) {
		inst := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "KubernetesCluster",
			"metadata": map[string]any{"name": name, "namespace": "default"},
			"spec":     map[string]any{"region": "eu"},
		}}
		Expect(h.cli.Cluster(path).Create(ctx, inst)).To(Succeed())
	}

	recordExists := func(name string) bool {
		rec := &unstructured.Unstructured{}
		rec.SetGroupVersionKind(cmGVK)
		err := h.providerClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, rec)
		if err == nil {
			return true
		}
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "unexpected error getting liveness record: %v", err)

		return false
	}

	// waitProviderMaterialized blocks until the instance's provider AgentRequest AND
	// its liveness record both exist in the provider workspace. The liveness record
	// existing DESPITE the pending (incomplete) consumer child is the C1 assertion.
	waitProviderMaterialized := func(cluster, instance, uid string) (agentName, recordName string) {
		agentName = kropengine.ProviderChildName(cluster, instance, "eu-agent")
		recordName = kropengine.LivenessRecordName(cluster, uid)
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			if !h.providerChildExists(ctx, agentGVK, agentName) {
				return false, "provider AgentRequest not yet created"
			}
			if !recordExists(recordName) {
				return false, "liveness record not yet written (C1)"
			}

			return true, ""
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "provider child + liveness record never materialized")

		return agentName, recordName
	}

	BeforeAll(func() {
		h = startDynamicHarness(ctx, kubernetesClusterHarnessOptions())
		orphanPath, orphanWS = h.consumerPath, h.consumerWS
		freshPath, freshWS = h.bindConsumer(ctx)
	})

	AfterAll(func() {
		if h != nil {
			h.cancel()
		}
	})

	It("sweeps the orphaned provider child + record while a fresh instance survives", func() {
		// Both instances leave their consumer ConfigMap pending (token never set), so
		// every pass is INCOMPLETE — the liveness record must still be written (C1).
		createKC(orphanPath, "orphan")
		createKC(freshPath, "fresh")

		orphanUID := h.getInstanceUID(ctx, orphanPath, "orphan")
		freshUID := h.getInstanceUID(ctx, freshPath, "fresh")

		orphanAgent, orphanRecord := waitProviderMaterialized(orphanWS.Spec.Cluster, "orphan", orphanUID)
		freshAgent, freshRecord := waitProviderMaterialized(freshWS.Spec.Cluster, "fresh", freshUID)

		// Mid-life unbind: delete the orphan consumer's APIBinding. The vw disengages
		// its logical cluster, so its reconciler stops refreshing the record.
		h.unbindConsumer(ctx, orphanPath)

		// Right after unbind the provider child still exists (nothing has reclaimed it).
		Expect(h.providerChildExists(ctx, agentGVK, orphanAgent)).To(BeTrue(),
			"orphaned provider child must still exist immediately after unbind")

		// A Sweeper on the real provider client. StaleAfter (8s) sits comfortably above
		// the ~1s reconcile heartbeat that keeps the still-bound fresh instance's record
		// fresh; it also doubles as the startup grace window (C2), so the first sweeps
		// no-op until real time has advanced a full StaleAfter past the sweeper start.
		sweeper := &kropctrl.Sweeper{
			ProviderClient:  h.providerClient,
			RecordNamespace: "default",
			StaleAfter:      8 * time.Second,
		}

		// Poll Sweep directly. Once the grace window closes and the orphan record has
		// gone stale, the orphan's provider child + record are reclaimed. The fresh
		// instance's manager keeps beating (~1s), so its record never goes stale.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			if err := sweeper.Sweep(ctx); err != nil {
				return false, "sweep error: " + err.Error()
			}
			if h.providerChildExists(ctx, agentGVK, orphanAgent) {
				return false, "orphan provider child not yet swept"
			}
			if recordExists(orphanRecord) {
				return false, "orphan liveness record not yet swept"
			}

			return true, ""
		}, 60*time.Second, 500*time.Millisecond, "orphaned provider child + record were never swept")

		// NEGATIVE: the fresh, still-bound instance's provider child + record survive —
		// its record's lastReconciled stays recent via the manager heartbeat. Keep
		// sweeping for a few more seconds and assert it holds throughout.
		Consistently(func() bool {
			Expect(sweeper.Sweep(ctx)).To(Succeed())

			return h.providerChildExists(ctx, agentGVK, freshAgent) && recordExists(freshRecord)
		}, 3*time.Second, 500*time.Millisecond).Should(BeTrue(),
			"fresh instance's provider child/record must survive the sweep")
	})
})
