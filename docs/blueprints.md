# Authoring blueprints

A **blueprint** is a `krop.opendefense.cloud/v1alpha1 ResourceGraphDefinition`
(RGD). You author one; krop compiles it into a bindable `APIExport` and reconciles
the generated instance kind across every consumer that binds it. This page is the
authoring reference: the RGD shape, krop's additions (the `target` routing field
and `externalRef` reads), CEL, auto-derived permissionClaims, naming/pruning,
schema evolution, and a fully annotated example.

If you have not run the end-to-end walkthrough yet, do
[getting-started.md](getting-started.md) first. For the internals, see
[architecture.md](architecture.md).

---

## The `ResourceGraphDefinition` shape

krop's RGD is [kro](https://kro.run)'s RGD wrapped in a thin krop spec: the
`spec` is kro's `Schema` + `Resources`, and each resource inlines kro's resource
verbatim plus **one** krop-owned field — `target`
(`api/v1alpha1/resourcegraphdefinition_types.go`). A build-time `ToKro()`
conversion strips `target` back into a routing map and hands the graph builder
pristine kro types. The object is **cluster-scoped** in the provider workspace.

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
| `target` | **krop addition.** Which plane the node's object(s) route to: `consumer` (default) \| `provider` \| `host`. See [target routing](#the-target-routing-field) |
| `template` | the child object to **write** (any GVK; a full manifest with `${...}` CEL). Mutually exclusive with `externalRef` |
| `externalRef` | **read-only.** An existing object krop reads but never creates or GCs. Mutually exclusive with `template`. See [external references](#externalref-reading-objects-krop-does-not-own) |
| `readyWhen` | CEL predicate(s) that must hold before the node counts as ready and dependents proceed |
| `includeWhen` | CEL predicate(s) gating whether the node is created at all |
| `forEach` | expand the node into one child per collection element |

`readyWhen`/`includeWhen`/`forEach`/`${...}`/`template`/`externalRef` are all
standard kro semantics — krop does not change them; `target` is krop's only added
field.

---

## The `target` routing field

krop resolves each resource to a **plane** using a single per-resource `target`
field (a sibling of `id`/`template`, **not** a template annotation):

```yaml
resources:
  - id: config
    target: consumer          # or: provider | host — omit for the consumer default
    template: { ... }
```

| Value | Where the object is written / read | Authorized by | Identity |
| --- | --- | --- | --- |
| `consumer` (**default**, if `target` is absent/empty) | the **consumer** (tenant) workspace | an **accepted** permissionClaim on the consumer's APIBinding | the APIExport **virtual-workspace** identity |
| `provider` | the **provider** workspace (`default` namespace) | the controller's own provider-workspace RBAC | the controller's **own** kcp identity |
| `host` | the **physical host cluster** the controller runs in | RBAC in the host cluster (the provider's responsibility) | the in-cluster / `--host-kubeconfig` host client |

`consumer`/`provider`/`host` are the only valid values; the CRD **enum-validates**
`target` at apply time, and the engine re-checks it. `target` routes both
`template` children (where they are written) and `externalRef` reads (which plane
they are read from).

**Why three targets.** A single instance often needs to write some state into the
tenant's own workspace (things the tenant should see and the claim authorizes),
some into the provider's back-office workspace (fulfilment requests the tenant
should never touch), and — for the api-syncagent use case — some into the physical
**host** cluster the controller runs in (real infrastructure like VMs). See
[decisions/0011-external-refs-and-host-target.md](decisions/0011-external-refs-and-host-target.md).

Provider- and host-target children are in the **same ownership domain** as the
controller (its own / the host client), so they involve **no permissionClaim**.
Consumer-target children cross a workspace boundary and therefore require the claim
handshake below. A `host`-routed resource fails loudly if the controller has no
host client configured (fail-closed) — see
[permissions.md](permissions.md#host-target-and-the-host-client).

---

## `externalRef`: reading objects krop does not own

A resource can carry an `externalRef` **instead of** a `template` (the two are
mutually exclusive, enforced by the CRD). An `externalRef` is an existing object
that krop **reads but never creates, mutates, labels, owns, or garbage-collects**.
Its observed `status`/`data` funnels into other resources via `${id.status.x}`
CEL, exactly like a `template` node's observed object — so an external input on one
plane can drive a written child on another.

Two forms:

```yaml
# single — one object by name (NotFound ⇒ the instance pends until it appears)
- id: vpc
  target: consumer
  externalRef:
    apiVersion: net.example/v1
    kind: VPC
    metadata:
      name: ${schema.spec.vpcName}      # CEL allowed in name/namespace/selector
      namespace: default

# collection — every object matching a label selector
- id: peers
  target: consumer
  externalRef:
    apiVersion: net.example/v1
    kind: VPC
    metadata:
      namespace: default
      selector:
        matchLabels: { tier: shared }
```

`externalRef` reads honor `target` like any resource: a `consumer` external ref is
read through the virtual workspace under a **read-only** permissionClaim
(`get,list,watch`); a `provider`/`host` external ref is read with the controller's
own / the host client (no claim). External refs are **never** pruned or swept —
krop does not own them.

> **Foreign-type invariant.** Reading a *foreign* (non-core) external CRD type
> still requires that type's owning `APIExport` to be **bound in the provider
> workspace** so its `identityHash` resolves — the same invariant that governs
> foreign consumer-target writes. Core types (group `""`) need no binding. See
> [permissions.md](permissions.md#external-refs-and-the-foreign-export-invariant).

### Cross-plane example: VPC (read) → VM (host write)

The motivating pattern: provision a VM into the **host** cluster that must live
inside an existing **VPC** owned by the tenant. Declare the VPC as a consumer
`externalRef`, read `${vpc.status.vpcId}`, and funnel it into a `host`-target VM
`template`:

```yaml
resources:
  # read-only: an existing VPC in the consumer workspace (never created/GC'd)
  - id: vpc
    target: consumer
    externalRef:
      apiVersion: net.example/v1
      kind: VPC
      metadata:
        name: ${schema.spec.vpcName}
        namespace: default
  # written into the physical host cluster, wired to the VPC's observed id
  - id: vm
    target: host
    template:
      apiVersion: compute.example/v1
      kind: VirtualMachine
      metadata:
        name: ${schema.spec.name}
        namespace: default
      spec:
        vpcId: ${vpc.status.vpcId}         # cross-plane CEL: consumer read → host write
```

The `${vpc.status.vpcId}` reference makes `vm` depend on `vpc`; krop reads the VPC
first, and if it (or its `status.vpcId`) is not present yet the VM **pends** —
identical convergence to any cross-target CEL dependency.

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
**consumer-target** nodes and emits one `permissionClaim` per GroupResource that is
*not* the instance's own type, with verbs split by what the node does:

- **writable children** (`template`, consumer) → full CRUD
  (`get,list,watch,create,update,patch,delete`);
- **external refs** (`externalRef`, consumer) → **read-only**
  (`get,list,watch`) — krop only ever reads them.

Claims are sorted for stable publications (`internal/registrar/claims.go`) and
published on the APIExport. Provider/host-target nodes get **no** claim (own /
host client).

Two rules follow from this:

1. **The consumer must ACCEPT the claim.** A cross-workspace (consumer-target)
   write is authorized only when the consumer's APIBinding sets the matching claim
   to `state: Accepted`. A `Rejected` (or absent) claim → the child is silently
   never written. This is by design; see the negative fixture
   `test/fixtures/apibinding-kubernetescluster-noclaim.yaml`.

2. **Foreign (non-core) consumer-target types must be bound in the provider
   workspace** — whether they are writable children **or** external refs. A claim
   for a core type (group `""`, e.g. `configmaps`)
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
children in the provider workspace, host children in the host cluster).
`externalRef` nodes are **never** pruned — krop does not own them. See
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
      target: provider
      template:
        apiVersion: fulfil.krop.opendefense.cloud/v1alpha1
        kind: AgentRequest
        metadata:
          name: ${schema.spec.region}-agent
          namespace: default
        spec:
          region: ${schema.spec.region}
    # (2) CONSUMER-target child (default): written into the consumer workspace
    #     through the APIExport vw, authorized by the accepted `configmaps` claim.
    #     Reads the PROVIDER child's status cross-target → PENDS until it is set.
    - id: config
      target: consumer
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.region}-cluster-config
          namespace: default
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
- [ ] Every resource has an `id`, exactly one of `template`/`externalRef`, and (if
      not consumer) an explicit `target: provider` or `target: host`.
- [ ] Every foreign consumer-target type's export (writable **or** external-ref)
      is **bound in the provider workspace** (else publish fails
      `ClaimIdentityUnresolved`).
- [ ] If any resource routes to `host`, the controller has a host client
      (in-cluster config or `--host-kubeconfig`) and the provider has granted it
      host-cluster RBAC for those GVKs.
- [ ] Every provider-target GVK has a least-privilege rule in
      `provider-rbac.yaml` (never `*`/`*`).
- [ ] Every provider-target child CRD is **served in the provider workspace**
      before the blueprint is created (kro type-checks CEL against it).
- [ ] Consumers know to bind the export **accepting** the derived claims.
