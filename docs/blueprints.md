# Authoring blueprints

A **blueprint** is a `krop.opendefense.cloud/v1alpha1 ResourceGraphDefinition`
(RGD). You author one; krop compiles it into a bindable `APIExport` and reconciles
the generated instance kind across every consumer that binds it. This page is the
authoring reference: the RGD shape, krop's one addition (target routing), CEL,
auto-derived permissionClaims, naming/pruning, schema evolution, and a fully
annotated example.

If you have not run the end-to-end walkthrough yet, do
[getting-started.md](getting-started.md) first. For the internals, see
[architecture.md](architecture.md).

---

## The `ResourceGraphDefinition` shape

krop's RGD **is** [kro](https://kro.run)'s RGD, served under our own group. The
`spec` is parsed into kro's type unchanged (`api/v1alpha1/resourcegraphdefinition_types.go`);
routing lives in per-resource annotations, not in a forked schema. The object is
**cluster-scoped** in the provider workspace.

```yaml
apiVersion: krop.opendefense.cloud/v1alpha1   # krop's group…
kind: ResourceGraphDefinition                 # …but kro's RGD spec verbatim
metadata:
  name: kubernetescluster                      # blueprint name (cluster-scoped)
spec:
  schema:        # the generated instance API: its spec + status (kro SimpleSchema)
    apiVersion: v1alpha1
    kind: KubernetesCluster
    group: krop.opendefense.cloud
    spec:
      region: string
    status:
      configMapName: ${config.metadata.name}
      agentToken: ${agentRequest.status.token}
  resources:     # the graph of child objects the instance materializes
    - id: agentRequest
      template: { ... }
    - id: config
      template: { ... }
```

### `spec.schema` — the generated instance API

This is a kro **SimpleSchema**. `group`/`version`/`kind` name the API krop
generates and serves through the APIExport; `spec` is the instance's input
surface; `status` is a set of CEL projections krop writes back onto each instance
(see [CEL](#cel-expressions)). SimpleSchema uses shorthand types (`string`,
`integer`, `boolean`, `[]string`, nested objects, `string | default=...`,
`string | required=true`); the fuller SimpleSchema reference lives in kro's docs.

### `spec.resources[]` — the child graph

Each resource is a node in a DAG that krop derives from the `${...}` references
between nodes (kro builds and topologically orders it). Per-resource fields:

| Field | Meaning |
| --- | --- |
| `id` | node identifier; also the CEL handle other nodes reference (`${config.status...}`) |
| `template` | the child object to apply (any GVK; a full manifest with `${...}` CEL) |
| `readyWhen` | CEL predicate(s) that must hold before the node counts as ready and dependents proceed |
| `includeWhen` | CEL predicate(s) gating whether the node is created at all |
| `forEach` | expand the node into one child per collection element |

`readyWhen`/`includeWhen`/`forEach`/`${...}` are all standard kro semantics —
krop does not change them.

---

## The one krop addition: `target` routing

krop resolves each child to a **workspace** using a single annotation on the
resource's `template.metadata.annotations`:

```yaml
metadata:
  annotations:
    krop.opendefense.cloud/target: consumer   # or: provider
```

| Value | Where the child is written | Authorized by | Identity |
| --- | --- | --- | --- |
| `consumer` (**default**, if the annotation is absent/empty) | the **consumer** (tenant) workspace | an **accepted** permissionClaim on the consumer's APIBinding | the APIExport **virtual-workspace** identity |
| `provider` | the **provider** workspace (`default` namespace) | the controller's own provider-workspace RBAC | the controller's **own** kcp identity |

`consumer` and `provider` are the only valid values; any other value is a graph
build error (`internal/engine/route.go`). The annotation is **stripped before
apply** so it never leaks onto the materialized object.

**Why two targets.** A single instance often needs to write some state into the
tenant's own workspace (things the tenant should see and the claim authorizes) and
some state into the provider's back-office workspace (fulfilment requests the
tenant should never touch). Consumer-target children are the tenant-visible face;
provider-target children are the provider-side plumbing. See
[decisions/0002-per-resource-target-annotation.md](decisions/0002-per-resource-target-annotation.md).

Provider-target children are in the **same ownership domain** as the controller,
so they involve **no permissionClaim**. Consumer-target children cross a workspace
boundary and therefore require the claim handshake below.

---

## CEL expressions

Templates and `schema.status` use kro's `${...}` CEL. The handles krop supports:

- **`${schema.spec.*}`** — the instance's own input, e.g.
  `${schema.spec.region}`.
- **`${<resourceId>.status.*}`** (and `.metadata.*`, `.spec.*`) — read another
  node's live object. This works **cross-resource and cross-target**: a
  **consumer**-target child can read a **provider**-target child's status. In the
  example, the consumer `ConfigMap` reads `${agentRequest.status.token}` from the
  provider-target `AgentRequest`.
- **`schema.status.*`** — CEL projections written back onto each instance's
  `.status`. In the example, `configMapName: ${config.metadata.name}` and
  `agentToken: ${agentRequest.status.token}`.

**Cross-target dependencies pend, they do not fail.** If a consumer child reads a
provider child's status that is not set yet, that child **pends** — the reconcile
reports "not complete", requeues (~30s), and the instance's `Ready` condition
stays `False`/`Progressing` until the referenced status appears. Only then does
the consumer child materialize. (This is exactly the token flow in the
walkthrough.) Because pending suppresses pruning of not-yet-applied children,
a partially-converged instance never has its work reclaimed mid-flight — see
[operations.md](operations.md#garbage-collection--the-orphan-sweep).

---

## Auto-derived permissionClaims

You do **not** hand-write claims. The Registrar scans the blueprint's
**consumer-target** children, and for every GroupResource that is *not* the
instance's own type it emits one `permissionClaim` (verbs
`get,list,watch,create,update,patch,delete`), sorted for stable publications
(`internal/registrar/claims.go`). Those claims are published on the APIExport.

Two rules follow from this:

1. **The consumer must ACCEPT the claim.** A cross-workspace (consumer-target)
   write is authorized only when the consumer's APIBinding sets the matching claim
   to `state: Accepted`. A `Rejected` (or absent) claim → the child is silently
   never written. This is by design; see the negative fixture
   `test/fixtures/apibinding-kubernetescluster-noclaim.yaml`.

2. **Foreign (non-core) consumer-target types must be bound in the provider
   workspace.** A claim for a core type (group `""`, e.g. `configmaps`)
   legitimately carries an empty identity hash. But a claim for a *foreign* group
   needs that group's `identityHash` to resolve — which requires the owning
   APIExport to be **bound in the provider workspace**. If it is not, publish
   **fails** with condition reason **`ClaimIdentityUnresolved`** rather than
   emitting a silently-broken claim (`internal/registrar/claims.go`
   `validateClaims`). Fix: bind the foreign type's export in the provider
   workspace, and the next resync republishes.

See [decisions/0004-auto-derived-permission-claims.md](decisions/0004-auto-derived-permission-claims.md)
and [permissions.md](permissions.md#the-permissionclaims-spine).

---

## Provider-child naming (collision-free)

Many consumers' provider-target children land in **one** provider workspace, so
krop cannot use the template's literal `metadata.name` — two tenants both creating
`eu-agent` would collide. Instead every provider-target child is renamed
deterministically to `<cluster>-<instance>-<originalName>-<hash>`, where the hash
is a short SHA-256 of the null-joined `(consumerCluster, instanceName,
originalName)` tuple, truncated to fit 253 chars (`internal/engine/naming.go`).
The rename is collision-free across consumers and stable across reconciles, so the
same instance always addresses the same provider child.

Consumer-target children keep their literal name — they live in the consumer's own
workspace, so there is no cross-tenant collision.

> **CEL reads still work across the rename.** `${agentRequest.status.token}`
> resolves against the live renamed object; you reference the node by its `id`,
> not its on-cluster name.

---

## `includeWhen` / `forEach` and pruning

krop reconciles the *desired* child set on every pass and **prunes** labeled
children that are no longer desired (only after a **complete** pass, so pending
children are never reclaimed). This means:

- An `includeWhen` that flips to false → its child is pruned.
- A `forEach` collection that shrinks → the removed elements' children are pruned.
- A resource removed from the blueprint → its children are pruned.

Pruning runs per target (consumer children in the consumer workspace, provider
children in the provider workspace). See
[architecture.md](architecture.md#42-instance-reconcile-dual-target--cross-target-cel).

---

## Schema evolution (live blueprint edits)

Editing a live blueprint's `spec` is supported: the Registrar detects the new spec
hash and **restarts** the per-blueprint instance manager to serve the *new*
compiled graph (change-detected `Stop`+`Ensure`,
[decisions/0009-change-detected-manager-restart.md](decisions/0009-change-detected-manager-restart.md)).
An unchanged 5-minute resync does **not** restart anything.

⚠️ **Caveat — single-version serving.** This is not multi-version serving. An
**incompatible** schema change on an in-place version is not served alongside the
old shape; existing instances are re-served under the new graph. Proper
multi-version serving (existing instances stay on their bound version) is a
documented future enhancement — see
[architecture.md](architecture.md#7-known-limitations--future-work). For breaking
changes, prefer publishing under a new `version`/`kind`.

---

## Worked example (annotated)

`test/fixtures/blueprint-kubernetescluster-rgd.yaml` — a `KubernetesCluster` that
requests an agent (provider-side) and exposes a config (consumer-side) carrying
the agent's token:

```yaml
apiVersion: krop.opendefense.cloud/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: kubernetescluster
spec:
  schema:
    apiVersion: v1alpha1
    kind: KubernetesCluster
    group: krop.opendefense.cloud
    spec:
      region: string                              # instance input
    status:
      configMapName: ${config.metadata.name}      # projection: the consumer child's name
      agentToken: ${agentRequest.status.token}     # projection: the provider child's live status
  resources:
    # (1) PROVIDER-target child: written into the provider workspace by the
    #     controller's own identity; renamed collision-free; no permissionClaim.
    - id: agentRequest
      template:
        apiVersion: fulfil.krop.opendefense.cloud/v1alpha1
        kind: AgentRequest
        metadata:
          name: ${schema.spec.region}-agent
          namespace: default
          annotations:
            krop.opendefense.cloud/target: provider
        spec:
          region: ${schema.spec.region}
    # (2) CONSUMER-target child (default): written into the consumer workspace
    #     through the APIExport vw, authorized by the accepted `configmaps` claim.
    #     Reads the PROVIDER child's status cross-target → PENDS until it is set.
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
          token: ${agentRequest.status.token}       # cross-target CEL read
```

Publishing this blueprint yields:

- an `APIExport` named `kubernetesclusters.krop.opendefense.cloud` serving the
  `KubernetesCluster` kind,
- one auto-derived `configmaps` permissionClaim (the only foreign consumer-target
  GroupResource; `AgentRequest` is provider-target, so it needs **no** claim — it
  needs provider-workspace RBAC instead, which the operator grants),
- and, per instance, an `AgentRequest` in the provider workspace + a `ConfigMap`
  in the consumer workspace once the token is set.

### What the operator must do for *your* blueprint

Every provider-target GVK your blueprint emits needs a matching rule in the
provider-workspace RBAC (`config/kcp/rbac/provider-rbac.yaml`) — the shipped rule
only covers this example's `AgentRequest`. Never widen it to `*`/`*`. See
[operations.md](operations.md#rbac--identity) and
[permissions.md](permissions.md).

---

## Checklist for a new blueprint

- [ ] `spec.schema` defines the instance `group`/`version`/`kind`, its input
      `spec`, and any `status` CEL projections.
- [ ] Every resource has an `id`, a `template`, and (if not consumer) an explicit
      `krop.opendefense.cloud/target: provider` annotation.
- [ ] Every foreign consumer-target type's export is **bound in the provider
      workspace** (else publish fails `ClaimIdentityUnresolved`).
- [ ] Every provider-target GVK has a least-privilege rule in
      `provider-rbac.yaml` (never `*`/`*`).
- [ ] Every provider-target child CRD is **served in the provider workspace**
      before the blueprint is created (kro type-checks CEL against it).
- [ ] Consumers know to bind the export **accepting** the derived claims.
