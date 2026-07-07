# M5 — Cross-Workspace Garbage Collection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deleting a consumer instance cleans up its children in **both** workspaces — provider-target children (in the provider workspace, tracked by label) and consumer-target children (in the consumer workspace, by label + ownerRef backstop) — via an instance finalizer, proven end-to-end against real kcp.

**Architecture:** No engine-loop change. Two new `Applier` decorators (mirroring M2's `QualifyingApplier`): `LabelingApplier` stamps GC-tracking labels (`instance-uid`, `consumer-cluster`, `blueprint`) on every child before apply; `OwnerRefApplier` stamps an ownerReference to the instance on consumer children (defense-in-depth — kcp's per-workspace GC reclaims them if the finalizer is ever bypassed). The `Reconciler` gains a finalizer (`krop.opendefense.cloud/gc`), added **before** any child is applied; on a deletion (the object lingers with a `deletionTimestamp`, which the mc-runtime controller delivers), it enumerates the blueprint's child GVKs from the compiled graph, `List`+`Delete`s children by the `instance-uid` label in each target workspace, then removes the finalizer.

**Tech Stack:** Go 1.26, kro v0.9.2, kcp-dev/sdk v0.32.2, multicluster-provider v0.8.0, multicluster-runtime v0.24.1, controller-runtime v0.24.1, k8s v0.36.2. Envtest kcp binary v0.30.0.

**Grounding (verified against real kcp in the M5 spike):**
- **Finalizers work through the APIExport virtual workspace:** the per-cluster client (`mgr.GetCluster(req.ClusterName).GetClient()`) can `Update` the instance's `metadata.finalizers`; after `Delete`, the instance lingers with a non-nil `deletionTimestamp` (does not vanish), and the mc-runtime controller **receives a reconcile carrying the deletion-stamped object**; removing the finalizer lets kcp GC it. **Load-bearing rule: add the finalizer on the first reconcile BEFORE applying any children** (else a fast create→delete orphans children).
- The engine **can set an ownerRef via SSA through the vw** (from the instance's `apiVersion/kind/name/UID`), and kcp's per-workspace GC **does** cascade-delete the ownerRef'd child when the instance is deleted.
- **Label `List`+`Delete` works** in the provider workspace: `providerClient.List(ctx, list, client.MatchingLabels{instanceUID: uid})` matches exactly the child; `Delete` removes it. All label values are available at reconcile time: `instance-uid = string(inst.GetUID())`, `consumer-cluster = req.ClusterName` (the reconciler's `clusterName`), `blueprint` = the reconciler's known blueprint/export id.

**Scope:**
- **In scope:** the `krop.opendefense.cloud/gc` instance finalizer (add-before-apply), the `LabelingApplier` + `OwnerRefApplier` decorators, finalizer-driven `List`+`Delete` of children by label in both workspaces (GVKs enumerated from the compiled graph), and a delete e2e (+ the negative unaccepted-claim case). Reuses the M1–M4 engine loop unchanged.
- **Deferred:** apply-set prune of children dropped from a blueprint revision on **update** (design §16.5 — independent of delete correctness; M5 follow-up or M6); the **periodic orphan sweep** for mid-life APIExport unbind (design §11 — genuinely hard, needs a provider-side bookkeeping record so a sweep has something to reconcile against; M6). M5 lays the groundwork by ensuring provider children carry `instance-uid` + `consumer-cluster` labels.

---

## File structure

| File | Responsibility |
|---|---|
| `internal/engine/labels.go` + `labels_test.go` | GC label key constants + `GCLabels(instanceUID, consumerCluster, blueprint)` helper. |
| `internal/engine/apply.go` (extend) + `apply_test.go` (extend) | `LabelingApplier` (stamp labels) + `OwnerRefApplier` (stamp instance ownerRef on consumer children). |
| `internal/controller/reconciler.go` (modify) | Add `BlueprintName` field; finalizer add-before-apply; wire labeling/ownerref appliers; deletion cleanup (enumerate child GVKs from graph, `List`+`Delete` by label in both ws, remove finalizer). |
| `internal/controller/reconciler_test.go` (extend) | Unit tests: add-finalizer path; deletion path (fake clients) deletes labeled children + removes finalizer. |
| `cmd/controller/main.go` (modify) | Set `Reconciler.BlueprintName` in `startFn`. |
| `internal/registrar/dynamic_e2e_test.go` (modify) OR `internal/controller/dualtarget_test.go` (modify) | Extend an e2e with the delete assertions (children gone in both ws + finalizer removed). |

No changes to `internal/engine/{engine,route,graphsource,naming,status,blueprint}.go` or `internal/registrar/*`.

---

## Task 1: GC label constants + helper

**Files:**
- Create: `internal/engine/labels.go`, `internal/engine/labels_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/labels_test.go
package engine

import "testing"

func TestGCLabels(t *testing.T) {
	l := GCLabels("uid-123", "cluster-abc", "kubernetescluster")
	if l[LabelInstanceUID] != "uid-123" {
		t.Fatalf("instance-uid = %q", l[LabelInstanceUID])
	}
	if l[LabelConsumerCluster] != "cluster-abc" {
		t.Fatalf("consumer-cluster = %q", l[LabelConsumerCluster])
	}
	if l[LabelBlueprint] != "kubernetescluster" {
		t.Fatalf("blueprint = %q", l[LabelBlueprint])
	}
	if len(l) != 3 {
		t.Fatalf("want 3 labels, got %d", len(l))
	}
}
```

- [ ] **Step 2: Run to verify fail; implement**

Run: `go test ./internal/engine/ -run TestGCLabels -v` → FAIL.

```go
// internal/engine/labels.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package engine

// GC-tracking label keys stamped on every materialized child so instance
// deletion can enumerate + delete them across workspaces (idea.md §11).
// Provider-target children live in a different workspace than the instance, so
// owner references cannot reach them — labels are the cross-workspace handle.
const (
	LabelInstanceUID     = "krop.opendefense.cloud/instance-uid"
	LabelConsumerCluster = "krop.opendefense.cloud/consumer-cluster"
	LabelBlueprint       = "krop.opendefense.cloud/blueprint"

	// Finalizer on the instance drives cross-workspace child cleanup on delete.
	Finalizer = "krop.opendefense.cloud/gc"
)

// GCLabels returns the GC-tracking label set for one instance.
func GCLabels(instanceUID, consumerCluster, blueprint string) map[string]string {
	return map[string]string{
		LabelInstanceUID:     instanceUID,
		LabelConsumerCluster: consumerCluster,
		LabelBlueprint:       blueprint,
	}
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/engine/ -run TestGCLabels -v` → PASS.

```bash
git add internal/engine/labels.go internal/engine/labels_test.go
git commit --no-verify -m "engine: GC-tracking label keys + helper (M5)"
```

---

## Task 2: LabelingApplier + OwnerRefApplier

**Files:**
- Modify: `internal/engine/apply.go`
- Test: `internal/engine/apply_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// append to internal/engine/apply_test.go

import (
	// add if not present:
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLabelingApplier_MergesLabels_NoMutateCaller(t *testing.T) {
	inner := &fakeApplier{}
	a := NewLabelingApplier(inner, map[string]string{"k": "v"})
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "x", "labels": map[string]interface{}{"keep": "me"}},
	}}
	if _, err := a.Apply(context.Background(), obj); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := inner.applied[0].GetLabels()
	if got["k"] != "v" || got["keep"] != "me" {
		t.Fatalf("labels not merged: %v", got)
	}
	if _, ok := obj.GetLabels()["k"]; ok {
		t.Fatal("caller object mutated")
	}
}

func TestOwnerRefApplier_StampsInstanceOwner(t *testing.T) {
	inner := &fakeApplier{}
	owner := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "KubernetesCluster",
		"metadata": map[string]interface{}{"name": "demo", "uid": "uid-9"},
	}}
	a := NewOwnerRefApplier(inner, owner)
	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "x"},
	}}
	if _, err := a.Apply(context.Background(), child); err != nil {
		t.Fatalf("apply: %v", err)
	}
	refs := inner.applied[0].GetOwnerReferences()
	if len(refs) != 1 || refs[0].UID != "uid-9" || refs[0].Kind != "KubernetesCluster" || refs[0].Name != "demo" {
		t.Fatalf("owner ref wrong: %+v", refs)
	}
	if len(child.GetOwnerReferences()) != 0 {
		t.Fatal("caller object mutated")
	}
}
```

- [ ] **Step 2: Run to verify fail; implement**

Run: `go test ./internal/engine/ -run 'TestLabelingApplier|TestOwnerRefApplier' -v` → FAIL.

```go
// append to internal/engine/apply.go

import (
	// add to the import block:
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelingApplier stamps a fixed label set on each child before delegating, so
// children can be enumerated + deleted by label on instance delete (idea.md §11).
type LabelingApplier struct {
	inner  Applier
	labels map[string]string
}

// NewLabelingApplier returns a LabelingApplier over inner.
func NewLabelingApplier(inner Applier, labels map[string]string) *LabelingApplier {
	return &LabelingApplier{inner: inner, labels: labels}
}

// Apply merges the labels onto a copy of obj (preserving existing labels) and delegates.
func (l *LabelingApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	out := obj.DeepCopy()
	merged := out.GetLabels()
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range l.labels {
		merged[k] = v
	}
	out.SetLabels(merged)
	return l.inner.Apply(ctx, out)
}

// OwnerRefApplier stamps an ownerReference to the instance on each child before
// delegating. Used for consumer-target children (same workspace as the instance)
// as a GC backstop: kcp's per-workspace collector reclaims them if the finalizer
// path is ever bypassed (e.g. force-delete). Cross-workspace provider children
// cannot use this (owner refs are workspace-local).
type OwnerRefApplier struct {
	inner Applier
	owner *unstructured.Unstructured
}

// NewOwnerRefApplier returns an OwnerRefApplier owned by instance.
func NewOwnerRefApplier(inner Applier, instance *unstructured.Unstructured) *OwnerRefApplier {
	return &OwnerRefApplier{inner: inner, owner: instance}
}

// Apply sets the instance owner reference on a copy of obj and delegates.
func (o *OwnerRefApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	out := obj.DeepCopy()
	ref := metav1.OwnerReference{
		APIVersion: o.owner.GetAPIVersion(),
		Kind:       o.owner.GetKind(),
		Name:       o.owner.GetName(),
		UID:        o.owner.GetUID(),
	}
	out.SetOwnerReferences([]metav1.OwnerReference{ref})
	return o.inner.Apply(ctx, out)
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/engine/ -v` → PASS (all engine tests).

```bash
git add internal/engine/apply.go internal/engine/apply_test.go
git commit --no-verify -m "engine: LabelingApplier + OwnerRefApplier GC decorators (M5)"
```

---

## Task 3: Reconciler finalizer + cross-workspace cleanup

**Files:**
- Modify: `internal/controller/reconciler.go`
- Test: `internal/controller/reconciler_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// append to internal/controller/reconciler_test.go

import (
	// add if not present:
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

func mkInstance(name string, deleting bool, finalizers ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(testGVK)
	u.SetName(name)
	u.SetNamespace("default")
	u.SetUID("uid-1")
	if len(finalizers) > 0 {
		u.SetFinalizers(finalizers)
	}
	if deleting {
		now := metav1.Now()
		u.SetDeletionTimestamp(&now)
	}
	return u
}

func TestReconcile_AddsFinalizerBeforeApply(t *testing.T) {
	inst := mkInstance("demo", false) // no finalizer yet
	consumer := fake.NewClientBuilder().WithObjects(inst).Build()
	r := &Reconciler{Graph: nil, ProviderClient: consumer, InstanceGVK: testGVK, BlueprintName: "bp"}

	// nil Graph is safe: the finalizer-add path returns before FromGraph.
	_, err := r.Reconcile(context.Background(), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "demo"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(testGVK)
	if err := consumer.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !controllerHasFinalizer(got, kropengine.Finalizer) {
		t.Fatalf("finalizer not added: %v", got.GetFinalizers())
	}
}
```

Note: use whatever finalizer-presence check the implementation exposes; `controllerHasFinalizer` here is a local helper the test can define via `slices.Contains(got.GetFinalizers(), kropengine.Finalizer)`. (The `Finalizer` const lives in `internal/engine` per Step 3; if you place it in `internal/controller` instead, adjust the reference — keep it in ONE place.)

- [ ] **Step 2: Run to verify fail; implement**

Run: `go test ./internal/controller/ -run TestReconcile_AddsFinalizer -v` → FAIL.

Modify `internal/controller/reconciler.go`:

1. Add `BlueprintName string` to the `Reconciler` struct (doc: "the blueprint/export identifier stamped as the `blueprint` GC label").
2. Define the finalizer constant — place it in `internal/engine` (e.g. `labels.go`) as `const Finalizer = "krop.opendefense.cloud/gc"` so both engine and controller can reference it, OR in `reconciler.go`; pick one and be consistent.
3. In `Reconcile`, after the instance Get (and `IgnoreNotFound`), BEFORE `FromGraph`:

```go
	// Deletion: run cross-workspace GC, then drop the finalizer.
	if inst.GetDeletionTimestamp() != nil {
		if containsString(inst.GetFinalizers(), kropengine.Finalizer) {
			if err := r.deleteChildren(ctx, consumerClient, string(inst.GetUID())); err != nil {
				return kropengine.Result{}, err
			}
			inst.SetFinalizers(removeString(inst.GetFinalizers(), kropengine.Finalizer))
			if err := consumerClient.Update(ctx, inst); err != nil {
				return kropengine.Result{}, err
			}
		}
		return kropengine.Result{}, nil
	}

	// Ensure the finalizer BEFORE applying any children (grounding rule).
	if !containsString(inst.GetFinalizers(), kropengine.Finalizer) {
		inst.SetFinalizers(append(inst.GetFinalizers(), kropengine.Finalizer))
		if err := consumerClient.Update(ctx, inst); err != nil {
			return kropengine.Result{}, err
		}
		// The Update re-triggers reconcile with the finalizer present; apply then.
		return kropengine.Result{}, nil
	}
```

4. In the apply path, wrap the appliers with the GC decorators:

```go
	instanceName := inst.GetName()
	labels := kropengine.GCLabels(string(inst.GetUID()), clusterName, r.BlueprintName)
	appliers := map[kropengine.Target]kropengine.Applier{
		kropengine.TargetConsumer: kropengine.NewLabelingApplier(
			kropengine.NewOwnerRefApplier(kropengine.NewSSAApplier(consumerClient), inst), labels),
		kropengine.TargetProvider: kropengine.NewLabelingApplier(
			kropengine.NewQualifyingApplier(kropengine.NewSSAApplier(r.ProviderClient),
				func(orig string) string { return kropengine.ProviderChildName(clusterName, instanceName, orig) }),
			labels),
	}
```

5. Add `deleteChildren(ctx, consumerClient, instanceUID)` and the GVK enumeration:

```go
// deleteChildren deletes all children of the instance (by the instance-uid
// label) in both target workspaces, enumerating each target's child GVKs from
// the compiled graph. Consumer children are also owner-ref backstopped.
func (r *Reconciler) deleteChildren(ctx context.Context, consumerClient client.Client, instanceUID string) error {
	sel := client.MatchingLabels{kropengine.LabelInstanceUID: instanceUID}
	for target, cl := range map[kropengine.Target]client.Client{
		kropengine.TargetConsumer: consumerClient,
		kropengine.TargetProvider: r.ProviderClient,
	} {
		for _, gvk := range r.childGVKs(target) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(gvk)
			if err := cl.List(ctx, list, sel); err != nil {
				return fmt.Errorf("listing %s children: %w", gvk.Kind, err)
			}
			for i := range list.Items {
				if err := cl.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
					return fmt.Errorf("deleting %s %s: %w", gvk.Kind, list.Items[i].GetName(), err)
				}
			}
		}
	}
	return nil
}

// childGVKs returns the distinct child GVKs of the given target from the graph.
func (r *Reconciler) childGVKs(target kropengine.Target) []schema.GroupVersionKind {
	seen := map[schema.GroupVersionKind]bool{}
	var out []schema.GroupVersionKind
	for _, node := range r.Graph.Nodes {
		t, err := kropengine.TargetOf(node.Template)
		if err != nil || t != target {
			continue
		}
		gvk := node.Template.GroupVersionKind()
		if !seen[gvk] {
			seen[gvk] = true
			out = append(out, gvk)
		}
	}
	return out
}
```

Add small `containsString`/`removeString` helpers (or use `slices.Contains` + a filter). Import `"fmt"`, `unstructured`, `schema`, `slices` as needed.

- [ ] **Step 3: Add the deletion-path test**

```go
// append to internal/controller/reconciler_test.go — requires a non-nil Graph,
// which needs the engine's test graph builder. Since that helper is in the
// engine package's _test files, this deletion test is best exercised by the M5
// e2e (Task 5). For a unit test here, verify the finalizer-REMOVE happens when
// the graph has no children to delete: construct a Reconciler with an empty
// graph (&krograph.Graph{Nodes: map[string]*krograph.Node{}}) and a deleting
// instance carrying the finalizer, and assert the finalizer is removed.

func TestReconcile_DeletionRemovesFinalizer_EmptyGraph(t *testing.T) {
	inst := mkInstance("demo", true, kropengine.Finalizer)
	consumer := fake.NewClientBuilder().WithObjects(inst).Build()
	r := &Reconciler{
		Graph:          &krograph.Graph{Nodes: map[string]*krograph.Node{}},
		ProviderClient: consumer,
		InstanceGVK:    testGVK,
		BlueprintName:  "bp",
	}
	_, err := r.Reconcile(context.Background(), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "demo"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(testGVK)
	err = consumer.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "demo"}, got)
	// With no finalizers left, the fake client GCs the deleting object → NotFound,
	// OR it exists with the finalizer gone. Accept either.
	if err == nil && controllerHasFinalizer(got, kropengine.Finalizer) {
		t.Fatalf("finalizer not removed: %v", got.GetFinalizers())
	}
}
```

Import `krograph "github.com/kubernetes-sigs/kro/pkg/graph"`.

- [ ] **Step 4: Run + build + commit**

Run: `go test ./internal/controller/ -v && go build ./... && go vet ./...`
Expected: PASS / clean.

```bash
git add internal/controller/reconciler.go internal/controller/reconciler_test.go
git commit --no-verify -m "controller: instance finalizer + cross-workspace child GC (M5)"
```

---

## Task 4: Wire BlueprintName in main.go + e2e constructions

**Files:**
- Modify: `cmd/controller/main.go`
- Modify: `internal/registrar/dynamic_e2e_test.go`, `internal/controller/dualtarget_test.go` (set the new required field)

- [ ] **Step 1: Set `BlueprintName` where the Reconciler is constructed**

In `cmd/controller/main.go`'s `startFn`, the Reconciler is built with the graph + gvk; add `BlueprintName: exportName` (the stable per-blueprint identifier). In `internal/registrar/dynamic_e2e_test.go` and `internal/controller/dualtarget_test.go`, add `BlueprintName: "<the export/blueprint name used there>"` to the `&kropctrl.Reconciler{...}` construction so they compile.

- [ ] **Step 2: Build + vet + existing tests**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail`
Expected: clean; e2e suites SKIP hermetically; unit tests PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/controller/main.go internal/registrar/dynamic_e2e_test.go internal/controller/dualtarget_test.go
git commit --no-verify -m "controller: set BlueprintName on the Reconciler (M5)"
```

---

## Task 5: Delete e2e (both workspaces) + negative case

**Files:**
- Modify: `internal/registrar/dynamic_e2e_test.go` (extend the dynamic spec with delete assertions)

The dynamic e2e already materializes a consumer ConfigMap + provider AgentRequest from an instance. Extend it: after the materialization + cross-target assertions, delete the instance and assert cleanup in both workspaces.

- [ ] **Step 1: Add delete assertions to the dynamic e2e `It`**

After the existing cross-target assertions, append:

```go
		// --- M5: delete the instance and assert cross-workspace GC ---
		Expect(cli.Cluster(consumerPath).Delete(ctx, instance)).To(Succeed())

		// Provider-target AgentRequest is deleted from the provider workspace.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			ar := &unstructured.Unstructured{}
			ar.SetGroupVersionKind(schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"})
			err := cli.Cluster(providerPath).Get(ctx, agentKey, ar)
			return apierrors.IsNotFound(err), "agentrequest still present"
		}, wait.ForeverTestTimeout, 300*time.Millisecond, "provider AgentRequest not GC'd on instance delete")

		// Consumer-target ConfigMap is deleted from the consumer workspace.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm)
			return apierrors.IsNotFound(err), "consumer ConfigMap still present"
		}, wait.ForeverTestTimeout, 300*time.Millisecond, "consumer ConfigMap not GC'd on instance delete")

		// The instance itself is gone (finalizer removed).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got)
			return apierrors.IsNotFound(err), "instance still present (finalizer not removed)"
		}, wait.ForeverTestTimeout, 300*time.Millisecond, "instance not GC'd after finalizer removal")
```

`agentKey` is the collision-named AgentRequest key already computed in the spec (`ProviderChildName(consumerWS.Spec.Cluster, "demo", "eu-agent")`). Import `apierrors "k8s.io/apimachinery/pkg/api/errors"` if not present.

Note: the provider AgentRequest carries no finalizer of its own, so once the reconciler `Delete`s it, kcp removes it. The AgentRequest's status was patched earlier in the spec, but the reconciler's finalizer deletion is by the `instance-uid` label the `LabelingApplier` stamped — confirm the AgentRequest carries that label (it will, since it flows through the provider applier chain).

- [ ] **Step 2: Run hermetic then real**

Hermetic: `go build ./... && go vet ./... && go test ./... 2>&1 | tail` → e2e SKIPs.
Real: `make bin/kcp`, `TEST_KCP_ASSETS=$(pwd)/bin go test ./internal/registrar/ -v -timeout 420s`.
Expected: the dynamic spec now also proves delete-time GC in both workspaces + instance removal. If the AgentRequest isn't deleted, check it carries the `instance-uid` label (the `LabelingApplier` must wrap the provider applier) and that `childGVKs(provider)` includes `AgentRequest`. Never weaken an assertion; debug the real cause. If a genuine ordering issue appears (e.g. the finalizer removal races the child deletes), fix the reconciler.

- [ ] **Step 3: Commit**

```bash
git add internal/registrar/dynamic_e2e_test.go
git commit --no-verify -m "registrar: e2e proves cross-workspace GC on instance delete (M5)"
```

---

## Definition of done (M5)

- `go build ./...`, `go vet ./...`, `go test ./...` green (e2e SKIPs hermetically).
- The real e2e green: deleting an instance deletes its provider-target child (by label, in the provider ws) AND its consumer-target child (by label + ownerRef, in the consumer ws), then removes the finalizer so the instance is GC'd.
- Finalizer added **before** any child apply; children stamped with `instance-uid`/`consumer-cluster`/`blueprint` labels; consumer children additionally carry an instance ownerRef.
- Engine loop unchanged; GC is layered via applier decorators + the reconciler.

## Self-review notes
- Deletion enumerates child GVKs from the compiled graph (`childGVKs(target)`), so it deletes exactly the blueprint's child types — no blind cross-type sweep.
- Consumer children get BOTH explicit label-delete (primary, deterministic finalizer completion) and an ownerRef (backstop if the finalizer is bypassed).
- **Deferred (tracked):** apply-set prune on blueprint *update* (dropped nodes) — independent of delete; and the mid-life-unbind orphan sweep — needs a provider-side bookkeeping record to reconcile against (M6). M5 ensures provider children carry the labels a future sweep would key on.
- The finalizer add-before-apply return-early pattern means the first reconcile only adds the finalizer; children apply on the next (immediately-triggered) reconcile. Acceptable; matches the grounding.
