# External Refs + Host Target Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add kro `externalRef` read support (all three planes) and a `host` write target, routed by a
new per-resource `target` field that replaces the `krop.opendefense.cloud/target` annotation.

**Architecture:** Our CRD wraps kro's tiny spec (`Schema` + `Resources`), inlining kro's `Resource` and
adding an enum `target` field; `ToKro()` strips it into a build-time routing map (`resourceID → target`)
and hands the graph builder clean kro types. The engine gains a read-only branch (external nodes are
Get/List'd through a `Reader`, never applied) and resolves target per-node from the routing map. The
`host` target reuses the provider applier/naming/label/GC/sweep machinery over a separate in-cluster
client. Consumer external refs get read-only permissionClaims.

**Tech stack:** Go, kcp SDK, kro v0.9.2 (library), controller-runtime v0.24, multicluster-runtime,
Ginkgo/Gomega envtest (kcp 0.30.0 via TEST_KCP_ASSETS).

**Design spec:** `docs/superpowers/specs/2026-07-08-external-refs-and-host-target-design.md` (read it for
rationale). Each task ends with the build + tests green (`go build ./... && make lint && go test ./...`
for unit tasks; envtest task adds `make test`).

**Reference the existing code** — the appliers (`internal/engine/apply.go`), the reconcile loop
(`internal/engine/engine.go`), the Reconciler (`internal/controller/reconciler.go`), the Sweeper
(`internal/controller/sweeper.go`), and the entrypoint (`cmd/controller/main.go`) are the patterns to
mirror. Match their comment density and idiom.

---

### Task 1: Engine routing + read primitives (additive)

Add the new engine primitives WITHOUT removing the annotation yet, so the build stays green.

**Files:**
- Modify: `internal/engine/route.go` (add `TargetHost`, `ParseTarget`, `TargetForNode`; keep
  `TargetOf`/`StripRouting` for now)
- Create: `internal/engine/reader.go`
- Test: `internal/engine/route_test.go`, `internal/engine/reader_test.go`

- [ ] **Step 1: Add host target + routing helpers to `route.go`.**

Add to the `const` block: `TargetHost Target = "host"`. Add:

```go
// allTargets is the set of valid routing targets (also enforced by the CRD enum).
var allTargets = map[Target]bool{TargetConsumer: true, TargetProvider: true, TargetHost: true}

// ParseTarget validates a raw target string. Empty ⇒ TargetConsumer (the default).
func ParseTarget(s string) (Target, error) {
	if s == "" {
		return TargetConsumer, nil
	}
	t := Target(s)
	if !allTargets[t] {
		return "", fmt.Errorf("invalid target %q (want %q, %q or %q)", s, TargetConsumer, TargetProvider, TargetHost)
	}
	return t, nil
}

// TargetForNode resolves a node's routing target from the build-time routing map
// (keyed by resource id == node.Spec.Meta.ID), defaulting to consumer when absent.
func TargetForNode(id string, routing map[string]Target) Target {
	if t, ok := routing[id]; ok {
		return t
	}
	return TargetConsumer
}
```

- [ ] **Step 2: Write `internal/engine/reader.go`.**

```go
// internal/engine/reader.go — read side of engine I/O, mirroring Applier.
package engine

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reader reads existing objects a blueprint references via externalRef but does
// not create. It is the read counterpart of Applier: the engine drives all
// read I/O through it, keeping the reconcile loop client-agnostic and testable.
type Reader interface {
	// Get fetches one object by name (and namespace, empty for cluster-scoped).
	Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error)
	// List returns objects of gvk in namespace (empty ⇒ all namespaces) matching selector.
	List(ctx context.Context, gvk schema.GroupVersionKind, namespace string, selector labels.Selector) ([]*unstructured.Unstructured, error)
}

// ClientReader implements Reader over one workspace/cluster-scoped client.Client.
type ClientReader struct{ c client.Client }

// NewClientReader builds a ClientReader bound to one client.
func NewClientReader(c client.Client) *ClientReader { return &ClientReader{c: c} }

func (r *ClientReader) Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := r.c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (r *ClientReader) List(ctx context.Context, gvk schema.GroupVersionKind, namespace string, selector labels.Selector) ([]*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if selector != nil {
		opts = append(opts, client.MatchingLabelsSelector{Selector: selector})
	}
	if err := r.c.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		out[i] = &list.Items[i]
	}
	return out, nil
}
```

- [ ] **Step 3: Unit tests.** `ParseTarget` (empty→consumer, valid, invalid→error); `TargetForNode`
  (hit→target, miss→consumer). `ClientReader` Get/List against a `fake.NewClientBuilder()` client
  (found, not-found error surfaces, list-by-selector). Run `go test ./internal/engine/...`.

- [ ] **Step 4: Commit** `feat(engine): add host target, ParseTarget/TargetForNode, Reader`.

---

### Task 2: API `target` field + `ToKro` + routing plumbing

Add the wrapper types and thread the routing map through publish → served blueprint → Reconciler. The
engine does NOT consume routing yet (still annotation-based), so behavior is unchanged and fixtures stay
as they are. Build green.

**Files:**
- Modify: `api/v1alpha1/resourcegraphdefinition_types.go`
- Create: `api/v1alpha1/routing.go` (the `ToKro` conversion + tests target)
- Modify: `api/v1alpha1/zz_generated.deepcopy.go` (regen), `config/crd/**` (regen)
- Modify: `internal/registrar/registrar.go`, `internal/registrar/hash.go` (hash our wrapper),
  `internal/engine/graphsource.go` (accepts kro spec)
- Modify: `cmd/controller/main.go` (`servedBlueprint` gains routing; `OnPublished` signature),
  `internal/registrar/registrar.go` (OnPublished call site), `internal/controller/reconciler.go`
  (`Reconciler.Routing` field, unused this task)
- Test: `api/v1alpha1/routing_test.go`

- [ ] **Step 1: Wrapper types.** In `resourcegraphdefinition_types.go` replace
  `Spec krov1alpha1.ResourceGraphDefinitionSpec` with `Spec ResourceGraphDefinitionSpec`, and add (in
  `routing.go`):

```go
// Resource wraps a kro resource with a krop routing target. All kro fields
// (id/template/externalRef/readyWhen/includeWhen/forEach) are inlined verbatim.
type Resource struct {
	krov1alpha1.Resource `json:",inline"`
	// Target routes this resource's object(s): consumer (default) | provider | host.
	// +kubebuilder:validation:Enum=consumer;provider;host
	// +optional
	Target string `json:"target,omitempty"`
}

// ResourceGraphDefinitionSpec is kro's spec (Schema + Resources) with each
// resource carrying a routing Target. ToKro strips the targets back out.
type ResourceGraphDefinitionSpec struct {
	// +kubebuilder:validation:Required
	Schema *krov1alpha1.Schema `json:"schema,omitempty"`
	// +optional
	Resources []*Resource `json:"resources,omitempty"`
}

// ToKro returns the underlying kro spec (clean types for the graph builder) plus
// a routing map of resource id → target. Resources with an empty target are
// omitted from the map (they default to consumer downstream).
func (s ResourceGraphDefinitionSpec) ToKro() (krov1alpha1.ResourceGraphDefinitionSpec, map[string]string) {
	routing := map[string]string{}
	res := make([]*krov1alpha1.Resource, 0, len(s.Resources))
	for _, r := range s.Resources {
		if r == nil {
			continue
		}
		kr := r.Resource
		res = append(res, &kr)
		if r.Target != "" {
			routing[r.ID] = r.Target
		}
	}
	return krov1alpha1.ResourceGraphDefinitionSpec{Schema: s.Schema, Resources: res}, routing
}
```

- [ ] **Step 2: Regenerate** deepcopy + CRD: `make generate manifests` (or the repo's codegen make
  targets — check the `Makefile`). Verify `zz_generated.deepcopy.go` gains `Resource`/
  `ResourceGraphDefinitionSpec` DeepCopy and the served CRD YAML under `config/crd` (and the chart CRD
  copy `charts/krop-controller/crds/`) now shows `resources[].target` with the enum. Fix the codegen
  markers if generation misses them.

- [ ] **Step 3: Rewire kro-spec consumers.**
  - `registrar.go`: where it builds the graph (`krov1alpha1.ResourceGraphDefinition{Spec: bp.Spec}`) and
    hashes, change to `kroSpec, routingRaw := bp.Spec.ToKro()`; build the kro RGD from `kroSpec`; pass
    `routingRaw` forward (Step 4). `SpecHash` now takes the WRAPPER spec (`bp.Spec`) so target changes
    bump the hash — update `hash.go`'s signature to `SpecHash(spec v1alpha1.ResourceGraphDefinitionSpec)`
    (import our api package) or hash `bp.Spec` directly at the call site; keep it deterministic
    (json.Marshal). Confirm `graphsource.Build` still receives a `*krov1alpha1.ResourceGraphDefinition`.
  - `graphsource.go`: unchanged signature; it is now fed the RGD built from `kroSpec`.

- [ ] **Step 4: Thread routing to the served blueprint.**
  - `main.go`: `servedBlueprint` gains `routing map[string]kropengine.Target`. `OnPublished` gains a
    `routing map[string]kropengine.Target` parameter; store it on the `servedBlueprint`; set
    `reconciler.Routing = sb.routing` where the Reconciler is constructed.
  - `registrar.go`: convert `routingRaw` (`map[string]string`) to `map[string]kropengine.Target` via
    `kropengine.ParseTarget` (fail the reconcile with a clear error on an invalid value — belt-and-braces
    beyond the CRD enum), and pass it to `OnPublished`.
  - `reconciler.go`: add field `Routing map[string]kropengine.Target` (unused this task).

- [ ] **Step 5: Test `ToKro`.** `routing_test.go`: target present → map entry; empty target → absent;
  kro fields (id/template/externalRef) preserved; nil resource skipped. Run `go test ./api/...`.

- [ ] **Step 6: Full build + existing suite.** `go build ./... && make lint && go test ./...` (existing
  e2e fixtures still use the annotation and still pass — engine is unchanged this task). **Commit**
  `feat(api): per-resource target field + ToKro routing extraction`.

---

### Task 3: Engine read-only branch + routing cutover (remove annotation)

The cutover. The engine consumes the routing map and the read-only branch; the annotation mechanism is
deleted; all fixtures/examples/e2e blueprints migrate to `target`.

**Files:**
- Modify: `internal/engine/engine.go` (signature + read branch + routing), `internal/engine/route.go`
  (delete `TargetOf`/`StripRouting`/`TargetAnnotation`)
- Modify: `internal/controller/reconciler.go` (build `readers`, pass `Routing`; `childGVKs` via
  `TargetForNode` + exclude external nodes)
- Modify: `internal/registrar/claims.go` (`ForeignConsumerGRs` via routing + exclude external nodes —
  verb split lands in Task 4)
- Migrate: `config/kcp/examples/blueprint-kubernetescluster.yaml`,
  `internal/engine/embedded/blueprint-kubernetescluster.yaml`,
  `test/fixtures/blueprint-kubernetescluster-rgd.yaml`, `test/fixtures/blueprint-prune-rgd.yaml`, and any
  inline blueprints in `*_test.go` — replace each template's `metadata.annotations.
  krop.opendefense.cloud/target: X` with a resource-level `target: X`.
- Test: update `internal/engine/engine_test.go` and add external-read cases.

- [ ] **Step 1: New `Reconcile` signature + read branch.** Change to
  `Reconcile(ctx, rt, appliers map[Target]Applier, readers map[Target]Reader, routing map[string]Target)`.
  In the per-node loop, before the current apply block:
  - `target := TargetForNode(node.Spec.Meta.ID, routing)`.
  - after `desired, err := node.GetDesired()` (keep the existing `ErrDataPending` handling), branch on
    `node.Spec.Meta.Type`:

```go
switch node.Spec.Meta.Type {
case graph.NodeTypeExternal:
	reader, ok := readers[target]
	if !ok {
		return res, fmt.Errorf("node %s: no reader configured for target %q", node.Spec.Meta.ID, target)
	}
	if len(desired) == 0 { // nothing to read (ignored/empty) — skip
		continue
	}
	d := desired[0]
	obj, err := reader.Get(ctx, d.GroupVersionKind(), d.GetNamespace(), d.GetName())
	if apierrors.IsNotFound(err) {
		res.Ready, res.Requeue = false, true // wait for the external object to appear
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("node %s: external get: %w", node.Spec.Meta.ID, err)
	}
	node.SetObserved([]*unstructured.Unstructured{obj})
	if rerr := node.CheckReadiness(); rerr != nil {
		res.Ready, res.Requeue = false, true
	}
	continue
case graph.NodeTypeExternalCollection:
	// analogous: reader.List(gvk, ns, selector from desired[0].metadata.selector); SetObserved(items).
	// Reuse kro's selector extraction shape (metadata.selector → labels.Selector); empty ⇒ everything.
	...
}
```

  The existing apply path handles the default (`Resource`/`Collection`/`Instance`) — replace the
  per-object `TargetOf(obj)` + `StripRouting(obj)` with the node-level `target` computed above (all of a
  node's objects share its target). External nodes must NOT be applied, labeled, owned, or recorded.
  Add the `graph` and `apierrors` imports.

  On a not-found external ref, `res.Complete` must stay false (return before the `res.Complete = true`
  line) so prune is disabled while the dependency is pending — same as `ErrDataPending`.

- [ ] **Step 2: Delete annotation routing.** Remove `TargetOf`, `StripRouting`, and `TargetAnnotation`
  from `route.go`. `go build ./...` will now flag every caller — fix them in Steps 3–4.

- [ ] **Step 3: Reconciler.** In `reconciler.go`:
  - Build a `readers map[kropengine.Target]kropengine.Reader{ TargetConsumer: NewClientReader(consumerClient), TargetProvider: NewClientReader(r.ProviderClient) }`
    (host added in Task 5).
  - Pass `readers` and `r.Routing` to `kropengine.New().Reconcile(...)`.
  - `childGVKs`: resolve `t := kropengine.TargetForNode(node.Spec.Meta.ID, r.Routing)` instead of
    `TargetOf(node.Template)`, and `continue` when `node.Spec.Meta.Type` is `NodeTypeExternal` or
    `NodeTypeExternalCollection` (never GC objects we don't own). Import kro `graph`.

- [ ] **Step 4: Claims enumeration.** In `claims.go`, change
  `ForeignConsumerGRs(g, instanceGR, routing map[string]kropengine.Target)` to resolve target via
  `TargetForNode(node.Spec.Meta.ID, routing)` and skip external nodes for now (verb split is Task 4).
  Update the call site in `registrar.go` to pass the routing map (converted to `map[string]Target`).

- [ ] **Step 5: Migrate blueprints.** In each fixture/example/embedded blueprint and any inline `*_test.go`
  blueprint, move the routing annotation to a resource-level `target:`. Example transform:

```yaml
# before
- id: agentRequest
  template:
    metadata:
      annotations: { krop.opendefense.cloud/target: provider }
    ...
# after
- id: agentRequest
  target: provider
  template:
    ...
```

  `grep -rn "opendefense.cloud/target"` across `config/`, `test/`, `internal/engine/embedded/` and
  `*_test.go` must return nothing but the new ADR/docs (docs handled in Task 8).

- [ ] **Step 6: Update engine tests.** Adjust `engine_test.go` to the new signature (pass `readers` +
  `routing`); add: external node Get → observed feeds a downstream CEL child; external NotFound ⇒
  `Ready=false, Requeue=true, Complete=false` and the applier is never called (assert via a spy applier);
  external node is never recorded/labeled. Run `go test ./internal/...`.

- [ ] **Step 7: Full build + suite.** `go build ./... && make lint && go test ./...`. **Commit**
  `feat(engine)!: externalRef read branch + target-field routing, drop annotation`.

---

### Task 4: Read-only permission claims for consumer external refs

**Files:**
- Modify: `internal/registrar/claims.go`, `internal/registrar/registrar.go`
- Test: `internal/registrar/claims_test.go`

- [ ] **Step 1: Split writable vs external.** Change `ForeignConsumerGRs` to return two `[]schema.GroupResource`
  sets — writable (consumer template nodes, excluding the instance's own GR) and external (consumer
  external-ref nodes) — keyed off `node.Spec.Meta.Type` and `TargetForNode(id, routing)`.

- [ ] **Step 2: Verb sets.** Add `var readOnlyVerbs = []string{"get", "list", "watch"}`. Give `DeriveClaims`
  a variant (or a `verbs []string` parameter) so it emits `claimVerbs` for writable GRs and
  `readOnlyVerbs` for external GRs. Merge both claim lists; identity resolution + `validateClaims`
  unchanged (a foreign external CRD still needs its export bound in the provider workspace).

- [ ] **Step 3: Wire.** Update `registrar.go` to derive both sets and produce the merged claims.

- [ ] **Step 4: Tests.** consumer external ref ⇒ `get/list/watch` only; consumer template ⇒ full CRUD;
  provider/host external ⇒ no claim; core external (empty group) ⇒ empty identityHash allowed; foreign
  external with unresolved identity ⇒ `validateClaims` rejects. Run `go test ./internal/registrar/...`.

- [ ] **Step 5: Commit** `feat(registrar): read-only claims for consumer external refs`.

---

### Task 5: Host write target

**Files:**
- Modify: `internal/controller/reconciler.go` (`HostClient`; host applier/reader; childGVKs/prune/delete host)
- Modify: `cmd/controller/main.go` (host client from InClusterConfig + `--host-kubeconfig`; wire to Reconciler)
- Test: `internal/engine` applier-chain test if needed; `internal/controller` reconciler unit/e2e in Task 7

- [ ] **Step 1: Reconciler host wiring.** Add `HostClient client.Client` to `Reconciler`. When non-nil:
  - appliers map gains
    `TargetHost: NewLabelingApplier(NewQualifyingApplier(NewRecordingApplier(NewSSAApplier(r.HostClient), &appliedHost), func(orig string) string { return kropengine.ProviderChildName(clusterName, instanceName, orig) }), labels)`
    (mirror the provider chain; new `appliedHost` sink).
  - readers map gains `TargetHost: NewClientReader(r.HostClient)`.
  - `deleteChildren` / `pruneChildren` target→client maps gain `TargetHost: r.HostClient`.
  - prune call site includes `TargetHost: appliedHost`.
  - All host entries are added only when `r.HostClient != nil` (build the maps conditionally).

- [ ] **Step 2: Host client in `main.go`.** Add flag
  `hostKubeconfig = flag.String("host-kubeconfig", "", "Path to a kubeconfig for the host cluster (the physical cluster to provision host-target children into). Defaults to in-cluster config.")`.
  Resolve: if `*hostKubeconfig != ""` load it (`clientcmd.BuildConfigFromFlags("", *hostKubeconfig)`),
  else try `rest.InClusterConfig()`. On success build `hostClient, _ := client.New(hostCfg, client.Options{Scheme: <core+CRD scheme>})`
  and pass to the Reconciler (`reconciler.HostClient = hostClient`) and log "host target enabled". On
  failure (no in-cluster config, dev run), log "host target disabled (no host kubeconfig)" and leave
  `HostClient` nil — blueprints routing to host then fail loudly per the engine's missing-applier error.

- [ ] **Step 3: Build + suite.** `go build ./... && make lint && go test ./...`. **Commit**
  `feat(controller): host write target (in-cluster client + --host-kubeconfig)`.

---

### Task 6: Host children in the orphan sweep

**Files:**
- Modify: `internal/controller/reconciler.go` (`writeLivenessRecord` records `hostChildGVKs`)
- Modify: `internal/controller/sweeper.go` (`HostClient`; `sweepChildren` sweeps host too)
- Modify: `cmd/controller/main.go` (pass `HostClient` to the `Sweeper`)
- Test: `internal/controller/sweeper_test.go` (or the existing orphan-sweep unit test)

- [ ] **Step 1: Record host GVKs.** In `writeLivenessRecord`, add
  `data["hostChildGVKs"] = r.childGVKString(kropengine.TargetHost)` (refactor `providerChildGVKString`
  into a `childGVKString(target)` helper reusing `childGVKs`). When `HostClient` is nil, `childGVKs(host)`
  is empty and the field is "" — harmless.

- [ ] **Step 2: Sweep host children.** Add `HostClient client.Client` to `Sweeper`. In `sweepChildren`,
  after sweeping provider children, if `s.HostClient != nil` read `data.hostChildGVKs` and delete host
  children by the same instance-uid label via `s.HostClient` (mirror the provider loop; factor a
  `sweepChildrenVia(ctx, cl, gvkRaw, instanceUID)` helper used for both planes). The liveness record
  stays in the provider workspace.

- [ ] **Step 3: Wire.** `main.go` sets `Sweeper{..., HostClient: hostClient}`.

- [ ] **Step 4: Tests.** Extend the sweeper unit test: a stale record with `hostChildGVKs` sweeps host
  children through a fake host client; nil `HostClient` skips host sweep without error. Run
  `go test ./internal/controller/...`.

- [ ] **Step 5: Commit** `feat(controller): sweep orphaned host children`.

---

### Task 7: Envtest e2e

**Files:**
- Create: `internal/controller/externalref_e2e_test.go`, `internal/controller/host_target_e2e_test.go`
  (follow the existing `dynamic_e2e_test.go` / `dualtarget_test.go` harness in
  `internal/controller/dynamic_harness_test.go`)
- Create fixtures under `test/fixtures/` as needed

- [ ] **Step 1: External-ref e2e.** Pre-create a VPC-like namespaced CR (a simple CRD or an existing
  kind) in the consumer workspace with a `status` value. Blueprint: `externalRef` (target consumer) for
  the VPC + a consumer child template consuming `${vpc.status.<field>}`. Assert the child materializes
  with the funneled value; assert the VPC object is unchanged and still present after the instance is
  deleted (never GC'd). Run under `make test`.

- [ ] **Step 2: Host-target e2e.** Point the Reconciler's `HostClient` at the envtest apiserver (same
  cluster is fine for the test). Blueprint with a `target: host` child. Assert: the child is created with
  a `ProviderChildName`-style collision-free name and the GC labels; removing it from the desired set
  prunes it; a stale liveness record carrying `hostChildGVKs` causes the Sweeper to delete it.

- [ ] **Step 3: Run** `make test`. Fix drift. **Commit** `test: e2e for external refs + host target`.

---

### Task 8: Documentation, ADR, chart

**Files:**
- Modify: `docs/blueprints.md`, `docs/permissions.md`, `docs/architecture.md`, `README.md`,
  `docs/guides/writing-your-first-blueprint.md`, `docs/guides/cross-target-dependencies.md`
- Create: `docs/decisions/0011-external-refs-and-host-target.md`
- Modify: `docs/decisions/0002-per-resource-target-annotation.md` (mark superseded by 0011)
- Modify: `charts/krop-controller/values.yaml`, deployment template, `config/kcp/rbac/*` notes

- [ ] **Step 1: Blueprint docs.** Document the `target` field (values, default consumer) replacing the
  annotation; document `externalRef` (single + collection) and the VPC/VM cross-plane example. Update
  every doc/README snippet that shows the old `krop.opendefense.cloud/target` annotation to the `target`
  field (`grep -rn "opendefense.cloud/target" docs/ README.md`). Keep the ADR files' historical prose.

- [ ] **Step 2: Permissions docs.** Read-only claims for consumer external refs; host-cluster RBAC is the
  provider's responsibility (out of krop's least-privilege scope); `--host-kubeconfig` / in-cluster host
  client; the foreign-export-binding invariant for external-ref identityHash.

- [ ] **Step 3: Architecture + ADR.** Add the three-plane composition diagram. Write ADR 0011 (why the
  per-resource `target` field over annotation/namespace-prefix/top-level-map; host client sourcing;
  identityHash invariant). Mark ADR 0002 superseded.

- [ ] **Step 4: Chart.** Add a `hostKubeconfig` value + optional host-kubeconfig secret mount (mirror the
  existing kcp-kubeconfig mount) rendering `--host-kubeconfig`; note the in-cluster default. `helm lint`.

- [ ] **Step 5: Commit** `docs: external refs, host target, target field, ADR 0011`.

---

## Self-review notes

- **Spec coverage:** externalRef read (Task 3) ✓, host target (Task 5) ✓, external-ref claims (Task 4) ✓,
  target field replacing annotation (Tasks 2–3) ✓, host orphan sweep (Task 6) ✓, thorough tests (unit in
  each task + e2e Task 7) ✓, docs (Task 8) ✓.
- **Green at each task:** Task 1–2 additive; Task 3 is the atomic cutover (deletes annotation, migrates
  all fixtures, updates all callers in one commit); 4–8 build on green.
- **Type consistency:** routing map is `map[string]kropengine.Target` end to end (`ToKro` yields
  `map[string]string`, converted once in the registrar via `ParseTarget`). `Reader`/`ClientReader`,
  `TargetForNode`, `TargetHost` names are used identically across tasks.
