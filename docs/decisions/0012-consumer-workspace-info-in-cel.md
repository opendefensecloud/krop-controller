# 12. Consumer workspace info in CEL — the `consumer-cluster` instance annotation

## Status

Accepted.

## Context

Host- and provider-target children keep their template `metadata.name` (only
provider-target children are additionally renamed collision-free by krop — see
[blueprints.md](../blueprints.md#provider-child-naming-collision-free)). When many
tenants share **one** host cluster or provider namespace, a literal name like `db`
collides across tenants. A blueprint author needs a way to derive a collision-free
name **from the consumer's identity** — e.g. prefix the host Deployment with the
tenant's workspace so `<tenantA>-db` and `<tenantB>-db` never clash.

That requires surfacing the consumer's workspace identity to the blueprint's CEL.
Two questions follow: **which** identity value, and **how** to expose it to CEL.

**Which value — canonical cluster name vs. workspace path.** kcp gives an instance
two handles to its home workspace: the human **workspace path** (`root:acme:team`)
and the **canonical logical-cluster name** (e.g. `kvdk8299mah3yj1p`). The path is
**mutable** — a workspace can be renamed or moved, which would silently change any
name derived from it — and resolving it requires an extra RBAC-gated lookup. The
canonical cluster name is **globally unique and immutable**, already in hand on the
reconcile request (`req.ClusterName`), and is exactly the stable handle needed for
collision-free naming in a shared plane.

**How to expose it — no room for a new top-level CEL variable.** kro exposes the
instance to CEL only as the `schema` variable (its `spec` + `metadata`, **no**
`status`). There is no supported way to add a bare new top-level variable (a
`${workspace.name}`): kro's build-time validation type-checks every expression
against the graph it knows about, and an unknown top-level identifier fails that
validation. The one CEL-reachable place kro does **not** constrain is
`metadata.annotations`, an open `map[string]string`.

## Decision

Before building the kro runtime, the reconciler **stamps the consumer's canonical
logical-cluster name onto the instance metadata as an annotation**:

```
krop.opendefense.cloud/consumer-cluster: <logical-cluster-name>
```

Blueprints reference it via CEL like any other metadata field:

```yaml
metadata:
  name: ${schema.metadata.annotations["krop.opendefense.cloud/consumer-cluster"]}-${schema.spec.name}
```

The stamp is applied to a **runtime-only deep copy** of the instance
(`internal/engine/workspace.go` `AnnotationConsumerCluster` / `StampConsumerCluster`,
called from `internal/controller/reconciler.go` before `kroruntime.FromGraph`). It
is **never persisted**: only the pristine instance is `Status().Update`'d. The
annotation deliberately shares its key with the `consumer-cluster` **label** krop
already applies to every materialized child, so the instance annotation and the
child labels always carry the same value.

Rationale for the two choices:

- **Canonical cluster name, not the path** — unique + immutable (safe to hash names
  against), already available on the request, and needs no extra path→cluster
  lookup or RBAC. The mutable path would make derived names silently unstable.
- **An annotation, not a new CEL variable** — `metadata.annotations` is the only
  CEL-reachable open map that passes kro's build-time validation; a top-level
  `${workspace.name}` would not type-check. Reusing the metadata surface keeps the
  injection zero-schema and requires no fork of kro.

## Consequences

- **Blueprints can derive collision-free host/provider child names** from the
  consumer identity with a plain CEL reference — no krop-specific renaming needed
  for host/consumer children. The shipped
  [`blueprint-hosteddatabase.yaml`](../../config/kcp/examples/blueprint-hosteddatabase.yaml)
  uses exactly this to prefix its host Deployment.
- **Runtime-only, never stored.** The annotation exists only on the copy handed to
  the kro runtime; the persisted instance is untouched, so there is no API surface
  churn, no reconcile-loop write amplification, and nothing for a consumer to edit.
- **Instance annotation and child label always agree** (shared key), so operators
  can correlate an instance to its materialized children by the same value.
- **A reserved annotation key.** `krop.opendefense.cloud/consumer-cluster` is owned
  by krop on the runtime copy; a value a consumer sets for that key on the stored
  instance is overwritten in-memory for the CEL pass (and is irrelevant, since it is
  never read back from storage).
