# 13. Leveled, structured logging with a fixed field contract

## Status

Accepted.

## Context

One krop process serves many blueprints (bucket, vpc, subnet, dnszone, …) and many
instances of each, across consumer workspaces. Providers read its logs with
`kubectl logs` and in Loki/Grafana. Before this decision:

- The only reconcile line (`reconciled; requeueing`) was emitted at Info every ~30 s per
  instance, carried `cluster` and `name` but no namespace or blueprint, and said nothing
  about what the pass did.
- The engine ended a pass on the first pending node and dropped kro's reason, so a
  stuck instance (issue #72) could not be diagnosed from logs.
- Registrar, supervisor and sweeper each logged ad hoc, with differing keys for the same
  thing.

## Decision

All components log through controller-runtime's zap/logr with three tiers and one set
of field names. Full detail: [leveled-logging design](../superpowers/specs/2026-11-12-leveled-logging-design.md).

- **Default (Info + Error): quiet.** Errors, rare lifecycle events (blueprint
  published/republished/deleted, instance manager started/stopped/crashed), and **every
  deletion krop decides on its own** (prune, instance-deletion GC, orphan sweep, APIExport
  withdrawal). Child deletions are one line per pass and target listing the children, so
  controller-runtime's log sampling (100 lines per message per second, then 1 in 100)
  cannot drop them in bursts. Error lines carry no stack trace by default
  (`--zap-stacktrace-level=panic`). No per-pass lines.
  Sampling applies to every level below V(2), so at V(1) the per-pass lines are
  thinned when more than 100 passes end within a second; V(2) is unsampled.
- **V(1) (`--zap-log-level=debug`): why each pass ended.** One line per instance pass with
  the outcome (`Complete`, `NotReady`, `Pending`, `ExternalNotFound`, `Failed`), the node
  that ended it, and kro's reason.
- **V(2) (`--zap-log-level=2`): what each pass did.** One line per node: skipped, applied,
  read or not ready, with target and final child name.
- **Field contract.** Instance-scoped lines carry `blueprint` (RGD name), `export`,
  `kind`, `cluster`, `namespace`, `name`, `instanceUID`; per-node lines add `node`,
  `target`, `gvk`, `child`, `reason`. The instance logger is installed via the controller
  builder's `WithLogConstructor` so controller-runtime's own error lines carry it too.
- **No object content.** Lines never include observed or desired object values (children
  may carry secrets in status). The one exception is the reason code of a not-ready
  object's `Ready` condition (see the amendment below).

The engine's `Result` records the pass outcome, ending node and reason; the reconciler
logs from it. Logging does not change reconcile behaviour.

## Consequences

- A provider can filter one instance's history, or all pending passes of one blueprint,
  by stable JSON fields; `docs/operations.md` documents the fields and example queries.
  Renaming a field is a breaking change for those queries.
- The default level drops from one line per instance per 30 s to errors, lifecycle and
  deletions.
- `Result` carries why a pass ended, which #49 can reuse to surface the reason in
  instance status.
- The RGD name must be threaded from the registrar to the instance reconciler (it only
  knew the export name). The `blueprint` GC label on children is unchanged.
- Sweeper lines join instance lines by `instanceUID`; the liveness record is not extended.

## Amendment (2026-11-12): the Ready condition's reason code

### Context

The catalogue of log output per case showed that for #72 the debug line names the
blocking node and kro's readyWhen expression, but not why the child is not ready. Config
Connector says why in the child's `Ready` condition: a `reason` code (`UpdateFailed`,
`DependencyNotReady`, `ManagementConflict`) and a free-text `message` (e.g. the GCP API
error). The provider had to `kubectl get` the child to tell "still creating" from
"failing".

### Decision

- A not-ready node's V(2) line, and the V(1) `pass ended` line of a `NotReady` pass, add
  `readyReason`: the `reason` of the object's `Ready` condition when its status is not
  `True`. A `Pending` pass that waits for a not-ready dependency adds `dependency` (the
  dependency kro names in its reason) and that dependency's `readyReason`.
- Only the reason code is logged, and only when it has the format Kubernetes requires of
  a condition reason (`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`) and at most 64
  characters. Anything else is treated as free text and dropped.
- The condition's `message` is never logged. Another controller writes it, often from a
  cloud API or client library error, and krop cannot know what it contains.

### Consequences

- A provider sees from the debug line whether a dependency is failing (`UpdateFailed`) or
  still being created, and reads the message with `kubectl get` when needed.
- The residual risk is a controller that breaks the convention and writes a secret that
  looks like a code into `reason`. krop accepts that risk. The same applies to kro's
  readiness error text, now logged as `reason`: it names expressions, but a CEL
  evaluation error can quote the value it failed on (e.g. an invalid timestamp).
- Only the `Ready` condition is read. Controllers that report errors in another condition
  (e.g. Crossplane's `Synced`) give no `readyReason` beyond their `Ready` reason.
