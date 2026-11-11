# 13. Leveled, structured logging with a fixed field contract

## Status

Proposed.

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
  published/republished, instance manager started/stopped/crashed), and **every deletion
  krop decides on its own** (prune, instance-deletion GC, orphan sweep). No per-pass lines.
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
  may carry secrets in status).

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
