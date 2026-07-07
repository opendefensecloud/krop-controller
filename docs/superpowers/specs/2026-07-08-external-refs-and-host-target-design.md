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

## Blueprint API change: the `routing` map

External refs **cannot** carry the `krop.opendefense.cloud/target` annotation (kro's
`ExternalRefMetadata` has no annotations field, and our CRD embeds kro's spec verbatim). To route them,
add a krop-owned `routing` map, inlined into our spec so everything stays under `spec` and kro's spec
stays embedded and untouched:

```go
// api/v1alpha1/resourcegraphdefinition_types.go
type ResourceGraphDefinitionSpec struct {
    krov1alpha1.ResourceGraphDefinitionSpec `json:",inline"`
    // Routing maps a kro resource `id` to a krop target: "consumer" | "provider" | "host".
    // It is the ONLY way to route externalRef resources (which cannot carry the per-template
    // target annotation). For template resources the annotation still applies; a routing entry,
    // if present, OVERRIDES it. Absent entry ⇒ existing default (annotation, else consumer).
    Routing map[string]string `json:"routing,omitempty"`
}
```

```yaml
spec:
  resources:
    - id: vpc
      externalRef: {apiVersion: net.example/v1, kind: VPC, metadata: {name: ${schema.spec.vpcName}}}
    - id: vm
      template: { metadata: {annotations: {krop.opendefense.cloud/target: host}}, ... }
  routing:
    vpc: consumer
```

**Consequences of inlining** (mechanical, all in `internal/registrar` + `internal/engine/graphsource`):
`bp.Spec` becomes our wrapper; kro-spec consumers read `bp.Spec.ResourceGraphDefinitionSpec`.
`SpecHash` hashes the **whole** wrapper (routing included) so a routing-only change bumps the hash and
republishes/restarts. controller-gen regenerates deepcopy + the CRD manifest.

**Target resolution unifies** behind one helper, replacing the two current call sites that read
`TargetOf(node.Template)` and the engine's per-object `TargetOf(obj)`:

```
TargetForNode(node, routing):
  if routing[node.Meta.ID] present -> parse+validate it        (authoritative; externalRefs + overrides)
  else                             -> TargetOf(node.Template)  (annotation; default consumer)
```

Per-node resolution is equivalent to today's per-object resolution (a node's forEach objects all share
one template annotation) and matches how `childGVKs` already resolves target off the node.

---

## Component changes

### 1. Engine — read-only branch + `Reader` (`internal/engine`)

- New `Target`: `TargetHost Target = "host"`. `TargetOf` accepts it; validation lists all three.
- New `Reader` interface (read side of I/O, mirroring `Applier`):
  ```go
  type Reader interface {
      Get(ctx, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error)
      List(ctx, gvk schema.GroupVersionKind, namespace string, selector labels.Selector) ([]*unstructured.Unstructured, error)
  }
  ```
  Backed by a `client.Client` per target (`ClientReader`). Registered as `map[Target]Reader`.
- `Reconcile(ctx, rt, appliers, readers, routing)` — per node:
  - resolve `target := TargetForNode(node, routing)`.
  - switch `node.Spec.Meta.Type`:
    - `NodeTypeExternal`: `readers[target].Get(gvk, desired.Namespace, desired.Name)` →
      `SetObserved([obj])`; **NotFound ⇒ `Ready=false, Requeue=true`, continue** (mirror `ErrDataPending`).
    - `NodeTypeExternalCollection`: `List(gvk, ns, selector)` → `SetObserved(items)`.
    - default (`Resource`/`Collection`/`Instance`): existing applier path, unchanged.
  - external nodes are **never** applied, labeled, owned, renamed, or recorded (no `ChildID`).
  - `CheckReadiness()` after observe, same as regular nodes.
- `Result.Complete` semantics unchanged: a not-found external ref sets `Requeue` and returns before
  `Complete` is set (prune stays disabled while any dependency — external or otherwise — is pending).

### 2. GC / prune — exclude external nodes (`internal/controller/reconciler.go`)

- `childGVKs`, `deleteChildren`, `pruneChildren` must **skip external nodes** (we don't own them; deleting
  them would destroy provider infrastructure). Gate on `node.Spec.Meta.Type` (external ⇒ skip) in
  `childGVKs` (the single enumeration all three use).
- `childGVKs`/prune/delete gain the `TargetHost` → `HostClient` entry (see §4).
- Target resolution in `childGVKs` switches to `TargetForNode(node, routing)`.

### 3. Permission claims — read-only for external consumer refs (`internal/registrar/claims.go`)

Split consumer-target nodes by whether they are writable (template) or external (read-only):

| node | target | claim |
|------|--------|-------|
| template, consumer | consumer | CRUD claim (existing) |
| externalRef, consumer | consumer | **read-only** claim: `get, list, watch` |
| template/externalRef, provider or host | — | **no claim** (own client + own/host RBAC) |

- `ForeignConsumerGRs` → returns two sets (writable, external) or annotates each GR with a verb set.
- `DeriveClaims` emits `claimVerbs` (CRUD) for writable and a new `readOnlyVerbs = {get,list,watch}` for
  external GRs. IdentityHash resolution is **unchanged**: a foreign external CRD type still needs its
  owning APIExport bound in the provider workspace to resolve its hash; `validateClaims` still rejects an
  unresolved foreign claim. Core types (empty group) carry empty identityHash and are allowed.
  This preserves the existing invariant and is documented as a known constraint (the provider must bind
  the foreign export it wants to read, exactly as for foreign writes today).

### 4. Host target — third write plane (`cmd/controller/main.go`, `internal/controller`)

- **Host client:** `rest.InClusterConfig()` by default; `--host-kubeconfig <path>` flag overrides
  (dev / out-of-cluster). If neither yields a config, the host client is **nil**: no host applier/reader
  is registered, and a blueprint routing to host fails loudly with the engine's existing
  "no applier configured for target host" error (fail-closed, no silent drop).
- **Host applier chain** mirrors provider (collision-free names across consumers, GC labels):
  `LabelingApplier(QualifyingApplier(ProviderChildName)(RecordingApplier(SSAApplier(hostClient))))`.
  Host children keep the template's namespace; `ProviderChildName` (cluster+instance+name hash) makes
  names collision-free within it. No `OwnerRefApplier` (cross-cluster, like provider).
- **Host reader:** `ClientReader{hostClient}` registered at `TargetHost`.
- **Reconciler** gains `HostClient client.Client` (nil ⇒ host maps omitted). Appliers/readers/childGVKs/
  prune/delete maps include host when non-nil.

### 5. Orphan sweep — host children must not leak (`internal/controller`)

Host children orphan the same way provider children do (consumer unbinds mid-life ⇒ finalizer never
fires). Extend the liveness mechanism:

- Liveness record gains a `hostChildGVKs` data field (same serialization as `providerChildGVKs`), written
  by `writeLivenessRecord` from `childGVKs(TargetHost)`.
- `Sweeper` gains `HostClient client.Client` (nil ⇒ host sweep skipped). `sweepChildren` sweeps provider
  children via `ProviderClient` and host children via `HostClient`, each by the recorded GVK list +
  instance-uid label. The record itself stays in the provider workspace (single source of truth).
- Wiring: `cmd/controller/main.go` passes `HostClient` to both `Reconciler` and `Sweeper`.

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

**Unit (`internal/engine`, `internal/registrar`):**
- Engine external branch: `NodeTypeExternal` Get → observed feeds downstream CEL; NotFound ⇒
  `Ready=false,Requeue=true` and **no** apply/label/record; `NodeTypeExternalCollection` List by selector.
- `TargetForNode`: routing entry overrides annotation; external node with no routing entry defaults
  consumer; invalid target value errors.
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
  (stale liveness record with `hostChildGVKs`).

**Full-stack (optional, `test/e2e`):** extend one spec with a host-target child once the envtest path is
green; not required for the feature to land.

---

## Documentation

- `docs/blueprints.md`: `externalRef` + the `routing` map, with the VPC/VM example.
- `docs/permissions.md`: read-only claims for consumer external refs; host-cluster RBAC is the provider's
  responsibility; `--host-kubeconfig` / in-cluster host client.
- `docs/architecture.md`: three-plane composition diagram (read plane + host write plane).
- ADR `docs/decisions/0011-external-refs-and-host-target.md`: why the `routing` map (kro schema
  constraint), host client sourcing, and the identityHash invariant for external refs.
- Chart: optional `--host-kubeconfig` value + host-kubeconfig secret mount (mirrors the existing
  kcp-kubeconfig mount); in-cluster default needs no host ClusterRole from *us* (provider-managed).

---

## Risks / decisions

- **`routing` adds a blueprint field.** Forced by kro's fixed externalRef schema; the only in-schema home
  for external-ref routing. Templates keep annotations (no migration); routing overrides when present.
- **External-ref identityHash constraint** (provider must bind the foreign export to read it) is inherited
  from the existing foreign-write model, not new — documented, not relaxed here.
- **Host orphan sweep** must ship with the host target, or host children leak on mid-life unbind. Included.
- **Host client nil ⇒ fail-closed:** blueprints routing to host error clearly rather than silently drop.
