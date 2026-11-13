# 14. Consumer-safe instance status: classified errors, percentage progress, recoverable state

## Status

Accepted.

## Context

A consumer orders an instance (a Bucket, a VPC, …) and krop builds it from a graph of
nodes, some in the consumer's workspace, some in the provider workspace or on the host
cluster. Before this decision (issue #49):

- A failed pass returned before writing status, so the consumer kept seeing the
  previous pass's status while krop failed in the background.
- Every not-ready pass wrote the same `Ready=False` reason `Progressing` with a generic
  message, so the consumer could not tell a bucket still being created from one that
  will never be: "it is stuck and nothing happens".
- The raw errors krop has are not safe to show. An admission denial on the host can
  name another tenant's object (Kyverno: "Another StorageBucket already uses the GCS
  name …"), a Config Connector message names GCP projects, and kro's evaluation errors
  quote the blueprint's expressions.

## Decision

krop writes the instance status on **every** pass, failed ones included, from the
engine `Result` (which now also records the failed step, the node counts and the
not-ready object's `Ready` message). It uses only fields kro's generated CRD already
has, so the published schema does not change.

- **`Ready` condition, reason by what the consumer does about it.** `InProgress` (wait:
  a node or its data is not ready), `ReferenceNotFound`, `ReferenceFailed`, `ApplyFailed`,
  `EvaluationFailed`, `RoutingFailed`.
- **`Ready` message: verbatim only for the consumer's own text.** Errors from the
  consumer's workspace and the `Ready` message of a consumer child pass through. For
  anything from the provider workspace or the host, and for evaluation errors, the
  message is fixed text naming the node, the class of failure (the API status reason,
  e.g. `BadRequest`, or the spec fields the expression reads) and `ref <instance UID>`.
- **The ref finds the raw error.** The reconcile error (logged at the default level),
  every V(1)/V(2) line of the instance (ADR 0013), every child's `instance-uid` label
  and the liveness record carry the instance UID.
- **`Progressing` message: a percentage only.** The share of ready nodes, rounded down
  so a not-ready instance never shows `100%`. No node names or counts: they would show
  how the provider built the offering.
- **`status.state` in kro's vocabulary.** `ACTIVE` when ready; `ERROR` only for a failure
  a retry will not clear: an expression that cannot evaluate, a node with no target, or
  a request the API server refuses as such (bad request, invalid, forbidden,
  unauthorized, method not supported, too large); `IN_PROGRESS` otherwise, including
  conflicts, timeouts and an unavailable API server. A blueprint that maps
  `status.state` itself keeps its value.

Considered and not taken: mirroring raw errors (leaks host and tenant details); a
classified message without a ref (the provider cannot find the cause); Kubernetes
Events (every consumer APIBinding would need an `events` claim); a dedicated progress
field with a printer column (needs schema injection into the published, immutable
APIResourceSchema). Blueprint-authored error messages are a possible follow-up.

## Consequences

- A consumer, or a portal list view, can show ready / in progress / failed and how far
  an instance got without reading krop's reasons.
- The `Ready` reasons, the `state` values and the `Progressing` format are an API:
  `kubectl wait`, Flux health checks and portal rules match them. The old `Ready`
  reason `Progressing` is gone.
- Node names (the blueprint's resource IDs) still appear in `Ready` messages.
- The percentage is coarse. Nodes are not equal work, the total shrinks when
  `includeWhen` excludes a node, a pass that ends early leaves later nodes uncounted,
  and with few nodes the steps reveal the count.
- A failure that keeps recurring but could clear (e.g. a host API server down for an
  hour) stays `IN_PROGRESS`; there is no time-based escalation to `ERROR`.
- A consumer child whose `Ready` message changes often causes as many status writes.
