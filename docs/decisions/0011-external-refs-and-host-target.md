# 11. External refs + a `host` write target, routed by a per-resource `target` field

## Status

Accepted. Supersedes [0002](0002-per-resource-target-annotation.md) (the routing
annotation).

## Context

Two capabilities turn krop from a two-plane composer into a flexible
api-syncagent-style bridge:

1. **Read existing objects it did not create**, on any plane, and funnel their
   status into other resources via CEL (kro's `externalRef`).
2. **Write children into a third plane** — the physical **host** cluster the
   controller pod runs in — reusing the provider-target machinery.

The motivating flow: provision a VM into the host cluster that must live inside an
existing VPC owned by the tenant — read the VPC as a consumer `externalRef`, feed
`${vpc.status.vpcId}` into a `host`-target VM `template`.

Both need a routing surface. The `krop.opendefense.cloud/target` **annotation**
from ADR 0002 cannot express this: kro's `ExternalRefMetadata` is only
name/namespace/selector and kro's `Resource` has no metadata block at all, so an
`externalRef` has nowhere to hang a routing annotation. Some routing surface that
works for **both** `template` and `externalRef` resources is required, and it must
now carry a third value (`host`).

Alternatives considered for the routing surface:

- **(a) Keep the `krop.opendefense.cloud/target` template annotation** (ADR 0002).
  Cannot route `externalRef` resources (no metadata block), so routing would be
  split across two mechanisms — annotation for templates, something else for
  externalRefs. Rejected: non-uniform, and impossible for external refs.
- **(b) A namespace prefix** like `host/my-ns` on `metadata.namespace`. Overloads a
  semantically meaningful field, is not enum-validatable, collides with real
  namespace names, and is awkward for cluster-scoped objects. Rejected.
- **(c) A top-level routing map** (`spec.routing: {resourceId: target}`). Repeats
  every resource id in a second place that must be kept in sync with
  `spec.resources[].id`; drift is silent and easy. Rejected.

## Decision

Add a per-resource **`target` field** (`consumer` | `provider` | `host`, default
`consumer`) to a thin krop wrapper over kro's spec. Our `ResourceGraphDefinitionSpec`
is kro's `Schema` + `Resources`, where each `Resource` inlines kro's resource
verbatim (`id`/`template`/`externalRef`/`readyWhen`/`includeWhen`/`forEach`) and
adds the single `target` field
(`api/v1alpha1/resourcegraphdefinition_types.go`, `routing.go`). A build-time
`ToKro()` conversion strips `target` into a routing map (`resourceID → target`) and
returns pristine kro types for the graph builder.

The field is chosen over the alternatives because it is:

- **Explicit** — routing is a first-class, named field, not an overloaded string or
  a stripped annotation.
- **Uniform** — the same field routes `template` writes and `externalRef` reads;
  there is exactly one routing surface.
- **Enum-validated at the CRD** — `+kubebuilder:validation:Enum=consumer;provider;host`
  rejects bad targets at apply time (the engine re-checks via `ParseTarget`).
- **Non-repetitive** — routing lives next to the resource it routes, so there is no
  second list of ids to keep in sync (unlike a top-level map).

The accepted **cost** is that our spec is no longer byte-identical to kro's: a thin
`ToKro()` conversion sits between the served CRD and kro's builder, and `SpecHash`
now hashes the krop wrapper (so a `target` change bumps the hash and republishes).
This is a small, mechanical layer we judged worth the uniformity and validation.

### External refs (read plane)

An `externalRef` resource is read — never created, mutated, labeled, owned,
renamed, GC'd, or swept. The engine gains a read-only branch: external nodes are
Get/List'd through a `Reader` (`ClientReader` per target), `SetObserved`, and
readiness-checked; NotFound ⇒ the instance pends (mirroring `ErrDataPending`),
never errors. Consumer external refs get a **read-only** permissionClaim
(`get,list,watch`); provider/host external refs are read with the controller's own
/ the host client (no claim).

### Host client sourcing

The host client is sourced in `cmd/controller/main.go`: `--host-kubeconfig <path>`
if set, else `rest.InClusterConfig()`. If neither yields a config the host client
is **nil**, no host applier/reader is registered, and a blueprint routing to `host`
**fails loudly** with the engine's missing-target error (fail-closed — never a
silent drop). Host children reuse the entire provider-target machinery
(collision-free `ProviderChildName`, GC labels, prune, and — via the liveness
record's `hostChildGVKs` — the orphan Sweeper). Host-cluster RBAC is the
**provider's** responsibility and out of krop's least-privilege scope: the chart
ships **no** host ClusterRole.

### Inherited external-ref identityHash invariant

Reading a *foreign* (non-core) external CRD type through kcp still requires that
type's owning `APIExport` to be **bound in the provider workspace** so its
`identityHash` resolves — the exact same constraint that governs foreign
consumer-target writes. `validateClaims` still rejects an unresolved foreign claim
(publish fails `ClaimIdentityUnresolved`). Core types (group `""`) carry an empty
identityHash and are allowed. This invariant is inherited, not new, and is not
relaxed here.

## Consequences

- **One uniform, validated routing surface.** `target` routes every resource —
  template or externalRef — with CRD enum validation; the ADR 0002 annotation and
  its `TargetOf`/`StripRouting`/`TargetAnnotation` helpers are removed.
- **Spec is a thin wrapper, not byte-identical to kro.** `ToKro()` converts; kro's
  builder/runtime still see pristine kro types. `SpecHash` hashes the wrapper.
- **Existing blueprints/fixtures/docs migrated** from the annotation to the field
  (pre-release, low cost). Omitted `target` still defaults to `consumer`, so the
  default behavior is unchanged.
- **Three write planes + a read plane.** `host` children are reclaimed the same way
  provider children are (labels + finalizer + liveness sweep). External refs are
  never owned or reclaimed.
- **Host authorization is explicitly out of scope.** krop ships no host ClusterRole;
  the provider grants the host client its RBAC out-of-band. Nil host config ⇒ host
  target disabled, fail-closed.
