# KCP-Native Composition Controller (RGD Engine) — Design Spec

**Status:** Design / high-level. Intended to be implementable as a standalone component
(possibly in a separate repo). Grounded in two feasibility spikes (kro-in-kcp; api-syncagent)
and a kro v0.9.2 source audit performed 2026-07-06.

**Goal:** A **kcp-aware controller** that lets a provider author a declarative *blueprint*
(a kro `ResourceGraphDefinition`) in a **provider workspace**, publishes that blueprint as a
bindable **APIExport**, and — for every instance a consumer creates — materializes a graph of
child resources **split across two destinations**: the **consumer workspace** (reached through
the export's virtual workspace) *and* the **provider workspace**. It reuses kro's graph/CEL/runtime
engine as a **library** and replaces kro's single-cluster runtime with kcp-native machinery.

**Non-goal:** Re-implementing kro's expression language, schema shorthand, or DAG builder — those
are imported from kro. This spec is about the *kcp runtime* around that engine.

---

## 1. Motivation

### 1.1 Why not vanilla kro on kcp
A spike proved kro **does** reconcile against a single kcp workspace endpoint (it treats the
workspace's path-prefixed URL as an ordinary apiserver; no code change). But kro's model is
fundamentally single-endpoint:

- **kro's manager binds to one endpoint (one workspace).** Serving *N* tenant workspaces means *N*
  kro deployments (or a fan-out you build yourself). kro has no notion of the kcp *virtual
  workspace* that aggregates all bound consumers behind one endpoint.
- **kro creates a local CRD + a per-RGD micro-controller** to serve its generated API. In kcp you
  publish an **APIExport**, not a CRD; consumers get the type by **APIBinding**.
- **kro applies every child into the single cluster it runs against.** It cannot place some children
  in the consumer workspace and others in the provider workspace.

### 1.2 Why a blueprint (not a hand-written controller)
The product goal is that a provider offers a service (e.g. "a Kubernetes cluster with access")
**without writing a Go controller** — just a declarative RGD. This matches the access-model's D23
("no custom RBAC-projection controller; kcp claims + declarative objects") and the reserved
"order CRD (kro/Crossplane-style) is a separate sub-project" note in the non-escalating-provider
spec. The RGD *is* the offering.

### 1.3 Why dual-target (the crux)
A single order must produce resources in **two ownership domains**:

- **Consumer-workspace resources** — the tenant-facing objects the order grants (in the access
  model: `Scope`, `DelegationGrant`, `ResourceEnrollment`, provider `Role`/`ClusterRole`,
  `CapabilityMapping`). These are written into the *tenant* workspace, authorized by the tenant's
  acceptance of the export's permission claims.
- **Provider-workspace resources** — fulfilment/bookkeeping the *provider's* own downstream consumes
  (in the access model: an `AgentRequest` that a service-cluster syncer turns into a running agent;
  also quota, accounting, provisioning records). These live where the provider has authority.

One blueprint, two destinations, with dependencies (CEL references) allowed to cross between them.

---

## 2. KCP mechanics this design relies on (verified)

All confirmed against **kcp v0.32.3** during the spikes.

- **APIExport / APIResourceSchema.** A provider publishes an `APIExport` that references one or more
  `APIResourceSchema`s. Consumers gain the types by creating an `APIBinding` to the export.
- **APIExport virtual workspace.** A controller connecting to the export's virtual-workspace
  endpoint (`.../services/apiexport/<identityHash>/<group>`) sees **all instances of the exported
  types across every bound workspace**, each tagged by its logical-cluster name. This is the
  **fan-in** that removes kro's "one deployment per workspace" limit.
- **APIExportEndpointSlice.** Names the concrete virtual-workspace URLs; a controller discovers it to
  construct its provider. (The URL is empty until at least one `APIBinding` exists.)
- **multicluster-provider / multicluster-runtime.** `apiexport.Provider` built from the endpoint
  slice + an `mcmanager.Manager` yields a **per-logical-cluster client** via
  `mgr.GetCluster(ctx, clusterName).GetClient()`. *This is the exact pattern access-operator's
  `cmd/controller/main.go` already uses* — reuse it.
- **permissionClaims + identityHash.** To read/write a resource type your APIExport does **not**
  itself export (a *foreign* type — e.g. access-model types owned by another APIExport, or core
  `ConfigMap`s), your `APIExport.spec.permissionClaims` lists that GroupResource with the target
  export's `identityHash` (from its `.status.identityHash`). The consumer's `APIBinding` must
  **accept** those claims. Acceptance is the consent *and* the scoped write grant, revocable by
  deleting the binding.
- **Vanilla CRDs work inside a workspace** (used to seed helper CRDs if needed).

---

## 3. Vocabulary

| Term | Meaning |
|---|---|
| **Blueprint** | The declarative composition = a kro `ResourceGraphDefinition` (reused type), authored by the provider. |
| **Instance** | A CR of the blueprint-generated API kind, created by a consumer. Handled as `unstructured`. |
| **Provider workspace** | Where the blueprint is authored and its APIExport is published. |
| **Consumer workspace** | A workspace that bound the blueprint's APIExport and creates instances. |
| **Target** | Per-resource destination: `consumer` (default) or `provider`. |
| **Engine** | This controller. |

---

## 4. Architecture

```
┌─ Provider workspace ────────────────────────────┐
│  Blueprint (RGD)  ──────────────┐               │
│  APIExport (engine-published)   │  ▲ provider-   │
│    schema = generated instance  │  │ target      │
│    permissionClaims = foreign   │  │ children    │
│      types the blueprint writes │  │ (AgentReq…) │
│  APIExportEndpointSlice         │  │             │
└─────────────────┬───────────────┼──┼─────────────┘
                  │ virtual ws     │  │
                  │ (fan-in over   │  │
   ┌──────────────▼── all bound ───┼──┼──────────┐
   │  ENGINE (this controller)     │  │          │
   │  • Registrar: RGD → APIExport │  │          │
   │  • Instance reconciler        │  │          │
   │    (embeds kro runtime)       │  │          │
   │  • Cross-ws GC / finalizer    │  │          │
   └──────────────┬────────────────┼──┼──────────┘
                  │ per-instance    │  │
                  ▼ (consumer ws)   │  │
┌─ Consumer workspace ─────────────▼──┼────────────┐
│  APIBinding → blueprint APIExport   │            │
│    (accepts permissionClaims)       │ consumer-  │
│  Instance (the order)               │ target     │
│  consumer-target children ◄─────────┘ children   │
│    (Scope, DelegationGrant, …)                   │
└──────────────────────────────────────────────────┘
```

Three sub-components:

1. **Blueprint Registrar** — watches blueprints in provider workspaces; publishes each as an
   APIExport (§6).
2. **Instance Reconciler** — the RGD engine core; runs kro's runtime and applies children to the
   correct target (§7–§9).
3. **Lifecycle / GC manager** — cross-workspace finalizer + label-based garbage collection (§11).

---

## 5. The blueprint model

Reuse kro's RGD authoring surface verbatim: `spec.schema` (SimpleSchema shorthand → the instance
API's OpenAPI schema), `spec.resources[]` (each with `id`, `template`, and kro's `readyWhen`,
`includeWhen`, `forEach`, `${...}` CEL). **One extension is required: per-resource target
placement.**

### 5.1 Target placement — recommended shape
Introduce a thin **superset CRD** (the blueprint authors write *this*, not a raw kro RGD) that reuses
kro's imported types and adds placement:

```yaml
apiVersion: composition.<domain>/v1alpha1
kind: CompositionBlueprint          # engine-owned; embeds a kro RGD
metadata: { name: kubernetescluster }
spec:
  schema:                            # kro SimpleSchema, verbatim
    apiVersion: v1alpha1
    kind: KubernetesCluster
    spec: { region: string, tier: string | default="standard" }
    status: { agentReady: boolean }
  # kro resources, verbatim (id/template/readyWhen/includeWhen/forEach/CEL):
  resources:
    - id: scope
      target: consumer               # ← the one added field (default: consumer)
      template: { apiVersion: access.opendefense.cloud/v1alpha1, kind: Scope, ... }
    - id: agentRequest
      target: provider               # ← provider-workspace child
      template: { apiVersion: fulfil.<domain>/v1alpha1, kind: AgentRequest,
                  spec: { scopeRef: "${scope.metadata.name}", region: "${schema.spec.region}" } }
  # foreign GroupResources this blueprint writes into the CONSUMER workspace,
  # surfaced as the APIExport's permissionClaims (see §8):
  consumerClaims:
    - { group: access.opendefense.cloud, resource: scopes }
    - { group: access.opendefense.cloud, resource: delegationgrants }
    - { group: access.opendefense.cloud, resource: resourceenrollments }
    - { group: access.opendefense.cloud, resource: roles }
    - { group: access.opendefense.cloud, resource: clusterroles }
    - { group: access.opendefense.cloud, resource: capabilitymappings }
```

The engine extracts the embedded kro RGD (the `schema` + `resources` minus `target`/`consumerClaims`)
and hands it to kro's `Builder` unchanged. **`target` and `consumerClaims` are engine-only** and
never reach kro or the applied objects.

> **Alternative considered:** a per-resource annotation on the kro resource template
> (`composition.<domain>/target: provider`) stripped before apply. Keeps a raw kro RGD as the
> authored artifact but is less legible and risks leaking the annotation. The superset CRD is
> cleaner and keeps kro's type pristine. *This is an implementer decision — see §16.*

### 5.2 Cross-target dependencies
A `consumer`-target resource may reference a `provider`-target resource's status (or vice versa) via
CEL (`${agentRequest.status.tokenSecret}`). The engine feeds **observed state from both workspaces**
into the runtime's evaluation context (§7), so the topological order and CEL resolution are global
across targets. The engine must resolve the full DAG regardless of where each node lands.

---

## 6. Blueprint → APIExport publication (Registrar)

On blueprint create/update in a provider workspace:

1. **Build the graph.** Parse the embedded RGD into kro's `api/v1alpha1.ResourceGraphDefinition`
   and call `graph.Builder.NewResourceGraphDefinition(rgd, cfg)`. This validates naming, builds the
   instance CRD from SimpleSchema, extracts CEL, builds + cycle-checks the DAG, and returns a
   `*graph.Graph` whose `Graph.CRD` is the generated instance CRD.
   - **Coupling to plan around (verified):** `graph.NewBuilder(*rest.Config, httpClient)` needs a
     `rest.Config` that can serve **discovery/OpenAPI for every child GVK** (kro type-checks CEL
     against child schemas at build time). Point it at a workspace that has all child CRDs/APIs
     bound (the provider workspace bound to the access-operator export + the provider's own types).
     The `Builder.schemaResolver`/`restMapper` fields are interfaces but **unexported** — to inject a
     fully kcp-aware resolver without a working `rest.Config`, upstream a small exported constructor
     to kro (fields are already interfaces). Graph build is **per-blueprint, not per-instance**, so
     this cost is amortized.
2. **Synthesize the APIResourceSchema.** Convert `Graph.CRD`'s OpenAPI schema into an
   `apis.kcp.io` `APIResourceSchema` (name convention: `v<hash>.<plural>.<group>`, matching kcp's
   own scheme). One schema per blueprint schema-version.
3. **Publish/patch the APIExport** in the provider workspace, referencing the latest
   `APIResourceSchema`, and set `spec.permissionClaims` from the blueprint's `consumerClaims`
   (each resolved to its target export's `identityHash` — see §8). Maintain an
   `APIExportEndpointSlice` for the engine to connect to.
4. **Write status** back on the blueprint: exported API name, the APIExport `identityHash`,
   and `Ready`/error conditions.

Schema evolution: on blueprint schema change, mint a **new** `APIResourceSchema` and point the
APIExport's latest-version reference at it; existing instances continue under their bound version
(kcp handles multi-version serving). Optionally reuse kro's `GraphRevision` concept for
compiled-graph versioning (see §16).

---

## 7. Instance reconcile (the engine loop)

### 7.1 Manager wiring
For each published blueprint APIExport, run a controller over its virtual workspace:

- Build `apiexport.Provider` from the blueprint's `APIExportEndpointSlice`; wrap in an
  `mcmanager.Manager`. Register a `mcreconcile.Func` `For(<generated instance GVK>)`. Reuse
  access-operator's `main.go` shape 1:1.
- One engine process can host **multiple** such controllers (one per blueprint/endpoint slice), or
  run one process per blueprint. *See §16.*

### 7.2 Per-instance reconcile
`req.ClusterName` = the **consumer** workspace where the instance lives.

1. **Clients.**
   - `consumerClient = mgr.GetCluster(req.ClusterName).GetClient()` — via the blueprint's virtual
     workspace it sees the exported instance type **and** the permission-claimed foreign types in
     that consumer workspace.
   - `providerClient` — a client to the **provider** workspace (the blueprint's home logical
     cluster, a fixed known value per blueprint). Direct kcp client (the engine's own identity has
     RBAC there).
2. **Instantiate the engine.** `rt, _ := runtime.FromGraph(graph, instanceUnstructured, cfg)`
   (kro; client-free).
3. **Drive the loop** in topological order over `rt.Nodes()`:
   - `node.IsIgnored()` → skip if `includeWhen` is false.
   - `objs, _ := node.GetDesired()` → the concrete child object(s) (multiple for `forEach`),
     with CEL already resolved against currently-observed dependencies.
   - **Route by target:** look up the node's `id` in the placement map → apply each object with
     `consumerClient` or `providerClient` via **server-side apply** (field manager
     `composition-engine`), into the appropriate namespace (§9.1 naming).
   - **Read back** the applied object from the same target client and `node.SetObserved(observed)`
     so downstream nodes' CEL resolves — **including cross-target references** (a consumer node
     reading a provider node's status works because both observed states populate the one runtime
     context).
   - `node.CheckReadiness()` → feed per-resource readiness into instance status.
4. **Aggregate status** onto the instance (`consumerClient`): conditions
   (`Ready`, `Progressing`), per-resource state, and any `status.*` fields the blueprint maps from
   child statuses via CEL.
5. **Requeue** while any node is not-yet-ready or a cross-target dependency is unresolved.

The kro runtime never touches a cluster — the engine owns *all* I/O and target routing. This is
what makes dual-target apply possible with an unmodified engine.

---

## 8. Writing into the consumer workspace (permissionClaims)

- The **instance type** is exported by the blueprint's APIExport → natively visible/writable in the
  virtual workspace.
- **Consumer-target children of foreign types** (types the blueprint's APIExport does not export —
  e.g. access-model `Scope`/`Role`/…, or core `ConfigMap`) require the blueprint's APIExport to
  declare `permissionClaims` for each GroupResource, referencing the owning export's `identityHash`
  (access-operator's, for access-model types). The consumer's `APIBinding` must **accept** them.
- The blueprint author enumerates these in `spec.consumerClaims` (§5.1); the Registrar resolves each
  to an `identityHash` (read from the target APIExport's `.status.identityHash` in the workspace
  where it is published) and writes the `permissionClaims` block.
- **Consequence — consent & blast radius:** a consumer that binds the blueprint and accepts its
  claims has *authorized* the engine to write exactly those GroupResources into its workspace, and
  nothing else. Revoking = deleting the binding. Claims are per-type per-workspace today (blast
  radius = one tenant); kcp *claim selectors* (roadmap) will narrow this to specific objects/labels.
  (Same widening note as access-model D23.)

---

## 9. Writing into the provider workspace

- **Provider-target children** are created in the provider workspace with `providerClient`, where the
  engine's identity already has authority (it owns the APIExport there). **No permissionClaim
  needed** — same ownership domain.
- **Use cases:** fulfilment requests the provider's downstream consumes (e.g. an `AgentRequest`
  that a service-cluster syncer/`api-syncagent` turns into a running agent), quota/accounting
  records, provisioning state.

### 9.1 Naming & multi-tenancy
Provider-target children from *many* consumers land in the *one* provider workspace → they **must**
be collision-free. Template names/namespaces from the consumer identity + instance, e.g.
`name = <consumer-cluster-id-short>-<instance-name>` (mirrors kro's naming templates and the
api-syncagent hashing pattern proven in the spike). Consumer-target children live in their own
consumer workspace and can keep natural names.

---

## 10. Identity & RBAC

- **Consumer side:** the engine acts through the blueprint APIExport's virtual workspace as the
  **APIExport owner identity**, extended by the accepted `permissionClaims`. It can CRUD the exported
  instances and the claimed foreign types across all bound consumer workspaces — and *nothing the
  claims don't cover*. Bounded, consent-based blast radius.
- **Provider side:** the engine needs RBAC in the provider workspace for the provider-target child
  GVKs (it authors them directly).
- **Trust story:** the engine writes on behalf of the provider; consumers consented by accepting the
  claims. This is the same authorization spine as access-model D23 — no bespoke owner-stamping or
  mutating webhook.

---

## 11. Lifecycle & garbage collection (cross-workspace)

Kubernetes owner references are **cluster/workspace-local**, so they cannot own provider-target
children from a consumer-workspace instance. Design GC explicitly:

- **Consumer-target children** live in the same workspace as the instance → *owner refs work there*;
  use them for in-workspace GC + prune.
- **Provider-target children** are in a different workspace → **no owner ref**. Track them with a
  deterministic label set: `composition.<domain>/instance-uid`, `.../consumer-cluster`,
  `.../blueprint`.
- **Finalizer** on the instance. On delete: enumerate + delete provider-target children by label in
  the provider workspace (and confirm consumer-target children are GC'd), then remove the finalizer.
- **Drift/prune** (a resource removed from a later blueprint revision): reimplement a **label-based
  apply-set prune** (kro uses `ApplySet` + a dynamic client, which is single-cluster and in the
  dropped layer). Track the set of applied identities per instance per target; prune the difference.
- **Orphan risk:** if a consumer unbinds the APIExport mid-life (claims revoked), the engine loses
  visibility of that workspace. Provider-target children then need a **sweep** keyed off provider-side
  records (e.g. a periodic reconcile that deletes provider children whose consumer instance is no
  longer observable). Document this as a known operational concern.

---

## 12. Embedding kro — concrete integration map

Audited against **kro v0.9.2** (`github.com/kubernetes-sigs/kro`, Apache-2.0, Go 1.26+). **No
`internal/` packages anywhere** — the engine layers are importable as-is.

| kro package | Role | Use? |
|---|---|---|
| `api/v1alpha1` | `ResourceGraphDefinition`, `Schema`, `Resource` types | **Use** — parse/construct blueprints in memory |
| `pkg/simpleschema` | `ToOpenAPISpec(obj, customTypes)` → `JSONSchemaProps` (pure) | **Use** — SimpleSchema → OpenAPI |
| `pkg/graph/crd` | `SynthesizeCRD(...)` → full CRD (pure) | **Use** — generated instance CRD → APIResourceSchema |
| `pkg/graph` | `NewBuilder(*rest.Config,…)`, `Builder.NewResourceGraphDefinition(rgd,cfg) → *Graph` | **Use** — DAG build (needs discovery config; see §6.1) |
| `pkg/graph/parser` | `ExtractExpressions`, schema-aware template parse | **Use** (via Builder) |
| `pkg/cel` | `Expression{Program}.Eval(ctx map[string]any)` (pure, precompiled, thread-safe) | **Use** (via runtime) |
| `pkg/runtime` (+ `resolver`) | `FromGraph(g, instance, cfg)`; `Node.GetDesired/SetObserved/CheckReadiness/IsIgnored/DeleteTargets` — **client-free** | **Use** — the reconcile-one-instance core |
| `pkg/controller/instance` | single-cluster reconciler: ApplySet apply/prune, finalizers, owner-ref GC, status, `dynamic.Interface`+`RESTMapper` bound to one cluster | **Drop / rewrite** — this is the two-workspace loop we build. Its `resources.go` drive loop is the best reference blueprint. |
| `pkg/controller/resourcegraphdefinition` | creates the local CRD + spins per-RGD micro-controller | **Drop** — replaced by APIExport publication (§6) |
| `pkg/dynamiccontroller` | per-GVR dynamic informers/workqueue, watch coordinator (single-cluster) | **Drop** — replaced by kcp virtual-workspace informers |
| `pkg/controller/graphrevision`, `api/internal.kro.run/v1alpha1` | compiled-graph versioning | **Optional** — see §16 |

**Key property (verified):** `pkg/runtime` imports no client-go / controller-runtime / dynamic
client. Dependency values arrive purely via `SetObserved`, and `GetDesired` returns bare
`unstructured` objects. The engine chooses the target endpoint per object → **dual-target apply
needs no kro fork**.

**Versions to pin:** kro `v0.9.2`, Go `1.26+`, `cel-go v0.27`, `k8s.io v0.35.x`,
kcp `v0.32.3` (+ `multicluster-provider v0.8`, `multicluster-runtime`), matching what the spikes
ran. api-syncagent `v0.7.0` if the provider-target downstream uses it.

---

## 13. Failure modes & edge cases

- **Schema evolution:** new blueprint schema → new `APIResourceSchema`; existing instances stay on
  their bound version. Never mutate a served schema in place.
- **Unresolved cross-target dependency:** a node's CEL references a not-yet-ready sibling → requeue;
  don't apply partial. Surface as `Progressing`.
- **Partial apply across two targets:** there is **no** cross-workspace transaction. Make every apply
  idempotent (SSA), record per-resource status, and converge on requeue. A provider-target apply may
  succeed while a consumer-target apply is rejected (claim not accepted) — report both, don't roll
  back.
- **Discovery gap at build:** a child GVK whose schema the build `rest.Config` can't resolve → the
  Builder fails; the blueprint goes `Ready=False`. Ensure the build endpoint has all child schemas.
- **Consumer unbinds mid-reconcile / provider ws unreachable:** treat as transient; the GC sweep
  (§11) handles permanent unbinding.
- **Naming collisions in the provider workspace:** enforce the templated naming of §9.1.
- **Claim `identityHash` drift:** if a target APIExport is re-created its `identityHash` changes;
  the Registrar must re-resolve and re-publish claims.

---

## 14. Testing strategy

- **Unit (engine loop):** feed a blueprint + instance to kro's Builder/runtime; assert the desired
  child objects, their target routing, and CEL resolution — with two **fake** clients (consumer /
  provider). No kcp needed.
- **Cross-target dependency test:** provider-target child publishes a status field that a
  consumer-target child consumes via CEL; assert ordering + value propagation.
- **Registrar test:** blueprint → assert generated `APIResourceSchema` + `APIExport`
  (`permissionClaims` resolved from `consumerClaims`) + endpoint slice.
- **Integration (real kcp v0.32.3):** provider workspace with a blueprint → APIExport published;
  consumer workspace binds + accepts claims; create an instance → assert consumer-target children in
  the consumer workspace **and** provider-target children in the provider workspace; delete the
  instance → assert GC in both. (This is the promised "kcp e2e §13" validation for the access
  model, generalized.)
- **Negative:** consumer that does **not** accept a claim → the corresponding consumer-target apply
  is rejected by kcp; assert the engine reports it and does not leak.

---

## 15. Motivating example — the access-model offering (appendix)

The concrete blueprint that drove this design (`KubernetesCluster` offering):

- **consumer-target:** `Scope`, `DelegationGrant`, `ResourceEnrollment`, provider `Role`/`ClusterRole`,
  `CapabilityMapping` → written into the tenant workspace.
- **provider-target:** `AgentRequest` → written into the provider workspace, where a service-cluster
  path (`api-syncagent` → a second kro/RGD or projector) turns it into a running Teleport agent that
  joins the target cluster.

This single blueprint exercises the access model's three untested axes end-to-end:
**permissionClaims** (the tenant's APIBinding acceptance authorizing the consumer-target writes),
**delegation** (access-operator later follows the written `DelegationGrant` to aggregate provider
bindings), and **capabilities** (resource-scope-only rendering against the written
`CapabilityMapping`). It replaces the "kro-per-workspace" deployment the earlier spike exposed as
awkward.

---

## 16. Open questions for the implementer

1. **Target placement encoding:** superset `CompositionBlueprint` CRD (recommended, §5.1) vs.
   per-resource annotation on a raw kro RGD. Trade legibility/pristine-kro against "author a raw RGD".
2. **Process topology:** one engine process hosting many per-blueprint controllers vs. one process
   per blueprint. The former is simpler to operate; the latter isolates blast radius and schema-build
   configs.
3. **kro Builder schema resolution:** point `NewBuilder` at a schema-complete workspace endpoint
   (no fork) vs. upstream a small exported constructor to inject a kcp-aware
   `SchemaResolver`/`RESTMapper` (cleanest long-term; fields already interfaces, just unexported).
4. **GraphRevision reuse:** adopt kro's compiled-graph versioning for blueprint evolution, or roll a
   simpler per-schema-version scheme.
5. **Prune fidelity:** how aggressively to prune drifted/removed nodes across workspaces, and how to
   reconcile that with the §11 orphan sweep.
6. **Claim-selector adoption:** when kcp claim selectors land, narrow `permissionClaims` from
   per-type to per-object to shrink the consumer blast radius.
