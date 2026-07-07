# M2 — Dual-Target Apply Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One instance materializes children in **two workspaces** — a consumer-target child written into the consumer workspace *through the APIExport virtual workspace* (permissionClaim-authorized) and a collision-free-named provider-target child written into the provider workspace via a direct client — proven end-to-end by a proper vw-based envtest e2e.

**Architecture:** The engine's `Reconcile` already routes each desired object by its `krop.opendefense.cloud/target` annotation to a `map[Target]Applier`; M2 wires the `TargetProvider` applier. Two new small engine pieces: `ProviderChildName` (deterministic, collision-free naming) and `QualifyingApplier` (wraps an applier and renames `metadata.name` before apply — keeps the engine loop untouched). A new `internal/controller.Reconciler` extracts the reconcile glue so `cmd/controller/main.go` and the e2e share one dual-target path. The e2e runs the **real production path**: an in-process multicluster-runtime manager against the APIExport virtual workspace (bind a consumer *before* awaiting endpoints — the load-bearing rule from the M2 spike).

**Tech Stack:** Go 1.26, kro v0.9.2, kcp-dev/sdk v0.32.2, multicluster-provider v0.8.0, multicluster-runtime v0.24.1, controller-runtime v0.24.1, k8s v0.36.2. Envtest kcp binary v0.30.0.

**Scope (from design §3 M2, refined by the M2 spike):**
- **In scope, proven end-to-end:** dual-target apply (consumer via vw + provider via direct client), per-resource routing, collision-free provider-child naming, one **hand-written** permissionClaim (`configmaps`) that the consumer's APIBinding accepts (required for the vw write to succeed), and a proper vw-based e2e.
- **Deferred (later milestones):** permissionClaims **auto-derivation** (M4), cross-target CEL dependencies (M3), realistic provider child types like `AgentRequest` (need a CRD; M2 uses a ConfigMap), the full GC/finalizer story (M5).
- **This retires** the previously "compile-only" status of main.go's virtual-workspace fan-in — M2 exercises it for real. (The M1 e2e's direct-reconcile design was based on a misdiagnosis; M2's vw e2e supersedes it — see the "Remove the M1 direct-reconcile e2e" task.)

**Key spike facts (from `hack/vwspike/`, a throwaway proof — read it before Task 8, then it gets removed in Task 9):**
- kcp advertises the APIExport vw URL on a shard **only once at least one `APIBinding` consumes the export**. The e2e MUST create the consumer workspace + APIBinding **before** waiting for `APIExportEndpointSlice.status.endpoints`.
- vw wiring: `providerConfig := rest.CopyConfig(kcpConfig); providerConfig.Host += providerPath.RequestPath()`; `p, _ := apiexport.New(providerConfig, exportName, apiexport.Options{Scheme})`; `mgr, _ := mcmanager.New(providerConfig, p, mcmanager.Options{Scheme})`; reconciler gets the per-cluster consumer client via `mgr.GetCluster(ctx, req.ClusterName).GetClient()`.
- The default `APIExportEndpointSlice` is auto-created, named after the export, in the provider ws; wait on `slice.Status.APIExportEndpoints[0].URL`.
- `envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("x"))` returns `(logicalcluster.Name, logicalcluster.Path)` — the **first** return is the cluster name we need for provider-child naming.

**Verified permissionClaims shapes (kcp-dev/sdk v0.32.2 `apis/apis/v1alpha2`):**
- APIExport `spec.permissionClaims[]`: inline `GroupResource` (`group`, `resource`) + `verbs` (required, min 1) + `identityHash` (empty for core types like `configmaps`).
- APIBinding `spec.permissionClaims[]` (`AcceptablePermissionClaim`): the claim (group/resource/verbs/identityHash) + `selector` (`PermissionClaimSelector`, use `matchAll: true`) + `state: Accepted`.

---

## File structure

| File | Responsibility |
|---|---|
| `internal/engine/naming.go` + `naming_test.go` | `ProviderChildName(cluster, instance, original) string` — deterministic, DNS-safe, ≤253, collision-free. Pure. |
| `internal/engine/apply.go` (extend) + `apply_test.go` (extend) | `QualifyingApplier` — wraps an `Applier`, renames `metadata.name` before delegating. |
| `internal/engine/testsupport_test.go` (extend) + `engine_test.go` (extend) | Add a provider-target child to `sampleRGD`; assert dual-target routing through the real runtime. |
| `internal/controller/reconciler.go` + `reconciler_test.go` | `Reconciler` — the shared dual-target reconcile glue used by main.go and the e2e. |
| `cmd/controller/main.go` (modify) | Build a provider-workspace client; delegate the `mcreconcile.Func` to `Reconciler`. |
| `internal/engine/embedded/blueprint-kubernetescluster.yaml` + `config/kcp/examples/blueprint-kubernetescluster.yaml` (modify) | Add a provider-target ConfigMap child. |
| `config/kcp/apiexport-krop-m1.yaml` (modify) | Add the `configmaps` permissionClaim. |
| `test/fixtures/apibinding-kubernetescluster.yaml` (modify) | Accept the `configmaps` claim. |
| `internal/controller/suite_test.go` (keep) + `internal/controller/dualtarget_test.go` (new, replaces `m1_integration_test.go`) | The vw-based dual-target e2e. |
| `hack/vwspike/` (remove in Task 9) | Throwaway spike proof — reference for Task 8, then delete. |

---

## Task 1: Provider-child collision-free naming

**Files:**
- Create: `internal/engine/naming.go`
- Test: `internal/engine/naming_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/naming_test.go
package engine

import (
	"strings"
	"testing"
)

func TestProviderChildName_Deterministic(t *testing.T) {
	a := ProviderChildName("cluster1", "demo", "eu-record")
	b := ProviderChildName("cluster1", "demo", "eu-record")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
}

func TestProviderChildName_CollisionFreeAcrossClusters(t *testing.T) {
	a := ProviderChildName("cluster1", "demo", "eu-record")
	b := ProviderChildName("cluster2", "demo", "eu-record")
	if a == b {
		t.Fatalf("different consumers must not collide, both %q", a)
	}
}

func TestProviderChildName_CollisionFreeAcrossInstances(t *testing.T) {
	a := ProviderChildName("cluster1", "demo", "eu-record")
	b := ProviderChildName("cluster1", "prod", "eu-record")
	if a == b {
		t.Fatalf("different instances must not collide, both %q", a)
	}
}

func TestProviderChildName_LongInputStaysDNSSafe(t *testing.T) {
	long := strings.Repeat("a", 300)
	got := ProviderChildName(long, long, long)
	if len(got) > 253 {
		t.Fatalf("name too long: %d", len(got))
	}
	// deterministic even when hashed
	if got != ProviderChildName(long, long, long) {
		t.Fatal("hashed form not deterministic")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestProviderChildName -v`
Expected: FAIL — `undefined: ProviderChildName`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/naming.go
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

package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// maxNameLen is the Kubernetes metadata.name length ceiling.
const maxNameLen = 253

// ProviderChildName derives a deterministic, collision-free, DNS-safe name for a
// provider-target child. Many consumers' provider children land in ONE provider
// workspace (idea.md §9.1), so the name is qualified by the consumer's logical
// cluster name and the instance name. Inputs are assumed already DNS-safe
// (kcp cluster names, k8s-validated instance names, blueprint template names);
// over-long results fall back to a truncated prefix + content hash.
func ProviderChildName(clusterName, instanceName, originalName string) string {
	base := fmt.Sprintf("%s-%s-%s", clusterName, instanceName, originalName)
	if len(base) <= maxNameLen {
		return base
	}
	sum := sha256.Sum256([]byte(base))
	suffix := hex.EncodeToString(sum[:])[:16]
	// leave room for "-" + 16 hex chars
	prefix := base[:maxNameLen-1-len(suffix)]
	return prefix + "-" + suffix
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -run TestProviderChildName -v`
Expected: PASS (all four).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/naming.go internal/engine/naming_test.go
git commit --no-verify -m "engine: collision-free provider-child naming (M2)"
```

---

## Task 2: QualifyingApplier

**Files:**
- Modify: `internal/engine/apply.go`
- Test: `internal/engine/apply_test.go`

- [ ] **Step 1: Write the failing test**

```go
// append to internal/engine/apply_test.go

func TestQualifyingApplier_RenamesBeforeDelegating(t *testing.T) {
	inner := &fakeApplier{}
	q := NewQualifyingApplier(inner, func(orig string) string { return "pfx-" + orig })

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "record", "namespace": "default"},
	}}
	if _, err := q.Apply(context.Background(), obj); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(inner.applied) != 1 {
		t.Fatalf("inner not called once: %d", len(inner.applied))
	}
	if got := inner.applied[0].GetName(); got != "pfx-record" {
		t.Fatalf("inner received name %q, want pfx-record", got)
	}
	// original object must not be mutated
	if obj.GetName() != "record" {
		t.Fatalf("caller's object was mutated: name=%q", obj.GetName())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestQualifyingApplier -v`
Expected: FAIL — `undefined: NewQualifyingApplier`.

- [ ] **Step 3: Write minimal implementation**

```go
// append to internal/engine/apply.go

// QualifyingApplier wraps an Applier and rewrites metadata.name via a rename
// function before delegating. Used to give provider-target children collision-
// free names (see ProviderChildName) without changing the engine's routing loop.
type QualifyingApplier struct {
	inner  Applier
	rename func(original string) string
}

// NewQualifyingApplier returns a QualifyingApplier over inner.
func NewQualifyingApplier(inner Applier, rename func(original string) string) *QualifyingApplier {
	return &QualifyingApplier{inner: inner, rename: rename}
}

// Apply renames a copy of obj (metadata.name → rename(name)) and delegates.
func (q *QualifyingApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	renamed := obj.DeepCopy()
	renamed.SetName(q.rename(obj.GetName()))
	return q.inner.Apply(ctx, renamed)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -run TestQualifyingApplier -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/apply.go internal/engine/apply_test.go
git commit --no-verify -m "engine: QualifyingApplier for provider-child renaming (M2)"
```

---

## Task 3: Dual-target routing test (engine, real runtime, no kcp)

**Files:**
- Modify: `internal/engine/testsupport_test.go` (add a provider-target child to `sampleRGD`)
- Modify: `internal/engine/engine_test.go` (assert provider routing)

This proves the engine routes a provider-target node to the provider applier via the *real* kro runtime, with no cluster.

- [ ] **Step 1: Add a provider-target child to the test RGD**

In `internal/engine/testsupport_test.go`, extend `sampleRGD` with a second resource routed to `provider`. Add this `generator.WithResource(...)` call after the existing `config` resource (inside the `generator.NewResourceGraphDefinition(...)` arg list):

```go
		generator.WithResource("providerRecord", map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "${schema.spec.region}-provider-record",
				"namespace": "default",
				"annotations": map[string]interface{}{
					TargetAnnotation: string(TargetProvider),
				},
			},
			"data": map[string]interface{}{"region": "${schema.spec.region}"},
		}, nil, nil),
```

Note: `sampleRGD` already sets `status.configMapName = ${config.metadata.name}`. Leave that; do not reference the provider record in status (cross-target status is M3).

Adding this node breaks `TestBuildTestGraph_Builds` (in the same file), which asserts the topological order is `[config]`. Update it now to accept two nodes:

```go
	if len(g.TopologicalOrder) != 2 {
		t.Fatalf("topological order = %v, want 2 nodes", g.TopologicalOrder)
	}
```

- [ ] **Step 2: Write the failing test**

```go
// append to internal/engine/engine_test.go

func TestReconcile_RoutesToBothTargets(t *testing.T) {
	rt := newRuntime(t, newInstance("eu"))
	consumer := &fakeApplier{}
	provider := &fakeApplier{}

	e := New()
	res, err := e.Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer,
		TargetProvider: provider,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 1 || consumer.applied[0].GetName() != "eu-cluster-config" {
		t.Fatalf("consumer applier got %d objs: %+v", len(consumer.applied), consumer.applied)
	}
	if len(provider.applied) != 1 || provider.applied[0].GetName() != "eu-provider-record" {
		t.Fatalf("provider applier got %d objs: %+v", len(provider.applied), provider.applied)
	}
	if !res.Ready {
		t.Fatalf("want Ready, got %+v", res)
	}
}
```

Note: the provider name here is `eu-provider-record` (the blueprint template name) because `fakeApplier` is used directly — the collision-free renaming is done by `QualifyingApplier` (Task 2), which the `Reconciler` (Task 4) composes around the provider `SSAApplier`. The engine itself does not rename; it only routes.

- [ ] **Step 3: Run test to verify it fails, then passes**

Run: `go test ./internal/engine/ -run 'TestReconcile' -v`
Expected: initially FAIL if `sampleRGD` wasn't updated (provider applier gets 0) — after Step 1 it PASSES. Also re-run the whole package: `go test ./internal/engine/ -v` — the existing `TestReconcile_AppliesConsumerChild_StripsRouting` and `TestReconcile_ProjectsInstanceStatus` must STILL PASS (they only assert consumer/instance behavior; the extra provider node with `TargetConsumer` unconfigured would break `TestReconcile_AppliesConsumerChild_StripsRouting` which passes only `{TargetConsumer: consumer}` — the provider node then has no applier and errors). **Fix:** update `TestReconcile_AppliesConsumerChild_StripsRouting` and `TestReconcile_ProjectsInstanceStatus` to also pass a `TargetProvider: &fakeApplier{}` in their applier maps, since `sampleRGD` now has a provider node. Make that edit.

- [ ] **Step 4: Verify full package green**

Run: `go test ./internal/engine/ -v`
Expected: PASS (all engine tests, including the two updated ones + the new dual-target test).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/testsupport_test.go internal/engine/engine_test.go
git commit --no-verify -m "engine: prove dual-target routing via the real runtime (M2)"
```

---

## Task 4: Reconciler (shared dual-target glue)

**Files:**
- Create: `internal/controller/reconciler.go`
- Test: `internal/controller/reconciler_test.go`

Extracts the reconcile logic (currently inline in `cmd/controller/main.go`) into a reusable type so main.go and the e2e share ONE dual-target path (addresses the M1 drift concern). Its full behavior is exercised by the vw e2e (Task 8); here we unit-test construction + the consumer-not-found path with a fake client.

- [ ] **Step 1: Write the failing test**

```go
// internal/controller/reconciler_test.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var testGVK = schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesCluster"}

func TestReconciler_InstanceNotFound_NoError(t *testing.T) {
	consumer := fake.NewClientBuilder().Build()
	r := &Reconciler{Graph: nil, ProviderClient: consumer, InstanceGVK: testGVK}

	// No instance exists → IgnoreNotFound → no error, empty result.
	_, err := r.Reconcile(context.Background(), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "missing"})
	if err != nil {
		t.Fatalf("expected nil error on not-found, got %v", err)
	}
}

var _ = unstructured.Unstructured{} // keep import if unused after edits
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconciler_InstanceNotFound -v`
Expected: FAIL — `undefined: Reconciler` / package `controller` doesn't exist yet.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/controller/reconciler.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

// Package controller holds the krop instance reconcile glue shared by the
// controller entrypoint and the envtest e2e: one dual-target reconcile path.
package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// Reconciler drives one instance through the engine, applying consumer-target
// children via the per-request consumer client and provider-target children via
// a fixed provider-workspace client with collision-free names.
type Reconciler struct {
	// Graph is the compiled blueprint graph (built once at startup).
	Graph *krograph.Graph
	// ProviderClient writes provider-target children into the provider workspace.
	ProviderClient client.Client
	// InstanceGVK is the generated instance kind this reconciler serves.
	InstanceGVK schema.GroupVersionKind
}

// Reconcile fetches the instance via consumerClient, drives the engine with both
// appliers, and writes back projected status. clusterName is the consumer's
// logical cluster (used to qualify provider-child names). A missing instance is
// not an error.
func (r *Reconciler) Reconcile(ctx context.Context, consumerClient client.Client, clusterName string, key client.ObjectKey) (kropengine.Result, error) {
	inst := &unstructured.Unstructured{}
	inst.SetGroupVersionKind(r.InstanceGVK)
	if err := consumerClient.Get(ctx, key, inst); err != nil {
		return kropengine.Result{}, client.IgnoreNotFound(err)
	}

	rt, err := kroruntime.FromGraph(r.Graph, inst, krograph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		return kropengine.Result{}, fmt.Errorf("runtime: %w", err)
	}

	instanceName := inst.GetName()
	appliers := map[kropengine.Target]kropengine.Applier{
		kropengine.TargetConsumer: kropengine.NewSSAApplier(consumerClient),
		kropengine.TargetProvider: kropengine.NewQualifyingApplier(
			kropengine.NewSSAApplier(r.ProviderClient),
			func(orig string) string { return kropengine.ProviderChildName(clusterName, instanceName, orig) },
		),
	}

	res, err := kropengine.New().Reconcile(ctx, rt, appliers)
	if err != nil {
		return res, err
	}

	if desired, perr := kropengine.ProjectStatus(rt); perr == nil {
		if status, found, _ := unstructured.NestedMap(desired.Object, "status"); found {
			_ = unstructured.SetNestedMap(inst.Object, status, "status")
			_ = consumerClient.Status().Update(ctx, inst)
		}
	}
	return res, nil
}
```

Then delete the `var _ = unstructured.Unstructured{}` line from the test if it causes an "imported and not used" issue — adjust the test imports so it compiles (only `client`, `fake`, `schema`, `context`, `testing` are needed; drop `unstructured` from the test if unused).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestReconciler_InstanceNotFound -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/reconciler.go internal/controller/reconciler_test.go
git commit --no-verify -m "controller: extract shared dual-target Reconciler (M2)"
```

---

## Task 5: Wire the provider client into main.go

**Files:**
- Modify: `cmd/controller/main.go`

Build a provider-workspace client and delegate the `mcreconcile.Func` to `Reconciler`. The provider workspace is where the controller's kubeconfig points (the same `cfg` used to discover the endpoint slice) — the engine's own identity has RBAC there.

- [ ] **Step 1: Replace the inline reconcile closure**

In `cmd/controller/main.go`, after the compiled graph is built and before/around the manager wiring, construct a provider client and a `Reconciler`, and make the `mcreconcile.Func` delegate to it. Concretely:

Add import: `kropctrl "go.opendefense.cloud/krop-controller/internal/controller"`.

After `cfg` is validated and the graph `compiled` is built, add:

```go
	// The provider workspace is the one this controller is configured against
	// (where the blueprint's APIExport lives); the engine's identity has RBAC
	// there, so provider-target children are written with a direct client.
	providerClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("provider client: %w", err)
	}
	reconciler := &kropctrl.Reconciler{
		Graph:          compiled,
		ProviderClient: providerClient,
		InstanceGVK:    instanceGVK,
	}
```

Replace the existing `mcreconcile.Func(func(ctx, req) {...})` body with a delegation:

```go
	reconcileFn := mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
		cl, err := mgr.GetCluster(ctx, req.ClusterName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting cluster %q: %w", req.ClusterName, err)
		}
		res, err := reconciler.Reconcile(ctx, cl.GetClient(), string(req.ClusterName), req.NamespacedName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if res.Requeue {
			return ctrl.Result{RequeueAfter: requeueInterval}, nil
		}
		return ctrl.Result{}, nil
	})
```

Update the builder to use `reconcileFn` (whatever the local var was named before — keep `Named("krop-instance").For(newInstance(instanceGVK)).Complete(reconcileFn)`). Remove the now-unused engine imports from main.go if the closure no longer references them directly (e.g. `kroruntime`, `krograph`, `kropengine` may become unused — let the compiler tell you and drop them; `instanceGVK`/`newInstance` stay).

- [ ] **Step 2: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: clean. Fix any unused-import errors by removing imports main.go no longer uses.

- [ ] **Step 3: Commit**

```bash
git add cmd/controller/main.go
git commit --no-verify -m "controller: wire provider client + shared Reconciler into main (M2)"
```

---

## Task 6: Add a provider-target child to the blueprint

**Files:**
- Modify: `internal/engine/embedded/blueprint-kubernetescluster.yaml`
- Modify: `config/kcp/examples/blueprint-kubernetescluster.yaml`

Both copies must stay byte-identical except their header sync-comment.

- [ ] **Step 1: Add the provider resource to BOTH YAMLs**

Append this resource under `spec.resources` in both files (after the existing `config` resource):

```yaml
    - id: providerRecord
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.region}-provider-record
          namespace: default
          annotations:
            krop.opendefense.cloud/target: provider
        data:
          region: ${schema.spec.region}
```

Leave `spec.schema.status` as-is (`configMapName: ${config.metadata.name}` only — no provider reference; cross-target status is M3).

- [ ] **Step 2: Update the loader test and verify**

Adding the second resource to the embedded YAML breaks `TestLoadExampleBlueprint` (in `internal/engine/blueprint_test.go`), which asserts exactly one resource with id `config`. Update it to assert both resources are present:

```go
	if len(rgd.Spec.Resources) != 2 {
		t.Fatalf("want 2 resources, got %d", len(rgd.Spec.Resources))
	}
	ids := map[string]bool{}
	for _, r := range rgd.Spec.Resources {
		ids[r.ID] = true
	}
	if !ids["config"] || !ids["providerRecord"] {
		t.Fatalf("want resources config+providerRecord, got %v", ids)
	}
```

Run: `go test ./internal/engine/ -run 'TestLoadExampleBlueprint|TestBuildTestGraph|TestReconcile' -v`
Expected: PASS. (The embedded blueprint and `sampleRGD` are separate definitions but must agree in shape; keep them in sync.)

- [ ] **Step 3: Commit**

```bash
git add internal/engine/embedded/blueprint-kubernetescluster.yaml config/kcp/examples/blueprint-kubernetescluster.yaml internal/engine/blueprint_test.go
git commit --no-verify -m "blueprint: add a provider-target ConfigMap child (M2)"
```

---

## Task 7: permissionClaim fixtures

**Files:**
- Modify: `config/kcp/apiexport-krop-m1.yaml`
- Modify: `test/fixtures/apibinding-kubernetescluster.yaml`

The consumer-target ConfigMap is written *through the vw* as the APIExport identity, so the APIExport must claim `configmaps` and the APIBinding must accept it.

- [ ] **Step 1: Add the permissionClaim to the APIExport**

Set `spec.permissionClaims` in `config/kcp/apiexport-krop-m1.yaml`:

```yaml
  permissionClaims:
    - group: ""
      resource: configmaps
      verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

(Core type → empty `group`, no `identityHash`.)

- [ ] **Step 2: Accept the claim in the APIBinding**

Set `spec.permissionClaims` in `test/fixtures/apibinding-kubernetescluster.yaml`:

```yaml
  permissionClaims:
    - group: ""
      resource: configmaps
      verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
      selector:
        matchAll: true
      state: Accepted
```

Confirm both shapes against `github.com/kcp-dev/sdk@v0.32.2/apis/apis/v1alpha2/{types_apiexport.go,types_apibinding.go}` (PermissionClaim / AcceptablePermissionClaim / PermissionClaimSelector). Adjust field names if the SDK differs.

- [ ] **Step 3: Commit**

```bash
git add config/kcp/apiexport-krop-m1.yaml test/fixtures/apibinding-kubernetescluster.yaml
git commit --no-verify -m "kcp: claim + accept configmaps for the consumer-target write (M2)"
```

---

## Task 8: vw-based dual-target e2e

**Files:**
- Create: `internal/controller/dualtarget_test.go`
- Delete: `internal/controller/m1_integration_test.go`
- Keep/adjust: `internal/controller/suite_test.go`

**Before writing:** read `hack/vwspike/apiexport_test.go` and `hack/vwspike/m1shape_test.go` for the exact, working vw wiring on our stack (bind-first ordering, `apiexport.New`/`mcmanager.New`, endpoint-slice wait, `cli.Cluster(path)`, `NewWorkspaceFixture` returning the cluster name). Reproduce that shape.

`suite_test.go` already boots kcp and registers the scheme (from M1) — reuse it. If it declares package-level helpers `m1_integration_test.go` used, move any still-needed helper (e.g. `applyFixtureFromFile`) into `dualtarget_test.go` or `suite_test.go`.

- [ ] **Step 1: Delete the superseded direct-reconcile test**

```bash
git rm internal/controller/m1_integration_test.go
```

- [ ] **Step 2: Write the vw dual-target spec**

Create `internal/controller/dualtarget_test.go` (package `controller_test`) implementing this Ordered spec. Use the exact helper signatures verified in `hack/vwspike/`:

```go
// internal/controller/dualtarget_test.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package controller_test

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
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	"github.com/kcp-dev/sdk/apis/core"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"

	kropctrl "go.opendefense.cloud/krop-controller/internal/controller"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

const m2ExportName = "kubernetesclusters.krop.opendefense.cloud"

func applyFixtureFromFile(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, file string, vars map[string]string) {
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
		ctx                 = context.Background()
		cli                 clusterclient.ClusterClient
		providerPath        logicalcluster.Path
		consumerPath        logicalcluster.Path
		consumerClusterName logicalcluster.Name
		cancel              context.CancelFunc
	)

	BeforeAll(func() {
		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		// Provider workspace: ARS + APIExport (with the configmaps claim).
		_, providerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-provider"))
		applyFixtureFromFile(ctx, cli, providerPath, "../../config/kcp/apiresourceschema-kubernetesclusters.krop.opendefense.cloud.yaml", nil)
		applyFixtureFromFile(ctx, cli, providerPath, "../../config/kcp/apiexport-krop-m1.yaml", nil)

		// Consumer workspace + APIBinding (accepting the claim) — BEFORE awaiting
		// endpoints. kcp only advertises the vw URL once a binding exists.
		consumerClusterName, consumerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("krop-consumer"))
		applyFixtureFromFile(ctx, cli, consumerPath, "../../test/fixtures/apibinding-kubernetescluster.yaml",
			map[string]string{"PROVIDER_PATH": providerPath.String()})

		// Await the endpoint slice URL.
		var vwEndpoint string
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			slice := &apisv1alpha1.APIExportEndpointSlice{}
			if err := cli.Cluster(providerPath).Get(ctx, client.ObjectKey{Name: m2ExportName}, slice); err != nil {
				return false, err.Error()
			}
			if len(slice.Status.APIExportEndpoints) == 0 {
				return false, "no endpoints yet"
			}
			vwEndpoint = slice.Status.APIExportEndpoints[0].URL
			return true, ""
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "APIExport vw endpoints not populated")

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
		providerClient, err := client.New(providerConfig, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		instGVK := schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesCluster"}
		reconciler := &kropctrl.Reconciler{Graph: compiled, ProviderClient: providerClient, InstanceGVK: instGVK}

		// In-process manager against the APIExport virtual workspace.
		p, err := apiexport.New(providerConfig, m2ExportName, apiexport.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())
		mgr, err := mcmanager.New(providerConfig, p, mcmanager.Options{Scheme: scheme})
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
		_ = manager.Options{} // keep import if unused after edits
		_ = vwEndpoint         // endpoint URL is consumed by apiexport.New via the slice; retained for debugging
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
			return region == "eu", "consumer cm data.region="+region
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "consumer-target ConfigMap not materialized")

		// Provider-target ConfigMap in the provider workspace, collision-free name.
		wantProvider := kropengine.ProviderChildName(consumerClusterName.String(), "demo", "eu-provider-record")
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := cli.Cluster(providerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: wantProvider}, cm); err != nil {
				return false, err.Error()
			}
			region, _, _ := unstructured.NestedString(cm.Object, "data", "region")
			return region == "eu", "provider cm data.region="+region
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "provider-target ConfigMap not materialized as "+wantProvider)

		// Instance status projected.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			name, _, _ := unstructured.NestedString(got.Object, "status", "configMapName")
			return name == "eu-cluster-config", "status.configMapName="+name
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance status not projected")
	})
})
```

Implementer notes:
- Remove the two `_ = ...` keep-alive lines once imports settle; they're there so the snippet compiles if you trim usages. Ensure the final file has no unused imports.
- `consumerClusterName.String()` must equal what `req.ClusterName` yields inside the reconciler (both the consumer logical-cluster canonical name) so the expected provider name matches. If the assertion fails on a name mismatch, log both and reconcile: the reconciler uses `string(req.ClusterName)`; the test uses `consumerClusterName.String()` — these are the same logical cluster name from mc-runtime. Adjust if the spike shows a different accessor.
- Confirm `apiexport.New` takes `providerConfig` (workspace-scoped) not the raw `kcpConfig`, per `hack/vwspike/apiexport_test.go`.

- [ ] **Step 3: Run hermetic (SKIP) then the real e2e**

Run hermetic: `go build ./... && go vet ./... && go test ./... 2>&1 | tail` → `internal/controller` SKIPs (no `TEST_KCP_ASSETS`), everything else PASS.
Run real: `make bin/kcp` (v0.30.0), then `TEST_KCP_ASSETS=$(pwd)/bin go test ./internal/controller/ -v -timeout 360s`.
Expected: the `M2 dual-target reconcile` spec PASSES — consumer ConfigMap in consumer ws, collision-named provider ConfigMap in provider ws, status projected. If the consumer write is rejected, the permissionClaim/acceptance (Task 7) is wrong — fix the claim shape, not the assertion. Never weaken an assertion or skip around a real failure; if genuinely blocked, report BLOCKED with the kcp error.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/dualtarget_test.go
git rm internal/controller/m1_integration_test.go
git commit --no-verify -m "controller: vw-based dual-target e2e, replaces M1 direct-reconcile (M2)"
```

---

## Task 9: Remove the throwaway spike

**Files:**
- Delete: `hack/vwspike/`

- [ ] **Step 1: Remove it**

```bash
rm -rf hack/vwspike
```

(It was never committed — a plain `rm` suffices. Confirm with `git status` that nothing under `hack/vwspike` is staged.)

- [ ] **Step 2: Final verification**

Run: `go build ./... && go vet ./... && go test ./...` (hermetic; controller SKIPs) and the real e2e once more per Task 8 Step 3.
Expected: all green.

- [ ] **Step 3: Commit (if git tracked anything under hack/)**

```bash
git add -A
git commit --no-verify -m "chore: remove throwaway vw spike (M2)" || echo "nothing to commit"
```

---

## Definition of done (M2)

- `go build ./...`, `go vet ./...`, `go test ./...` green (controller SKIPs hermetically).
- `make test` / the real envtest e2e green: one `KubernetesCluster{region: eu}` created in a consumer workspace → the in-process manager reconciles it **through the APIExport virtual workspace** → a consumer-target `ConfigMap` appears in the consumer ws (claim-authorized) AND a collision-free-named provider-target `ConfigMap` appears in the provider ws; instance status projected.
- Engine core (`Reconcile`) unchanged; dual-target behavior added via `QualifyingApplier` + `ProviderChildName` + the shared `Reconciler`.
- main.go and the e2e share the one `Reconciler` (no drift).
- `hack/vwspike/` removed.
- main.go's vw fan-in is now genuinely exercised (no longer compile-only).

## Self-review notes
- The `configmaps` permissionClaim is **hand-written** (auto-derivation is M4) — this is the single claim the vw consumer write requires, not the deferred broader claims story.
- Provider child is a `ConfigMap` (self-contained); realistic fulfilment types (`AgentRequest`) need a CRD and come later.
- No cross-target CEL yet (M3): status references only the consumer child; the provider child is asserted directly by name in the e2e.
- GC/finalizers are M5 — M2 adds no cleanup; the e2e's workspaces are torn down by envtest.
