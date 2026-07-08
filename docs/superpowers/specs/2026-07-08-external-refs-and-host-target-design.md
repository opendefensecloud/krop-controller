# External Refs + Host Target — Design

**Goal:** Let a blueprint (a) **read** existing objects it did not create, from any of the three
planes (consumer / provider / host), via kro `externalRef`, and (b) **write** children into a third
plane — the physical **host** cluster the controller runs in — reusing the provider-target machinery.
Together these turn krop into a flexible api-syncagent replacement: it can pull an input from one plane
and funnel it (via kro CEL) into a child on another.

**Motivating example:** provision a VM into the host cluster that must spawn inside an existing VPC
living in the consumer workspace. The blueprint declares the VPC as an `externalRef` (consumer,
read-only), reads `${vpc.status.vpcId}`, and funnels it into the VM `template` routed to `host`.

**Non-goals:** API projection / re-export of host resources back into kcp; forced host-namespace
isolation; relaxing the existing "provider must bind a foreign export to resolve its identityHash"
invariant. RBAC in the host cluster is the provider's responsibility and explicitly out of scope as a
concern (per product direction).

---

## Background: what kro v0.9.2 already gives us (verified)

- A resource entry is **either** `template` **or** `externalRef` (mutually exclusive, enforced by kro's
  own CRD validation). `externalRef` = `{apiVersion, kind, metadata:{name|selector, namespace}}`.
- The graph builder tags such nodes `graph.NodeTypeExternal` (single, by `metadata.name`) or
  `graph.NodeTypeExternalCollection` (by `metadata.selector`), surfaced on `node.Spec.Meta.Type`
  alongside `node.Spec.Meta.GVR` and `node.Spec.Meta.Namespaced`.
- `node.GetDesired()` resolves the synthetic desired for an external node — CEL in the name / namespace
  / selector (`${schema.spec.*}`, other nodes) resolves exactly as for templates. So funneling
  `${vpc.status.vpcId}` into a downstream child is already native (same cross-target CEL proven in M3).
- kro's own read path is trivial and is the pattern we mirror: `Get(name)` (or `List(selector)`) →
  `node.SetObserved([...])` → `node.CheckReadiness()`; **not-found ⇒ wait, never apply, never error.**
  (See `pkg/controller/instance/resources_external.go`.)

The engine's current per-node loop only drives the **apply-then-read** path, so external nodes today
would be wrongly routed into the applier chain. The core change is a **read-only branch** in that loop.

---

## Blueprint API change: per-resource `target` field

External refs cannot carry the `krop.opendefense.cloud/target` template annotation (kro's
`ExternalRefMetadata` is only name/namespace/selector, and kro's `Resource` has no metadata block). To
route them — and to unify routing for templates too — add a krop-owned per-resource `target` field. Our
CRD wraps kro's tiny spec (only `Schema` + `Resources`), inlining kro's `Resource` and adding one
enum-validated field:

```go
// api/v1alpha1/resourcegraphdefinition_types.go
type Resource struct {
    krov1alpha1.Resource `json:",inline"`   // id/template/externalRef/readyWhen/includeWhen/forEach reused
    // Target routes this resource's object(s): consumer (default) | provider | host.
    // +kubebuilder:validation:Enum=consumer;provider;host
    // +optional
    Target string `json:"target,omitempty"`
}

type ResourceGraphDefinitionSpec struct {
    Schema    *krov1alpha1.Schema `json:"schema,omitempty"`
    Resources []*Resource         `json:"resources,omitempty"`
}

// ToKro strips target into a routing map (resource id -> target) and returns clean
// kro types for the graph builder. A missing/empty target defaults to consumer downstream.
func (s ResourceGraphDefinitionSpec) ToKro() (krov1alpha1.ResourceGraphDefinitionSpec, map[string]string)
```

```yaml
spec:
  resources:
    - id: vpc
      target: consumer                       # omitted ⇒ consumer (default)
      externalRef: { apiVersion: net.example/v1, kind: VPC, metadata: { name: ${schema.spec.vpcName} } }
    - id: vm
      target: host
      template: { ... }
```

This **replaces** the template annotation mechanism. `target` is the single routing surface for all
resources; the `krop.opendefense.cloud/target` annotation plus `TargetOf` / `StripRouting` /
`TargetAnnotation` are removed, and existing fixtures / the example blueprint / docs migrate to the field.

**Consequences** (all mechanical): our CRD spec becomes a thin wrapper over kro's; kro-spec consumers
call `bp.Spec.ToKro()`. `SpecHash` hashes our wrapper (target included) so a routing change bumps the
hash and republishes/restarts. controller-gen regenerates deepcopy + the CRD manifest (now carrying the
enum-validated `target`). CRD enum validation rejects bad targets at apply time.

**Target resolution** unifies behind the build-time routing map (`resourceID -> Target`), default
consumer, replacing every `TargetOf(...)` call:

```
targetForNode(nodeID, routing):
  if routing[nodeID] set -> that Target
  else                   -> TargetConsumer
```

The routing map is threaded to the engine, the Reconciler (childGVKs / prune / delete), and claims
derivation — all keyed by resource id (== `node.Spec.Meta.ID`).

---

## Component changes

### 1. Engine — read-only branch + `Reader` + routing (`internal/engine`)

- `Target` gains `TargetHost Target = "host"`; `TargetConsumer` / `TargetProvider` unchanged.
- Remove `TargetOf` / `StripRouting` / `TargetAnnotation` (annotation routing is gone). Add
  `TargetForNode(id string, routing map[string]Target) Target` (default consumer) and a
  `ParseTarget(string) (Target, error)` used by `ToKro`/validation.
- New `Reader` interface (read side of I/O, mirroring `Applier`):
  ```go
  type Reader interface {
      Get(ctx, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error)
      List(ctx, gvk schema.GroupVersionKind, namespace string, selector labels.Selector) ([]*unstructured.Unstructured, error)
  }
  ```
  Backed by a `client.Client` per target (`ClientReader`). Registered as `map[Target]Reader`.
- `Reconcile(ctx, rt, appliers, readers, routing)` — per node:
  - `target := TargetForNode(node.Spec.Meta.ID, routing)`.
  - switch `node.Spec.Meta.Type`:
    - `NodeTypeExternal`: `readers[target].Get(gvk, desired.Namespace, desired.Name)` →
      `SetObserved([obj])`; **NotFound ⇒ `Ready=false, Requeue=true`, continue** (mirror `ErrDataPending`).
    - `NodeTypeExternalCollection`: `List(gvk, ns, selector)` → `SetObserved(items)`.
    - default (`Resource`/`Collection`/`Instance`): existing applier path (no `StripRouting` — target
      now comes from routing, not the object).
  - external nodes are **never** applied, labeled, owned, renamed, or recorded (no `ChildID`).
  - `CheckReadiness()` after observe, same as regular nodes.
- Missing applier/reader for a target ⇒ hard error (fail-closed), as today.

### 2. GC / prune — exclude external nodes, add host (`internal/controller/reconciler.go`)

- `childGVKs`, `deleteChildren`, `pruneChildren` must **skip external nodes** (`node.Spec.Meta.Type` is
  external ⇒ skip; we don't own them, deleting them would destroy provider infrastructure).
- `childGVKs` resolves target via `TargetForNode(node.Spec.Meta.ID, r.Routing)` (replacing
  `TargetOf(node.Template)`).
- delete/prune target→client maps gain `TargetHost` → `HostClient` (when non-nil).
- Reconciler gains `Routing map[kropengine.Target]...` — actually `Routing map[string]kropengine.Target`
  (id→target) — and `HostClient client.Client`.

### 3. Permission claims — read-only for external consumer refs (`internal/registrar/claims.go`)

Split consumer-target nodes by writable (template) vs external (read-only):

| node | target | claim |
|------|--------|-------|
| template, consumer | consumer | CRUD claim (existing) |
| externalRef, consumer | consumer | **read-only** claim: `get, list, watch` |
| template/externalRef, provider or host | — | **no claim** (own client + own/host RBAC) |

- `ForeignConsumerGRs(g, instanceGR, routing)` returns writable + external GR sets (keyed off
  `node.Spec.Meta.Type` and the routing map).
- `DeriveClaims` emits `claimVerbs` (CRUD) for writable GRs and `readOnlyVerbs = {get,list,watch}` for
  external GRs. IdentityHash resolution is **unchanged**: a foreign external CRD type still needs its
  owning APIExport bound in the provider workspace; `validateClaims` still rejects an unresolved foreign
  claim. Core types (empty group) carry empty identityHash and are allowed. Documented as the inherited
  foreign-binding invariant (unchanged from foreign writes today).

### 4. Host target — third write plane (`cmd/controller/main.go`, `internal/controller`)

- **Host client:** `rest.InClusterConfig()` by default; `--host-kubeconfig <path>` flag overrides
  (dev / out-of-cluster). If neither yields a config, the host client is **nil**: no host applier/reader
  is registered, and a blueprint routing to host fails loudly with the engine's existing
  "no applier/reader configured for target host" error (fail-closed, no silent drop).
- **Host applier chain** mirrors provider (collision-free names across consumers, GC labels):
  `LabelingApplier(QualifyingApplier(ProviderChildName)(RecordingApplier(SSAApplier(hostClient))))`.
  Host children keep the template's namespace; `ProviderChildName` (cluster+instance+name hash) makes
  names collision-free within it. No `OwnerRefApplier` (cross-cluster, like provider).
- **Host reader:** `ClientReader{hostClient}` registered at `TargetHost`.
- **Reconciler** gains `HostClient client.Client` (nil ⇒ host maps omitted).

### 5. Orphan sweep — host children must not leak (`internal/controller`)

Host children orphan the same way provider children do (consumer unbinds mid-life ⇒ finalizer never
fires). Extend the liveness mechanism:

- Liveness record gains a `hostChildGVKs` data field (same serialization as `providerChildGVKs`), written
  by `writeLivenessRecord` from `childGVKs(TargetHost)`.
- `Sweeper` gains `HostClient client.Client` (nil ⇒ host sweep skipped). `sweepChildren` sweeps provider
  children via `ProviderClient` and host children via `HostClient`, each by the recorded GVK list +
  instance-uid label. The record itself stays in the provider workspace (single source of truth).
- `cmd/controller/main.go` passes `HostClient` to both `Reconciler` and `Sweeper`.

---

## Data flow (VPC → VM)

```
consumer ws:  VPC (externalRef, read-only claim)  --Get-->  node.observed
                                                                  |
kro CEL:                         ${vpc.status.vpcId} -----------> |
                                                                  v
host cluster: VM (template, target=host) <--SSA-- QualifyingApplier(ProviderChildName)+labels
                                                                  |
                              liveness record (provider ws) tracks host+provider child GVKs
                                                                  |
                              Sweeper reclaims host+provider children on mid-life unbind
```

---

## Testing

**Unit (`internal/engine`, `internal/registrar`, `api/v1alpha1`):**
- `ToKro`: builds routing map from `target`; empty target absent from map (⇒ consumer default);
  round-trips kro fields; invalid target rejected (also caught by CRD enum, but validate in code too).
- Engine external branch: `NodeTypeExternal` Get → observed feeds downstream CEL; NotFound ⇒
  `Ready=false,Requeue=true` and **no** apply/label/record; `NodeTypeExternalCollection` List by selector.
- `TargetForNode`: routing entry honored; missing id ⇒ consumer.
- Host applier chain: rename + labels + SSA to the host client; recorded for prune.
- Claims: consumer external ref ⇒ read-only verbs; consumer template ⇒ CRUD; provider/host external ⇒ no
  claim; foreign external with unresolved identity ⇒ `validateClaims` rejects.
- GC: `childGVKs` excludes external nodes (no delete/prune of externals).

**Envtest e2e (`internal/controller`, real kcp 0.30.0):**
- Pre-create a VPC-like CR in the consumer workspace; blueprint declares it `externalRef` (consumer),
  funnels `${vpc.status.*}` into a child; assert the child receives the value and the VPC is never
  modified/deleted (survives instance delete).
- Host target: point the host client at the envtest apiserver; assert a `host`-routed child is created
  with a collision-free name + GC labels, is pruned on removal, and is swept on simulated mid-life orphan
  (stale liveness record carrying `hostChildGVKs`).

**Full-stack (optional, `test/e2e`):** extend one spec with a host-target child once the envtest path is
green; not required for the feature to land.

---

## Documentation

- `docs/blueprints.md`: the `target` field + `externalRef`, with the VPC/VM example; note annotation
  routing is replaced by `target`.
- `docs/permissions.md`: read-only claims for consumer external refs; host-cluster RBAC is the provider's
  responsibility; `--host-kubeconfig` / in-cluster host client.
- `docs/architecture.md`: three-plane composition diagram (read plane + host write plane).
- ADR `docs/decisions/0011-external-refs-and-host-target.md`: why the per-resource `target` field (vs
  annotation / namespace-prefix / top-level map), host client sourcing, and the identityHash invariant.
- Chart: optional `--host-kubeconfig` value + host-kubeconfig secret mount (mirrors the existing
  kcp-kubeconfig mount); in-cluster default needs no host ClusterRole from *us* (provider-managed).

---

## Risks / decisions

- **`target` replaces the annotation.** Single explicit routing surface, uniform for templates +
  externalRefs, enum-validated at the CRD. Existing blueprints/fixtures/docs migrate (pre-release, low
  cost). Default when omitted is consumer (back-compatible default).
- **External-ref identityHash constraint** (provider must bind the foreign export to read it) is inherited
  from the existing foreign-write model, not new — documented, not relaxed here.
- **Host orphan sweep** ships with the host target, or host children leak on mid-life unbind. Included.
- **Host client nil ⇒ fail-closed:** blueprints routing to host error clearly rather than silently drop.
