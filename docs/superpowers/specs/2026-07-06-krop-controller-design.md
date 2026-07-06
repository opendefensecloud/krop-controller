# krop-controller — Design & Roadmap

**Date:** 2026-07-06
**Status:** Approved design; decomposition + roadmap. Supersedes decisions left open in `idea.md` §16.
**Companion:** `idea.md` (the full architecture spec this roadmap operationalizes).

`krop-controller` is a **kcp-native composition controller**: a provider authors a declarative
blueprint (a kro `ResourceGraphDefinition`), the controller publishes it as a bindable `APIExport`,
and for every consumer-created instance it materializes a graph of children **split across two
workspaces** — the **consumer** workspace (via the export's virtual workspace) and the **provider**
workspace. It reuses kro's graph/CEL/runtime engine *as a library* and replaces kro's single-cluster
runtime with kcp machinery.

This document records the decisions made during brainstorming and the milestone roadmap. The
architecture rationale lives in `idea.md`; here we capture **what was decided** and **the build
order**.

---

## 1. Decisions locked in

### 1.1 API group
Everything owned by this project lives under **`krop.opendefense.cloud`** — the engine's blueprint
CRD, generated instance types' routing/GC annotations and labels.

### 1.2 Blueprint type: our own CRD, kro-identical schema (resolves `idea.md` §16.1)
The provider authors a **`ResourceGraphDefinition` CRD served by us under
`krop.opendefense.cloud/v1alpha1`**, whose schema is **identical to kro's RGD** (generated from kro's
Go types). We own and install this CRD; there is **no dependency on kro's operator or CRDs** being
present. In Go, the Registrar parses the object into kro's `api/v1alpha1.ResourceGraphDefinition`
struct (same shape) and feeds it to kro's `Builder` unchanged.

Rejected alternatives: watching the literal `kro.run` RGD (cedes the group to kro, requires kro's
CRD installed); a superset CRD embedding a raw RGD (extra wrapper type to maintain).

### 1.3 Target placement: per-resource annotation (resolves `idea.md` §16.1)
Routing is expressed as an annotation **`krop.opendefense.cloud/target: consumer|provider`** placed
in each `resources[].template.metadata.annotations`. kro treats it as a literal annotation, so it
appears on the desired object returned by `Node.GetDesired()`. The engine reads the target **off the
object itself** and **strips the annotation in the SSA apply path**. Default when absent: `consumer`.

Consequences:
- **No separate placement-map.** Routing travels with the object; there is exactly one place
  (the apply path) that reads the target and strips the annotation — one place a leak could occur,
  one place to guard.
- The authored artifact stays a structurally-pristine kro RGD; kro's type is untouched.

### 1.4 permissionClaims are auto-derived (simplifies `idea.md` §5.1)
Because every consumer-target resource's GVK is present in its template, the Registrar **computes the
APIExport's `permissionClaims` by scanning consumer-target templates** — resolving each foreign
GroupResource to its owning APIExport's `identityHash`. The hand-maintained `consumerClaims` field
from `idea.md` §5.1 is **removed**, along with its keep-in-sync failure mode.

### 1.5 Routing annotations vs GC labels are distinct
- **Routing** = an *annotation* on the template, **stripped before apply**.
- **GC tracking** (`krop.opendefense.cloud/instance-uid`, `.../consumer-cluster`, `.../blueprint`) =
  *labels* added by the engine at apply time and **kept** on the applied object.

### 1.6 GraphRevision: skip the machinery, build a compiled-graph cache (resolves `idea.md` §16.4)
Grounded in a source read of kro **v0.9.2**:
- kro's `GraphRevision` (`internal.kro.run/v1alpha1`, cluster-scoped CRD) stores an *immutable
  snapshot of the source RGD spec* + a monotonic `revision` number — **not** the compiled graph. The
  compiled graph lives in a process-global in-memory `revisions.Registry` keyed by
  `(RGDName, Revision)`; the spec hash is stored as a *label*.
- The instance reconciler calls **`GetLatestRevision()` only**; `GetGraphRevision(rev)` is unused and
  instances hold no revision reference. **Pinning and rollback are latent infra, not realized
  behavior in v0.9.2** — instances always run latest.
- All of it (issuance, GC via `--rgd-max-graph-revisions` default 5, resolver) lives in
  `pkg/controller/resourcegraphdefinition` + `pkg/controller/instance` — both on our **Drop** list
  (`idea.md` §12). `runtime.FromGraph`, which we keep, takes a plain `*graph.Graph` and knows nothing
  about revisions.
- The registry is keyed **without a workspace dimension** — identical blueprint names across provider
  workspaces would collide in one process. Owner-ref adoption, cluster-scoped CRD, and
  list-by-selectable-field do not transfer to kcp.

**Decision:** Do **not** reuse kro's GraphRevision. Instead build a **workspace-aware compiled-graph
cache** keyed by `(provider-workspace, blueprint, specHash)` in the Registrar (M4). This is required
regardless — graph build is the expensive `idea.md` §6.1 operation and per-instance reconcile is hot
— and it delivers kro's *real* value (change-detection + caching) natively. Defer the observable
revision-history CRD and rollback/pinning; adopting them later (a workspace-keyed revision store) is a
layer-on, not a rewrite, and we are not behind kro.

### 1.7 Other `idea.md` §16 defaults
| §16 | Question | Decision | Milestone |
|---|---|---|---|
| .2 | Process topology | One process hosting **many per-blueprint controllers**; construct controllers per-blueprint from day one so splitting to one-process-per-blueprint later is cheap | arch from M1; confirmed M6 |
| .3 | kro Builder schema resolution | **Point `NewBuilder` at a schema-complete workspace endpoint (no fork)**; upstream an exported constructor only if the M0 spike shows it can't work | decided by M0 spike |
| .5 | Prune fidelity | Conservative **label-based apply-set**: track applied identities per instance *per target*, prune the diff; reconcile with the orphan sweep | M5 |
| .6 | Claim selectors | **Deferred** until kcp ships them | post-roadmap |

### 1.8 Sequencing strategy: runtime-first (A)
Retire the biggest technical risk (kro Builder under kcp) in a throwaway spike first, then build the
dual-target Instance Reconciler against a **hand-written** APIExport, and automate the Registrar only
once the thing it feeds is proven. Gets a demonstrable dual-target reconcile earliest.

---

## 2. What is reused vs built

**Reused as a proven pattern** (from the sibling `access-operator`, a *separate* project — read-only
reference, no shared module): the `apiexport.Provider` → `mcmanager.Manager` → `mcreconcile.Func(For(...))`
wiring, `APIExportEndpointSlice` discovery (`internal/kcp/endpointslice.go`), per-logical-cluster
client access (`mgr.GetCluster(ctx, req.ClusterName).GetClient()`), the `apigen` CRD→APIResourceSchema
flow (for **our own** engine CRD only), and the `envtest`-based kcp e2e harness shape.

**Reused as a library** (kro v0.9.2, no `internal/` packages): `api/v1alpha1`, `pkg/simpleschema`,
`pkg/graph/crd` (`SynthesizeCRD`), `pkg/graph` (`Builder`), `pkg/cel`, `pkg/runtime`
(`FromGraph`, `Node.GetDesired/SetObserved/CheckReadiness/IsIgnored`). **Dropped:**
`pkg/controller/{instance,resourcegraphdefinition,graphrevision}`, `pkg/dynamiccontroller`.

**Key divergence from access-operator:** its API is static (Go types → `apigen` at build time). Ours
is **dynamic** — the instance `APIResourceSchema` is synthesized at *runtime* from a blueprint by the
Registrar. `apigen` applies only to our own `ResourceGraphDefinition` engine CRD.

---

## 3. Milestone roadmap

Linear spine `M0 → M1 → M2 → M3 → M5 → M6`; **M4** depends only on M0 + a target for its output, so it
is the one milestone that could slide earlier or run in parallel — runtime-first keeps it after M3.
Each milestone is its own later spec → plan → implement cycle.

### M0 — Scaffold + kro-embedding spike (de-risk; throwaway spike code)
- Repo skeleton (`api/ cmd/ internal/ config/ test/`); pin deps. **kro `v0.9.2` drives the base set:
  it transitively pins k8s `v0.35.0`, cel-go `v0.27.0`, controller-runtime `v0.23.1` — kept as-is (do
  not force-bump; see M0 findings). kcp sdk `v0.32`, multicluster-provider `v0.8`,
  multicluster-runtime land in M1, at which point the whole set is re-resolved together.** Adapt the copied
  Makefile (strip the hardcoded `access.opendefense.cloud` APIExport wiring + `access-operator` image
  names; rename to `krop-controller`).
- **Spike:** confirm `runtime.FromGraph` + `Node.GetDesired/SetObserved` is genuinely client-free with
  fake data (no cluster); probe whether `graph.NewBuilder` can build a graph pointed at a
  schema-complete endpoint vs. needing an upstreamed exported constructor.
- **Exit:** a decision note picking the kro-Builder approach + a test that builds/drives a graph with
  **no cluster**. Retires the §6.1 risk.

### M1 — Instance Reconciler, single-target walking skeleton (first e2e proof)
- Hand-write one APIExport + APIResourceSchema for a trivial fixed blueprint, checked into
  `config/kcp`. **No Registrar yet.**
- Multicluster manager wired from access-operator's `main.go` shape; reconcile the instance GVK over
  the virtual workspace. Build the graph once at startup; drive
  `GetDesired → SSA apply (consumerClient) → read back → SetObserved → CheckReadiness`. **Consumer
  target only.** Status aggregation (`Ready`/`Progressing`).
- **Exit:** create an instance in a consumer ws → one consumer-target child materializes → status
  `Ready` (e2e via the envtest harness).

### M2 — Dual-target apply (the crux)
- Add `providerClient` (direct kcp client to the blueprint's home ws). Per-resource target routing off
  the stripped annotation; collision-free provider-child naming (`idea.md` §9.1). Hand-write
  `permissionClaims` on the APIExport; consumer `APIBinding` accepts; prove a *foreign*-type consumer
  child (a `Scope`) writes.
- **Exit:** one instance → consumer child in consumer ws **and** provider child in provider ws.

### M3 — Cross-target CEL + status mapping
- Feed observed state from **both** targets into the one runtime context so `${providerNode.status.x}`
  resolves cross-target. Global topological order; requeue on unresolved cross-target deps; blueprint
  `status.*` mapped from child statuses.
- **Exit:** provider child publishes a status field a consumer child consumes; ordering + value
  propagation asserted; partial apply reported, not rolled back.

### M4 — Blueprint Registrar (automate publication; replaces hand-written APIExport)
- Define the `krop.opendefense.cloud/v1alpha1` `ResourceGraphDefinition` CRD (kro-identical schema;
  `apigen` applies here). Registrar watches blueprints → parse into kro's RGD struct → `graph.Builder`
  → `SynthesizeCRD` → `APIResourceSchema` → publish/patch `APIExport` + `EndpointSlice` → **auto-derive
  `permissionClaims`** from consumer-target templates (resolve each to `identityHash`) → status back on
  the blueprint. **Compiled-graph cache** keyed by `(provider-workspace, blueprint, specHash)`.
  Schema evolution mints a new ARS, keeps old versions.
- **Exit:** authoring a blueprint auto-produces a bindable APIExport the M1–M3 reconciler serves
  **unchanged**.

### M5 — Lifecycle & cross-workspace GC
- Instance finalizer; label-set tracking of provider children; on delete enumerate+delete provider
  children by label, confirm consumer children GC via owner refs, drop finalizer. Label-based
  apply-set prune for drift (track applied identities per instance per target). Periodic orphan sweep
  for mid-life unbind.
- **Exit:** delete instance → clean in both ws; negative: unaccepted claim → rejected, reported, no
  leak.

### M6 — Hardening & topology
- Confirm the single-process/many-per-blueprint-controllers topology; `identityHash` drift
  re-resolution; full failure-mode coverage; observability; docs; the generalized `idea.md` §14
  integration e2e using the access-model `KubernetesCluster` offering as the fixture.

---

## 4. Primary risks

- **kro Builder under kcp (§6.1):** `NewBuilder` needs a `rest.Config` serving discovery/OpenAPI for
  every child GVK; `schemaResolver`/`restMapper` are interfaces but *unexported*. Retired by the M0
  spike; fallback is a small upstream PR exporting a constructor.
- **No cross-workspace transaction:** a provider-target apply may succeed while a consumer-target apply
  is rejected. Every apply is idempotent (SSA), per-resource status recorded, convergence on requeue —
  never rolled back (M2/M3/M5).
- **Orphan children on mid-life unbind:** consumer unbinds → engine loses workspace visibility →
  provider children need a sweep keyed off provider-side records (M5).
- **Provider-workspace name collisions:** many consumers' provider children share one workspace —
  enforced templated naming (`idea.md` §9.1, M2).
