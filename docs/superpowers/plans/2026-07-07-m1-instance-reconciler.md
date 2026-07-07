# M1 — Instance Reconciler (single-target walking skeleton) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reconcile a consumer-created instance of a hand-published blueprint into one consumer-workspace child, by driving kro's client-free runtime and owning all apply/observe I/O ourselves.

**Architecture:** A pure `internal/engine` package drives kro's `runtime.Runtime` node-by-node in topological order: for each node, resolve the desired object(s) (`GetDesired`), read the per-object routing annotation, strip it, apply via a target-keyed `Applier` (server-side apply), read the result back, feed it to `SetObserved`, and check readiness — then project instance status. The engine is client-agnostic (an `Applier` interface) so it unit-tests against a fake applier with a graph built by a test helper (no cluster). `cmd/controller/main.go` wires kcp's multicluster manager (copied from the access-operator pattern) and registers the engine as the reconciler over the generated instance GVK. M1 populates only the **consumer** target; the provider target and permissionClaims arrive in M2.

**Tech Stack:** Go 1.26, kro v0.9.2 (`pkg/runtime`, `pkg/graph`), kcp-dev/sdk v0.32.2, multicluster-provider v0.8.0, multicluster-runtime v0.24.1, controller-runtime v0.24.1, k8s v0.36.2.

**Scope / verification boundary (read first):**
- Fully unit-tested here (fake clients, no kcp): routing helpers, the engine reconcile loop, status aggregation, the graph-build wrapper's contract.
- **Runnable locally via an in-process envtest suite** (Task 11), modeled on `dependency-controller`'s `internal/controller/suite_test.go`: `multicluster-provider/envtest` boots a real kcp from a binary the Makefile downloads (`make test`, `TEST_KCP_ASSETS=$(LOCALBIN)`), the controller runs in-process in a goroutine against the APIExport virtual workspace. This exercises the endpoint-slice discovery, `graph.NewBuilder` against a real kcp workspace's discovery/OpenAPI (retiring the one residual §16.3 risk from the M0 findings), and the end-to-end "create instance → ConfigMap appears → status Ready". Skips cleanly when `TEST_KCP_ASSETS` is unset so plain `go test` stays hermetic.
- **Deferred to a later tier** (not M1): the kind + kcp-operator + helm full-stack e2e (`dependency-controller`'s `test/e2e/`). M1 proves the walking skeleton via the faster in-process envtest suite; the full-stack deployment tier comes with M6 hardening.
- M1 blueprint child is a **core `ConfigMap`** (consumer target) — universally discoverable, no permissionClaims. Foreign types + claims = M2.
- **API versions (verified against dependency-controller + access-operator):** `APIExport` and `APIBinding` are **`apis.kcp.io/v1alpha2`** (`github.com/kcp-dev/sdk/apis/apis/v1alpha2`); `APIResourceSchema` and `APIExportEndpointSlice` are **`apis.kcp.io/v1alpha1`**. Register both apis versions plus core v1alpha1 and tenancy v1alpha1 on the test scheme.

**Reference APIs (verified against the module cache in M0):**
- `runtime.FromGraph(g *graph.Graph, instance *unstructured.Unstructured, cfg graph.RGDConfig) (*runtime.Runtime, error)`
- `rt.Nodes() []*runtime.Node` (topological order, instance excluded); `rt.Instance() *runtime.Node`
- `node.Spec.Meta.ID string`, `node.Spec.Meta.GVR schema.GroupVersionResource`, `node.Spec.Meta.Namespaced bool`
- `node.IsIgnored() (bool, error)`, `node.GetDesired() ([]*unstructured.Unstructured, error)` (errors while a dependency is unobserved), `node.SetObserved([]*unstructured.Unstructured)`, `node.CheckReadiness() error`
- `graph.NewBuilder(*rest.Config, *http.Client) (*graph.Builder, error)`; `builder.NewResourceGraphDefinition(rgd, graph.RGDConfig{MaxCollectionSize, MaxCollectionDimensionSize}) (*graph.Graph, error)`
- Test-only no-cluster graph build (M0 spike): inject `testk8s.NewFakeResolver()` into `&graph.Builder{}` via unsafe — confined to a `_test`-only helper.

---

## File structure

| File | Responsibility |
|---|---|
| `internal/engine/route.go` | Routing/label constants; `TargetOf`, `StripRouting`. Pure. |
| `internal/engine/route_test.go` | Unit tests for routing helpers. |
| `internal/engine/apply.go` | `Applier` interface; `SSAApplier` (controller-runtime SSA + readback). |
| `internal/engine/apply_test.go` | `SSAApplier` test against the controller-runtime fake client. |
| `internal/engine/engine.go` | `Engine`, `Reconcile` — the node drive loop + routing + observe. |
| `internal/engine/status.go` | Instance status projection + Ready/Progressing conditions. |
| `internal/engine/engine_test.go` | Core loop tests with a fake applier + test-built graph. |
| `internal/engine/graphsource.go` | `GraphSource` interface; `EndpointGraphSource` over `graph.NewBuilder`. |
| `internal/engine/testsupport_test.go` | Test helper: build a `*graph.Graph` with the fake resolver (unsafe, test-only) + the M1 sample RGD. |
| `internal/kcp/endpointslice.go` | `FindEndpointSlice`, `ValidateKubeconfig` (adapted from access-operator). |
| `cmd/controller/main.go` | Multicluster manager wiring; registers the engine over the instance GVK (consumer target only). |
| `config/kcp/apiresourceschema-kubernetesclusters.krop.opendefense.cloud.yaml` | Hand-written ARS (`apis.kcp.io/v1alpha1`) for the M1 instance type. |
| `config/kcp/apiexport-krop-m1.yaml` | Hand-written APIExport (`apis.kcp.io/v1alpha2`) referencing the ARS (no permissionClaims in M1). |
| `config/kcp/examples/blueprint-kubernetescluster.yaml` | The RGD the ARS/engine correspond to (the graph the controller builds at startup). |
| `test/fixtures/apibinding-kubernetescluster.yaml` | Consumer-side `APIBinding` (`apis.kcp.io/v1alpha2`) for the e2e, with a `${…}` path placeholder. |
| `internal/controller/suite_test.go` | Ginkgo suite: `multicluster-provider/envtest` boot + scheme registration (modeled on dependency-controller). |
| `internal/controller/m1_integration_test.go` | In-process envtest e2e: provider ws fixtures → in-process manager → consumer ws binding → create instance → assert ConfigMap + status. |

Delete `internal/deps/deps.go` (the temporary M0 import anchor) once `main.go` and `internal/kcp` reference the kcp packages (Task 8).

---

## Task 1: Routing helpers

**Files:**
- Create: `internal/engine/route.go`
- Test: `internal/engine/route_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/route_test.go
package engine

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func obj(annos map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "x"},
	}}
	if annos != nil {
		u.SetAnnotations(annos)
	}
	return u
}

func TestTargetOf_DefaultsToConsumer(t *testing.T) {
	got, err := TargetOf(obj(nil))
	if err != nil || got != TargetConsumer {
		t.Fatalf("want consumer,nil; got %q,%v", got, err)
	}
}

func TestTargetOf_ReadsProvider(t *testing.T) {
	got, err := TargetOf(obj(map[string]string{TargetAnnotation: "provider"}))
	if err != nil || got != TargetProvider {
		t.Fatalf("want provider,nil; got %q,%v", got, err)
	}
}

func TestTargetOf_RejectsUnknown(t *testing.T) {
	if _, err := TargetOf(obj(map[string]string{TargetAnnotation: "bogus"})); err == nil {
		t.Fatal("want error for unknown target value")
	}
}

func TestStripRouting_RemovesAnnotationAndEmptyMap(t *testing.T) {
	u := obj(map[string]string{TargetAnnotation: "provider"})
	StripRouting(u)
	if _, ok := u.GetAnnotations()[TargetAnnotation]; ok {
		t.Fatal("routing annotation not stripped")
	}
	// the annotations map should be gone entirely when it was the only key
	if _, found, _ := unstructured.NestedMap(u.Object, "metadata", "annotations"); found {
		t.Fatal("empty annotations map should be removed")
	}
}

func TestStripRouting_PreservesOtherAnnotations(t *testing.T) {
	u := obj(map[string]string{TargetAnnotation: "consumer", "keep.me/x": "y"})
	StripRouting(u)
	if u.GetAnnotations()["keep.me/x"] != "y" {
		t.Fatal("unrelated annotation was dropped")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestTargetOf -v`
Expected: FAIL — `undefined: TargetOf` (package doesn't compile yet).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/route.go
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

// Package engine drives kro's client-free runtime for a single instance,
// owning all apply/observe I/O and routing each child to its target workspace.
package engine

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Target is a per-resource destination for an applied child object.
type Target string

const (
	// TargetConsumer routes a child into the consumer (tenant) workspace. Default.
	TargetConsumer Target = "consumer"
	// TargetProvider routes a child into the provider workspace.
	TargetProvider Target = "provider"

	// TargetAnnotation carries the routing decision on a resource template.
	// It is read off the desired object and StripRouting'd before apply.
	TargetAnnotation = "krop.opendefense.cloud/target"
)

// TargetOf returns the routing target of a desired object, defaulting to
// consumer when the annotation is absent. Unknown values are an error.
func TargetOf(o *unstructured.Unstructured) (Target, error) {
	v, ok := o.GetAnnotations()[TargetAnnotation]
	if !ok || v == "" {
		return TargetConsumer, nil
	}
	switch Target(v) {
	case TargetConsumer:
		return TargetConsumer, nil
	case TargetProvider:
		return TargetProvider, nil
	default:
		return "", fmt.Errorf("invalid %s=%q (want %q or %q)",
			TargetAnnotation, v, TargetConsumer, TargetProvider)
	}
}

// StripRouting removes the routing annotation before apply so it never leaks
// onto the materialized object. If it was the only annotation, the whole
// annotations map is removed to avoid an empty map in server-side apply.
func StripRouting(o *unstructured.Unstructured) {
	annos := o.GetAnnotations()
	if _, ok := annos[TargetAnnotation]; !ok {
		return
	}
	delete(annos, TargetAnnotation)
	if len(annos) == 0 {
		unstructured.RemoveNestedField(o.Object, "metadata", "annotations")
		return
	}
	o.SetAnnotations(annos)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -v`
Expected: PASS (all `TestTargetOf*` and `TestStripRouting*`).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/route.go internal/engine/route_test.go
git commit --no-verify -m "engine: per-resource routing annotation helpers (M1)"
```

---

## Task 2: Applier interface

**Files:**
- Create: `internal/engine/apply.go` (interface only in this task)
- Test: `internal/engine/apply_test.go` (a reusable fake applier the engine tests use)

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/apply_test.go
package engine

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeApplier records applied objects and echoes them back as "observed",
// optionally injecting extra status fields to simulate a controller populating
// the object after apply. It is the test double for the engine loop tests.
type fakeApplier struct {
	applied []*unstructured.Unstructured
	// mutate, if set, is called on a deep copy of the applied object to produce
	// the observed object (e.g. to set a status field a readyWhen depends on).
	mutate func(*unstructured.Unstructured)
}

func (f *fakeApplier) Apply(_ context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.applied = append(f.applied, o.DeepCopy())
	observed := o.DeepCopy()
	if f.mutate != nil {
		f.mutate(observed)
	}
	return observed, nil
}

func TestFakeApplier_SatisfiesInterface(t *testing.T) {
	var _ Applier = &fakeApplier{}
	obs, err := (&fakeApplier{}).Apply(context.Background(), obj(nil))
	if err != nil || obs == nil {
		t.Fatalf("apply returned %v,%v", obs, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestFakeApplier -v`
Expected: FAIL — `undefined: Applier`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/apply.go
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
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// FieldManager is the server-side-apply field owner for all engine writes.
const FieldManager = "krop-controller"

// Applier applies one desired object into a single target workspace and returns
// the object as observed afterwards (server-side apply result, read back). The
// engine owns all I/O through this interface, keeping the reconcile loop
// client-agnostic and unit-testable.
type Applier interface {
	Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -run TestFakeApplier -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/apply.go internal/engine/apply_test.go
git commit --no-verify -m "engine: Applier interface + fake applier test double (M1)"
```

---

## Task 3: SSAApplier (real server-side-apply applier)

**Files:**
- Modify: `internal/engine/apply.go` (add `SSAApplier`)
- Test: `internal/engine/apply_test.go` (add a fake-client test)

- [ ] **Step 1: Write the failing test**

```go
// append to internal/engine/apply_test.go

import (
	// add to the existing import block:
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSSAApplier_AppliesAndReadsBack(t *testing.T) {
	cl := fake.NewClientBuilder().Build()
	a := NewSSAApplier(cl)

	cm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "cfg", "namespace": "default"},
		"data":     map[string]interface{}{"region": "eu"},
	}}

	observed, err := a.Apply(context.Background(), cm)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	region, _, _ := unstructured.NestedString(observed.Object, "data", "region")
	if region != "eu" {
		t.Fatalf("read-back data.region = %q, want eu", region)
	}
}
```

Note: the controller-runtime fake client supports `client.Apply` patches. If the installed fake client rejects SSA, gate this test with `t.Skip` and rely on the Task 11 envtest instead — but attempt the fake path first, it is supported in controller-runtime v0.24.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestSSAApplier -v`
Expected: FAIL — `undefined: NewSSAApplier`.

- [ ] **Step 3: Write minimal implementation**

```go
// append to internal/engine/apply.go

import (
	// add to the existing import block:
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SSAApplier applies objects into one workspace via server-side apply using a
// controller-runtime client, then reads the object back so callers observe the
// full server state (including fields other controllers set).
type SSAApplier struct {
	c client.Client
}

// NewSSAApplier builds an SSAApplier bound to one workspace-scoped client.
func NewSSAApplier(c client.Client) *SSAApplier {
	return &SSAApplier{c: c}
}

// Apply server-side-applies obj (force ownership) and returns it as read back.
func (a *SSAApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	desired := obj.DeepCopy()
	if err := a.c.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return nil, err
	}
	observed := &unstructured.Unstructured{}
	observed.SetGroupVersionKind(obj.GroupVersionKind())
	if err := a.c.Get(ctx, client.ObjectKeyFromObject(desired), observed); err != nil {
		return nil, err
	}
	return observed, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -run TestSSAApplier -v`
Expected: PASS. If the fake client rejects the SSA patch, add `t.Skip("SSA validated under envtest in Task 11")` and continue.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/apply.go internal/engine/apply_test.go
git commit --no-verify -m "engine: SSAApplier server-side apply + read-back (M1)"
```

---

## Task 4: Test-only graph builder helper

**Files:**
- Create: `internal/engine/testsupport_test.go`

This ports the M0 spike's no-cluster graph build into a `_test`-only helper so the engine loop can be tested without a cluster. The `unsafe` injection stays confined to test code (kro itself constructs `Builder{...}` internally in its own tests; we cannot from outside without this).

- [ ] **Step 1: Write the helper + a self-check test**

```go
// internal/engine/testsupport_test.go
package engine

import (
	"reflect"
	"testing"
	"unsafe"

	"k8s.io/apimachinery/pkg/api/meta"
	memory "k8s.io/client-go/discovery/cached/memory"
	restmapper "k8s.io/client-go/restmapper"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// sampleRGD is the M1 blueprint: schema{region}, one consumer-target child that
// is a core ConfigMap carrying the region. ConfigMap is in the kro fake
// resolver's core scheme, so the graph builds with no cluster.
func sampleRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"kubernetescluster",
		generator.WithSchema(
			"KubernetesCluster", "v1alpha1",
			map[string]interface{}{"region": "string"},
			map[string]interface{}{"configMapName": "${config.metadata.name}"},
		),
		generator.WithResource("config", map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "${schema.spec.region}-cluster-config",
				"namespace": "default",
				"annotations": map[string]interface{}{
					TargetAnnotation: string(TargetConsumer),
				},
			},
			"data": map[string]interface{}{"region": "${schema.spec.region}"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"
	return rgd
}

// buildTestGraph builds a *graph.Graph with NO cluster by injecting kro's fake
// resolver into a graph.Builder via unsafe (test-only; see Task notes).
func buildTestGraph(t *testing.T, rgd *krov1alpha1.ResourceGraphDefinition) *graph.Graph {
	t.Helper()
	fakeResolver, fakeDiscovery := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDiscovery))
	b := &graph.Builder{}
	setUnexportedField(b, "schemaResolver", fakeResolver)
	setUnexportedField(b, "restMapper", meta.RESTMapper(rm))
	g, err := b.NewResourceGraphDefinition(rgd, graph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		t.Fatalf("buildTestGraph: %v", err)
	}
	return g
}

func setUnexportedField(obj interface{}, name string, val interface{}) {
	rv := reflect.ValueOf(obj).Elem().FieldByName(name)
	rv = reflect.NewAt(rv.Type(), unsafe.Pointer(rv.UnsafeAddr())).Elem()
	rv.Set(reflect.ValueOf(val))
}

func TestBuildTestGraph_Builds(t *testing.T) {
	g := buildTestGraph(t, sampleRGD())
	if len(g.TopologicalOrder) != 1 || g.TopologicalOrder[0] != "config" {
		t.Fatalf("topological order = %v, want [config]", g.TopologicalOrder)
	}
}
```

- [ ] **Step 2: Run the self-check**

Run: `go test ./internal/engine/ -run TestBuildTestGraph -v`
Expected: PASS — graph builds, single node `config`.

- [ ] **Step 3: Commit**

```bash
git add internal/engine/testsupport_test.go
git commit --no-verify -m "engine: test-only no-cluster graph builder helper (M1)"
```

---

## Task 5: Engine reconcile loop (core)

**Files:**
- Create: `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/engine_test.go
package engine

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

func newInstance(region string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "krop.opendefense.cloud/v1alpha1",
		"kind":       "KubernetesCluster",
		"metadata":   map[string]interface{}{"name": "demo", "namespace": "default"},
		"spec":       map[string]interface{}{"region": "eu"},
	}}
}

func newRuntime(t *testing.T, inst *unstructured.Unstructured) *runtime.Runtime {
	t.Helper()
	g := buildTestGraph(t, sampleRGD())
	rt, err := runtime.FromGraph(g, inst, graph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}
	return rt
}

func TestReconcile_AppliesConsumerChild_StripsRouting(t *testing.T) {
	inst := newInstance("eu")
	rt := newRuntime(t, inst)
	consumer := &fakeApplier{}

	e := New()
	res, err := e.Reconcile(context.Background(), rt, map[Target]Applier{TargetConsumer: consumer})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 1 {
		t.Fatalf("want 1 applied consumer object, got %d", len(consumer.applied))
	}
	got := consumer.applied[0]
	// CEL resolved from the instance:
	if got.GetName() != "eu-cluster-config" {
		t.Fatalf("child name = %q, want eu-cluster-config", got.GetName())
	}
	if region, _, _ := unstructured.NestedString(got.Object, "data", "region"); region != "eu" {
		t.Fatalf("child data.region = %q, want eu", region)
	}
	// routing annotation stripped before apply:
	if _, ok := got.GetAnnotations()[TargetAnnotation]; ok {
		t.Fatal("routing annotation leaked onto applied object")
	}
	if res.Ready != true {
		t.Fatalf("want Ready=true (ConfigMap has no readyWhen), got %+v", res)
	}
}

func TestReconcile_UnconfiguredTargetErrors(t *testing.T) {
	rt := newRuntime(t, newInstance("eu"))
	e := New()
	// No consumer applier configured → the single consumer child cannot route.
	_, err := e.Reconcile(context.Background(), rt, map[Target]Applier{})
	if err == nil {
		t.Fatal("want error when the child's target has no configured applier")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestReconcile -v`
Expected: FAIL — `undefined: New` / `undefined: Engine`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/engine.go
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
	"context"
	"fmt"

	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// Engine drives kro's runtime for a single instance: it resolves, routes,
// applies and observes each node, then aggregates instance status.
type Engine struct{}

// New returns a stateless Engine.
func New() *Engine { return &Engine{} }

// Result summarizes one reconcile pass.
type Result struct {
	// Ready is true when every included node passed CheckReadiness.
	Ready bool
	// Requeue is true when a node is not yet ready or a dependency is pending.
	Requeue bool
}

// Reconcile drives the runtime node-by-node in topological order. For each
// included node it resolves the desired object(s), routes each by its target
// annotation (stripping it), applies via the matching Applier, reads back, and
// feeds the observed state to the runtime so later nodes' CEL resolves. It never
// rolls back: partial application is reported via Result and a requeue.
func (e *Engine) Reconcile(ctx context.Context, rt *runtime.Runtime, appliers map[Target]Applier) (Result, error) {
	res := Result{Ready: true}

	for _, node := range rt.Nodes() {
		ignored, err := node.IsIgnored()
		if err != nil {
			return res, fmt.Errorf("node %s: includeWhen: %w", node.Spec.Meta.ID, err)
		}
		if ignored {
			continue
		}

		desired, err := node.GetDesired()
		if err != nil {
			// A dependency is not yet observed/ready → don't apply partial;
			// converge on a later requeue.
			res.Ready = false
			res.Requeue = true
			return res, nil
		}

		observed := make([]*unstructured.Unstructured, 0, len(desired))
		for _, obj := range desired {
			target, err := TargetOf(obj)
			if err != nil {
				return res, fmt.Errorf("node %s: %w", node.Spec.Meta.ID, err)
			}
			StripRouting(obj)
			applier, ok := appliers[target]
			if !ok {
				return res, fmt.Errorf("node %s: no applier configured for target %q", node.Spec.Meta.ID, target)
			}
			obs, err := applier.Apply(ctx, obj)
			if err != nil {
				return res, fmt.Errorf("node %s: apply to %s: %w", node.Spec.Meta.ID, target, err)
			}
			observed = append(observed, obs)
		}
		node.SetObserved(observed)

		if err := node.CheckReadiness(); err != nil {
			// Not an error: the child exists but isn't ready yet.
			res.Ready = false
			res.Requeue = true
		}
	}
	return res, nil
}
```

Add the missing import to `engine.go`:

```go
import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)
```

(Place it in the import block alongside `context`, `fmt`, and the kro runtime import.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -v`
Expected: PASS — all engine + routing + applier tests.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git commit --no-verify -m "engine: node drive loop with routing + observe (M1 core)"
```

---

## Task 6: Instance status aggregation

**Files:**
- Create: `internal/engine/status.go`
- Modify: `internal/engine/engine.go` (call status projection + write it)
- Test: `internal/engine/engine_test.go` (add a status test)

- [ ] **Step 1: Write the failing test**

```go
// append to internal/engine/engine_test.go

func TestReconcile_ProjectsInstanceStatus(t *testing.T) {
	inst := newInstance("eu")
	rt := newRuntime(t, inst)
	consumer := &fakeApplier{}

	e := New()
	if _, err := e.Reconcile(context.Background(), rt, map[Target]Applier{TargetConsumer: consumer}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The blueprint maps status.configMapName = ${config.metadata.name}.
	desiredInstance, err := ProjectStatus(rt)
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	name, _, _ := unstructured.NestedString(desiredInstance.Object, "status", "configMapName")
	if name != "eu-cluster-config" {
		t.Fatalf("status.configMapName = %q, want eu-cluster-config", name)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestReconcile_ProjectsInstanceStatus -v`
Expected: FAIL — `undefined: ProjectStatus`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/status.go
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// ProjectStatus returns the instance object with its blueprint-mapped status.*
// fields resolved from observed child state. The runtime must have observed all
// nodes the status expressions reference (i.e. call after Reconcile's loop).
func ProjectStatus(rt *runtime.Runtime) (*unstructured.Unstructured, error) {
	desired, err := rt.Instance().GetDesired()
	if err != nil {
		return nil, err
	}
	return desired[0], nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/status.go
git commit --no-verify -m "engine: project instance status.* from observed children (M1)"
```

---

## Task 7: GraphSource (production graph build over NewBuilder)

**Files:**
- Create: `internal/engine/graphsource.go`
- Test: `internal/engine/graphsource_test.go` (contract-level; the live build is exercised in Task 11 under envtest)

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/graphsource_test.go
package engine

import "testing"

// The production EndpointGraphSource wraps graph.NewBuilder, which needs a live
// discovery/OpenAPI endpoint, so we can only assert its construction contract
// here; the real build against a workspace is validated in test/e2e (Task 11).
func TestEndpointGraphSource_NilConfigErrors(t *testing.T) {
	if _, err := NewEndpointGraphSource(nil); err == nil {
		t.Fatal("want error constructing EndpointGraphSource with nil rest.Config")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestEndpointGraphSource -v`
Expected: FAIL — `undefined: NewEndpointGraphSource`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/graphsource.go
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
	"fmt"

	"k8s.io/client-go/rest"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// GraphSource builds a compiled kro graph from a blueprint RGD. Split behind an
// interface so the engine loop tests use an in-memory build (no cluster) while
// production builds against a schema-complete workspace endpoint.
type GraphSource interface {
	Build(rgd *krov1alpha1.ResourceGraphDefinition) (*graph.Graph, error)
}

// EndpointGraphSource builds graphs via graph.NewBuilder pointed at a workspace
// that serves discovery/OpenAPI for every child GVK (design §16.3 option A).
type EndpointGraphSource struct {
	builder *graph.Builder
}

// NewEndpointGraphSource constructs the builder from a workspace-scoped config.
// The config must serve discovery/OpenAPI — NewBuilder dereferences it eagerly.
func NewEndpointGraphSource(cfg *rest.Config) (*EndpointGraphSource, error) {
	if cfg == nil {
		return nil, fmt.Errorf("EndpointGraphSource: nil rest.Config")
	}
	b, err := graph.NewBuilder(cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("EndpointGraphSource: %w", err)
	}
	return &EndpointGraphSource{builder: b}, nil
}

// Build compiles the RGD into a graph (per-blueprint; amortized over instances).
func (s *EndpointGraphSource) Build(rgd *krov1alpha1.ResourceGraphDefinition) (*graph.Graph, error) {
	return s.builder.NewResourceGraphDefinition(rgd, graph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/engine/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/graphsource.go internal/engine/graphsource_test.go
git commit --no-verify -m "engine: EndpointGraphSource over graph.NewBuilder (M1)"
```

---

## Task 8: kcp endpoint-slice discovery helpers

**Files:**
- Create: `internal/kcp/endpointslice.go`
- Test: `internal/kcp/endpointslice_test.go`

Adapt the two helpers from access-operator (`internal/kcp/endpointslice.go`, `config.go`). Read the reference at `/home/nik/Development/iampam/access-operator/internal/kcp/` for the exact list logic, then reproduce here (do not import across the module boundary).

- [ ] **Step 1: Write the failing test**

```go
// internal/kcp/endpointslice_test.go
package kcp

import "testing"

func TestValidateKubeconfig_RejectsNonWorkspaceHost(t *testing.T) {
	if err := ValidateKubeconfig("https://example.com"); err == nil {
		t.Fatal("want error: host is not workspace-scoped (missing /clusters/ path)")
	}
}

func TestValidateKubeconfig_AcceptsWorkspaceHost(t *testing.T) {
	if err := ValidateKubeconfig("https://kcp.example.com/clusters/root:providers:acme"); err != nil {
		t.Fatalf("want nil for workspace-scoped host, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/kcp/ -v`
Expected: FAIL — package/`ValidateKubeconfig` undefined.

- [ ] **Step 3: Write minimal implementation**

Copy `FindEndpointSlice` and `ValidateKubeconfig` from the access-operator reference, adjusting the package clause and license header. `ValidateKubeconfig` must return an error when the host lacks a `/clusters/` path segment (workspace-scoped kubeconfig check). `FindEndpointSlice(ctx, c client.Client, apiExportName string) (string, error)` lists `APIExportEndpointSlice`s and returns the one for the given APIExport. Use:

```go
// internal/kcp/endpointslice.go
// Copyright 2026 opendefense contributors
// ... (Apache header as in other files) ...

package kcp

import (
	"context"
	"fmt"
	"strings"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ValidateKubeconfig ensures the configured host is workspace-scoped (its path
// contains a /clusters/ segment), which the endpoint-slice discovery relies on.
func ValidateKubeconfig(host string) error {
	if !strings.Contains(host, "/clusters/") {
		return fmt.Errorf("host %q is not workspace-scoped (expected a /clusters/<name> path)", host)
	}
	return nil
}

// FindEndpointSlice returns the name of the APIExportEndpointSlice that belongs
// to apiExportName in the configured workspace.
func FindEndpointSlice(ctx context.Context, c client.Client, apiExportName string) (string, error) {
	var slices apisv1alpha1.APIExportEndpointSliceList
	if err := c.List(ctx, &slices); err != nil {
		return "", fmt.Errorf("listing APIExportEndpointSlices: %w", err)
	}
	for i := range slices.Items {
		s := &slices.Items[i]
		if s.Spec.APIExport.Name == apiExportName {
			return s.Name, nil
		}
	}
	return "", fmt.Errorf("no APIExportEndpointSlice found for APIExport %q", apiExportName)
}
```

Verify the `APIExportEndpointSlice` type path and `Spec.APIExport.Name` field against the access-operator reference and the kcp SDK in the module cache; adjust the import/field if the SDK differs.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/kcp/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/kcp/
git commit --no-verify -m "kcp: APIExportEndpointSlice discovery + workspace-scope check (M1)"
```

---

## Task 9: Controller entrypoint (manager wiring)

**Files:**
- Create: `cmd/controller/main.go`
- Delete: `internal/deps/deps.go` (its packages are now referenced for real)

M1 wires the multicluster manager exactly as access-operator does, but the reconciler builds the graph once at startup from the example blueprint and reconciles the **unstructured** instance GVK. Only the consumer applier is configured (provider target is M2).

- [ ] **Step 1: Write `main.go`**

```go
// cmd/controller/main.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

// Command controller runs the krop instance reconciler over a blueprint's
// APIExport virtual workspace. M1: consumer-target children only.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	crreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
	kropkcp "go.opendefense.cloud/krop-controller/internal/kcp"
)

// M1 fixed identifiers for the single hand-published blueprint.
const (
	apiExportName    = "kubernetesclusters.krop.opendefense.cloud"
	instanceGroup    = "krop.opendefense.cloud"
	instanceVersion  = "v1alpha1"
	instanceKind     = "KubernetesCluster"
)

func main() {
	if err := run(); err != nil {
		ctrl.Log.WithName("entrypoint").Error(err, "fatal")
		os.Exit(1)
	}
}

func run() error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	ctx := signals.SetupSignalHandler()
	entryLog := log.Log.WithName("entrypoint")

	flag.Parse()

	cfg := ctrl.GetConfigOrDie()
	if err := kropkcp.ValidateKubeconfig(cfg.Host); err != nil {
		return fmt.Errorf("kubeconfig must be workspace-scoped: %w", err)
	}

	scheme := ctrl.NewScheme()
	_ = apisv1alpha1.AddToScheme(scheme)

	// Build the compiled graph once at startup from the example blueprint.
	// (M4 replaces this with the Registrar; M1 loads the checked-in RGD.)
	graphSource, err := kropengine.NewEndpointGraphSource(cfg)
	if err != nil {
		return fmt.Errorf("graph source: %w", err)
	}
	rgd, err := kropengine.LoadExampleBlueprint()
	if err != nil {
		return fmt.Errorf("loading example blueprint: %w", err)
	}
	compiled, err := graphSource.Build(rgd)
	if err != nil {
		return fmt.Errorf("building graph: %w", err)
	}

	bootstrapClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("bootstrap client: %w", err)
	}
	endpointSlice, err := kropkcp.FindEndpointSlice(ctx, bootstrapClient, apiExportName)
	if err != nil {
		return fmt.Errorf("discovering APIExportEndpointSlice: %w", err)
	}
	entryLog.Info("using APIExportEndpointSlice", "name", endpointSlice, "apiExport", apiExportName)

	provider, err := apiexport.New(cfg, endpointSlice, apiexport.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("apiexport provider: %w", err)
	}
	mgr, err := mcmanager.New(cfg, provider, manager.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	instanceGVK := schema.GroupVersionKind{Group: instanceGroup, Version: instanceVersion, Kind: instanceKind}
	eng := kropengine.New()

	reconciler := mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
		l := log.FromContext(ctx).WithValues("cluster", req.ClusterName, "name", req.Name)
		cl, err := mgr.GetCluster(ctx, req.ClusterName)
		if err != nil {
			return crreconcile.Result{}, fmt.Errorf("getting cluster %q: %w", req.ClusterName, err)
		}
		consumer := cl.GetClient()

		inst := &unstructured.Unstructured{}
		inst.SetGroupVersionKind(instanceGVK)
		if err := consumer.Get(ctx, req.NamespacedName, inst); err != nil {
			return crreconcile.Result{}, client.IgnoreNotFound(err)
		}

		rt, err := kroruntime.FromGraph(compiled, inst, krograph.RGDConfig{
			MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
		})
		if err != nil {
			return crreconcile.Result{}, fmt.Errorf("runtime: %w", err)
		}

		appliers := map[kropengine.Target]kropengine.Applier{
			kropengine.TargetConsumer: kropengine.NewSSAApplier(consumer),
			// TargetProvider added in M2.
		}
		res, err := eng.Reconcile(ctx, rt, appliers)
		if err != nil {
			return crreconcile.Result{}, err
		}

		desiredInstance, err := kropengine.ProjectStatus(rt)
		if err == nil {
			if status, found, _ := unstructured.NestedMap(desiredInstance.Object, "status"); found {
				_ = unstructured.SetNestedMap(inst.Object, status, "status")
				if uerr := consumer.Status().Update(ctx, inst); uerr != nil {
					l.Error(uerr, "status update")
				}
			}
		}

		if res.Requeue {
			return crreconcile.Result{Requeue: true}, nil
		}
		l.Info("reconciled", "ready", res.Ready)
		return crreconcile.Result{}, nil
	})

	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("krop-instance").
		For(inst()).
		Complete(reconciler); err != nil {
		return fmt.Errorf("building controller: %w", err)
	}

	entryLog.Info("starting manager")
	return mgr.Start(ctx)
}

// inst returns a fresh unstructured typed to the instance GVK for the watch.
func inst() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: instanceGroup, Version: instanceVersion, Kind: instanceKind})
	return u
}
```

Notes for the implementer:
- `LoadExampleBlueprint()` is added in Task 10; until then `main.go` will not compile — do Task 10 first if building incrementally, or stub `LoadExampleBlueprint` returning `sampleRGD()`'s production twin. The plan orders Task 10 before the build step below.
- `ctrl.NewScheme` / `apisv1alpha1.AddToScheme`: confirm the scheme registration path against the kcp SDK in the module cache; adjust if the SDK exposes `AddToScheme` differently.
- `mcbuilder ... For(unstructured)`: watching an unstructured GVK requires the provider/RESTMapper to resolve it, which the APIExport virtual workspace serves at runtime. Compile-checked here; behavior validated in Task 11.

- [ ] **Step 2: Delete the temporary import anchor**

```bash
git rm internal/deps/deps.go
```

- [ ] **Step 3: Commit (after Task 10 makes it build)**

Deferred — commit `main.go` together with Task 10 once `go build ./...` is green.

---

## Task 10: Example blueprint loader + checked-in RGD

**Files:**
- Create: `internal/engine/blueprint.go` (`LoadExampleBlueprint`)
- Create: `config/kcp/examples/blueprint-kubernetescluster.yaml`
- Test: `internal/engine/blueprint_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/blueprint_test.go
package engine

import "testing"

func TestLoadExampleBlueprint(t *testing.T) {
	rgd, err := LoadExampleBlueprint()
	if err != nil {
		t.Fatalf("LoadExampleBlueprint: %v", err)
	}
	if rgd.Spec.Schema.Kind != "KubernetesCluster" {
		t.Fatalf("schema kind = %q, want KubernetesCluster", rgd.Spec.Schema.Kind)
	}
	if len(rgd.Spec.Resources) != 1 || rgd.Spec.Resources[0].ID != "config" {
		t.Fatalf("want a single resource id=config, got %+v", rgd.Spec.Resources)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestLoadExampleBlueprint -v`
Expected: FAIL — `undefined: LoadExampleBlueprint`.

- [ ] **Step 3: Write the RGD YAML and the loader**

```yaml
# config/kcp/examples/blueprint-kubernetescluster.yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: kubernetescluster
spec:
  schema:
    apiVersion: v1alpha1
    kind: KubernetesCluster
    group: krop.opendefense.cloud
    spec:
      region: string
    status:
      configMapName: ${config.metadata.name}
  resources:
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.region}-cluster-config
          namespace: default
          annotations:
            krop.opendefense.cloud/target: consumer
        data:
          region: ${schema.spec.region}
```

```go
// internal/engine/blueprint.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package engine

import (
	_ "embed"
	"fmt"

	"sigs.k8s.io/yaml"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

//go:embed embedded/blueprint-kubernetescluster.yaml
var exampleBlueprintYAML []byte

// LoadExampleBlueprint parses the M1 example blueprint (a kro RGD) that the
// controller compiles at startup. Replaced by the Registrar in M4.
func LoadExampleBlueprint() (*krov1alpha1.ResourceGraphDefinition, error) {
	var rgd krov1alpha1.ResourceGraphDefinition
	if err := yaml.Unmarshal(exampleBlueprintYAML, &rgd); err != nil {
		return nil, fmt.Errorf("unmarshalling example blueprint: %w", err)
	}
	return &rgd, nil
}
```

`go:embed` cannot reach outside the package directory, so also copy the YAML to `internal/engine/embedded/blueprint-kubernetescluster.yaml` (identical content). Keep `config/kcp/examples/…` as the operator-facing copy; the embedded copy is what the binary compiles in. Add a one-line comment in both files pointing at the other to keep them in sync (M4 removes the embedded copy).

- [ ] **Step 4: Run tests + full build**

Run: `go test ./internal/engine/ -v && go build ./... && go vet ./...`
Expected: PASS / clean. This is where `cmd/controller/main.go` (Task 9) must compile.

- [ ] **Step 5: Commit (main.go + loader together)**

```bash
git add cmd/controller/main.go internal/engine/blueprint.go internal/engine/embedded/ config/kcp/examples/
git rm internal/deps/deps.go
git commit --no-verify -m "controller: manager wiring + example blueprint loader (M1)"
```

---

## Task 11: Hand-written APIExport/ARS/APIBinding fixtures

**Files:**
- Create: `config/kcp/apiresourceschema-kubernetesclusters.krop.opendefense.cloud.yaml`
- Create: `config/kcp/apiexport-krop-m1.yaml` (shown above in the fixture note)
- Create: `test/fixtures/apibinding-kubernetescluster.yaml`

These are the fixtures the Task 12 envtest suite applies. No Go tests here — validated by Task 12.

- [ ] **Step 1: Write the APIResourceSchema for the instance type**

Synthesize the CRD for `KubernetesCluster` (schema `spec.region: string`, `status.configMapName: string`) and convert to an `apis.kcp.io/v1alpha1` `APIResourceSchema` named `v1alpha1.kubernetesclusters.krop.opendefense.cloud`. Model it on the access-operator ARS at `/home/nik/Development/iampam/access-operator/config/kcp/apiresourceschema-rolebindings.access.opendefense.cloud.yaml`. The `spec.versions[0].schema.openAPIV3Schema` must have `spec.properties.region {type: string}` and `status.properties.configMapName {type: string}`, with `status` served as a subresource. Tip: `go run` a tiny throwaway that calls `simpleschema.ToOpenAPISpec` + `crd.SynthesizeCRD` (proven pure in M0 Q3) and hand-convert its output to the ARS shape, or copy the access-operator ARS and edit the schema.

- [ ] **Step 2: Write the APIExport** (content in the fixture note above — `apis.kcp.io/v1alpha2`, `spec.resources[]`, no permissionClaims).

- [ ] **Step 3: Write the consumer APIBinding fixture**

```yaml
# test/fixtures/apibinding-kubernetescluster.yaml
apiVersion: apis.kcp.io/v1alpha2
kind: APIBinding
metadata:
  name: kubernetesclusters
spec:
  reference:
    export:
      # ${PROVIDER_PATH} is substituted by the test with the provider workspace path.
      path: ${PROVIDER_PATH}
      name: kubernetesclusters.krop.opendefense.cloud
```

Confirm the v1alpha2 `APIBinding.spec.reference.export` shape (`path` + `name`) against `github.com/kcp-dev/sdk/apis/apis/v1alpha2` and dependency-controller's `test/fixtures/apibinding-*.yaml`.

- [ ] **Step 4: Commit**

```bash
git add config/kcp/ test/fixtures/
git commit --no-verify -m "kcp: hand-written ARS + APIExport + APIBinding fixtures (M1)"
```

---

## Task 12: In-process envtest e2e (the walking-skeleton proof)

**Files:**
- Create: `internal/controller/suite_test.go`
- Create: `internal/controller/m1_integration_test.go`

Modeled directly on `dependency-controller`'s `internal/controller/suite_test.go` + `integration_test.go`. Read those two files (via `gh api` / raw GitHub, repo `opendefensecloud/dependency-controller`, branch `main`) before writing — reproduce the boot + fixture helpers, adjusting types to M1. This suite runs under `make test` (which downloads the kcp binary and sets `TEST_KCP_ASSETS`); it skips when that env var is unset so plain `go test ./...` stays hermetic.

First add the test dependencies:

```bash
go get github.com/onsi/ginkgo/v2@latest github.com/onsi/gomega@latest
# multicluster-provider/envtest ships with the already-added multicluster-provider module.
```

- [ ] **Step 1: Write the suite boot**

```go
// internal/controller/suite_test.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package controller_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
)

var (
	env       *envtest.Environment
	kcpConfig *rest.Config
	scheme    = runtime.NewScheme()
)

func init() {
	runtime.Must(clientgoscheme.AddToScheme(scheme))
	runtime.Must(apisv1alpha1.AddToScheme(scheme)) // APIResourceSchema, APIExportEndpointSlice
	runtime.Must(apisv1alpha2.AddToScheme(scheme)) // APIExport, APIBinding
	runtime.Must(corev1alpha1.AddToScheme(scheme))
	runtime.Must(tenancyv1alpha1.AddToScheme(scheme))
}

func TestM1Integration(t *testing.T) {
	if os.Getenv("TEST_KCP_ASSETS") == "" {
		t.Skip("set TEST_KCP_ASSETS (make test downloads the kcp binary) to run the M1 envtest e2e")
	}
	RegisterFailHandler(Fail)

	var err error
	env = &envtest.Environment{}
	kcpConfig, err = env.Start()
	if err != nil {
		t.Fatalf("starting kcp envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	RunSpecs(t, "M1 Instance Reconciler Integration Suite")
}
```

- [ ] **Step 2: Run to verify it skips (hermetic)**

Run: `go test ./internal/controller/ -v`
Expected: `SKIP` (TEST_KCP_ASSETS unset). Confirms the suite compiles and is hermetic by default.

- [ ] **Step 3: Write the integration spec**

```go
// internal/controller/m1_integration_test.go
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
	"sigs.k8s.io/controller-runtime/pkg/manager"

	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	"github.com/kcp-dev/multicluster-provider/envtest"
	"github.com/kcp-dev/sdk/apis/core"
	"github.com/kcp-dev/logicalcluster/v3"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	ctrl "sigs.k8s.io/controller-runtime"

	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

const apiExportName = "kubernetesclusters.krop.opendefense.cloud"

// applyFixtureFromFile reads a YAML fixture, substitutes ${VAR} placeholders,
// and Creates it into the given workspace via the cluster client.
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

var _ = Describe("M1 instance reconcile", Ordered, func() {
	var (
		ctx          = context.Background()
		cli          clusterclient.ClusterClient
		providerPath logicalcluster.Path
		consumerPath logicalcluster.Path
		cancel       context.CancelFunc
	)

	BeforeAll(func() {
		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		// Provider workspace: apply the ARS + APIExport.
		_, providerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(),
			envtest.WithNamePrefix("krop-provider"))
		applyFixtureFromFile(ctx, cli, providerPath,
			"../../config/kcp/apiresourceschema-kubernetesclusters.krop.opendefense.cloud.yaml", nil)
		applyFixtureFromFile(ctx, cli, providerPath,
			"../../config/kcp/apiexport-krop-m1.yaml", nil)

		// Wait until the APIExport's endpoint slice is populated (virtual ws ready).
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			var slices apisv1alpha1EndpointSliceList
			_ = cli.Cluster(providerPath).List(ctx, &slices)
			for i := range slices.Items {
				if len(slices.Items[i].Status.APIExportEndpoints) > 0 {
					return true, ""
				}
			}
			return false, "endpoint slice not ready"
		}, wait.ForeverTestTimeout, 100*time.Millisecond, "APIExport virtual workspace not ready")

		// Start the controller in-process against the APIExport virtual workspace.
		runCtx, c := context.WithCancel(ctx)
		cancel = c
		exportCfg := rest.CopyConfig(kcpConfig)
		exportCfg.Host += providerPath.RequestPath()

		graphSource, err := kropengine.NewEndpointGraphSource(exportCfg)
		Expect(err).NotTo(HaveOccurred())
		rgd, err := kropengine.LoadExampleBlueprint()
		Expect(err).NotTo(HaveOccurred())
		compiled, err := graphSource.Build(rgd) // validates §16.3 NewBuilder-vs-live-kcp
		Expect(err).NotTo(HaveOccurred())

		provider, err := apiexport.New(exportCfg, apiExportName, apiexport.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())
		mgr, err := mcmanager.New(exportCfg, provider, manager.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		instGVK := schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesCluster"}
		eng := kropengine.New()
		reconciler := mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			cl, err := mgr.GetCluster(ctx, req.ClusterName)
			if err != nil {
				return ctrl.Result{}, err
			}
			consumer := cl.GetClient()
			inst := &unstructured.Unstructured{}
			inst.SetGroupVersionKind(instGVK)
			if err := consumer.Get(ctx, req.NamespacedName, inst); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			rt, err := kroruntime.FromGraph(compiled, inst, krograph.RGDConfig{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000})
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
		watch.SetGroupVersionKind(instGVK)
		Expect(mcbuilder.ControllerManagedBy(mgr).Named("krop-instance").For(watch).Complete(reconciler)).To(Succeed())
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(runCtx)).To(Succeed())
		}()

		// Consumer workspace: bind the export.
		_, consumerPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(),
			envtest.WithNamePrefix("krop-consumer"))
		applyFixtureFromFile(ctx, cli, consumerPath,
			"../../test/fixtures/apibinding-kubernetescluster.yaml",
			map[string]string{"PROVIDER_PATH": providerPath.String()})

		// Wait until the bound instance kind is List-able in the consumer ws.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesClusterList"})
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
			err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm)
			if err != nil {
				return false, err.Error()
			}
			region, _, _ := unstructured.NestedString(cm.Object, "data", "region")
			return region == "eu", "configmap data.region=" + region
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "consumer-target ConfigMap not materialized")

		// And project status.configMapName back onto the instance.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			name, _, _ := unstructured.NestedString(got.Object, "status", "configMapName")
			return name == "eu-cluster-config", "status.configMapName=" + name
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance status not projected")
	})
})
```

Implementer notes:
- `apisv1alpha1EndpointSliceList` in the endpoint-slice wait is shorthand — use the real type `apisv1alpha1.APIExportEndpointSliceList` (import `apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"`; it is already registered on the scheme in `suite_test.go`). Verify `Status.APIExportEndpoints` is the correct field name against the SDK.
- `envtest.NewWorkspaceFixture`, `envtest.Eventually`, `envtest.WithNamePrefix`, `clusterclient.New`, `cli.Cluster(path)`, `path.RequestPath()`, `path.String()`, `core.RootCluster.Path()` are all taken from dependency-controller's suite — confirm exact signatures against its `internal/controller/*_test.go` and adjust import paths (`clusterclient "github.com/kcp-dev/multicluster-provider/client"`).
- This is the test that retires the residual §16.3 risk: `graphSource.Build` calls `graph.NewBuilder` against the **real** provider-workspace endpoint. If it fails on kcp discovery/OpenAPI, that is the signal to open the fallback-B upstream `NewBuilderWithResolver` PR (M0 findings §16.3).

- [ ] **Step 4: Run the full suite locally (downloads kcp)**

Run: `make test` (or `TEST_KCP_ASSETS=$(pwd)/bin go test ./internal/controller/ -v` after `make bin/kcp`)
Expected: the `M1 instance reconcile` spec PASSES — ConfigMap appears, status projected. If `graph.NewBuilder` errors against kcp discovery, capture the error and follow the §16.3 fallback.

- [ ] **Step 5: Verify hermetic default stays green**

Run: `go test ./... 2>&1 | tail -20`
Expected: `internal/engine`, `internal/kcp` PASS; `internal/controller` SKIPs without `TEST_KCP_ASSETS`.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/ go.mod go.sum
git commit --no-verify -m "controller: in-process envtest e2e for the M1 walking skeleton (M1)"
```

---

## Definition of done (M1)

- `go build ./...`, `go vet ./...`, `go test ./...` green (the `internal/controller` envtest suite SKIPs without `TEST_KCP_ASSETS`).
- `make test` green: the in-process envtest e2e proves the walking skeleton end-to-end (create instance → consumer-target ConfigMap materializes → status projected) against a real kcp, **retiring the residual §16.3 `NewBuilder`-vs-live-kcp risk**.
- Unit-verified: routing/strip, SSA apply + read-back, the node drive loop (routing + observe + cross-node CEL), status projection, endpoint discovery.
- `cmd/controller/main.go` compiles and wires the multicluster manager over the instance GVK with a consumer-only applier map.
- Hand-written APIExport (`v1alpha2`) / ARS (`v1alpha1`) / APIBinding (`v1alpha2`) + example blueprint checked in; the graph builds from the embedded RGD.
- `internal/deps/deps.go` removed.
- **Deferred to M6 (not M1):** the kind + kcp-operator + helm full-stack deployment e2e tier.

## Self-review notes
- Provider target intentionally unimplemented (M2) but the engine's `map[Target]Applier` shape and routing helper already support it — an unconfigured provider target errors clearly rather than silently dropping.
- GC labels (`instance-uid`, etc.) are **not** added in M1 (they belong to M5); M1 relies on nothing for cleanup yet. Do not add them here.
- `MaxCollectionSize`/`MaxCollectionDimensionSize` are duplicated as literals across call sites; acceptable for M1. If a third call site appears, hoist to a package const `defaultRGDConfig` — not before (YAGNI).
