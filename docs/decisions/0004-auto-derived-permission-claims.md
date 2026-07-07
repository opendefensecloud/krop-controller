# 4. Auto-derive permissionClaims from consumer-target node GVRs

## Status

Accepted.

## Context

Consumer-target children of a *foreign* type (a type the blueprint's APIExport
does not itself export — e.g. another export's types, or core `ConfigMap`s) can
only be written into a consumer workspace if the blueprint's APIExport declares a
`permissionClaim` for that GroupResource (with the owning export's identityHash)
and the consumer's APIBinding **accepts** it. The instance kind itself is native
to the export and needs no claim.

The original design had the blueprint author hand-list these as a `consumerClaims`
field. But the set of foreign consumer-target GroupResources is already fully
determined by the graph — hand-listing it is redundant and can drift.

## Decision

Derive the claims from the compiled graph. `ForeignConsumerGRs`
(`internal/registrar/claims.go`) walks the graph nodes, reads each node's routing
target exactly as the engine does, and collects the GroupResources of
consumer-target nodes that are **not** the instance's own type.
`DeriveClaims` emits one `permissionClaim` per such GroupResource (verbs
`get,list,watch,create,update,patch,delete`, sorted for stable output), resolving
each identityHash from the APIBindings bound in the provider workspace
(`identityByGroupResource`, reading `status.identityHash`). The claims are
published on the APIExport by `UpsertAPIExport`.

A foreign (non-core) claim whose identityHash is unresolved fails the publish
(`Ready=False`, `ClaimIdentityUnresolved`) rather than emitting a claim that would
not authorize; the 5-minute resync retries once the provider binds the foreign
export. Core types (empty group) legitimately carry an empty identityHash.

## Consequences

- Claims cannot drift from the graph: adding/removing a consumer-target foreign
  child automatically adds/removes its claim on the next publish.
- Blueprint authors write no claims block.
- The provider must have bound the foreign types' owning APIExports before the
  blueprint can publish successfully — a fail-loud dependency rather than a
  silently broken claim consumers might accept.
- Consent and blast radius stay per-type per-workspace: a consumer authorizes
  exactly the derived GroupResources by accepting the binding, and revokes by
  deleting it.
</content>
