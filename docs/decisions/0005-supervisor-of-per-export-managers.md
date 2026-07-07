# 5. One multicluster manager per APIExport, owned by a Supervisor

## Status

Accepted.

## Context

An apiexport multicluster provider (`apiexport.New`) binds **exactly one**
`APIExportEndpointSlice` — i.e. one APIExport's virtual workspace. But krop's
control plane is dynamic: a provider can author many blueprints, each publishing
its own APIExport, and blueprints come and go at runtime. There is no single
static endpoint to serve.

## Decision

Run **one instance-serving multicluster manager per published APIExport**, and put
a `Supervisor` (`internal/supervisor/`) in charge of their lifecycle.

- The Registrar's `OnPublished` callback records the compiled graph + instance GVK
  and calls `Supervisor.Ensure(exportName)`; `OnDeleted` calls `Stop`.
- `Ensure` starts the manager in a goroutine with a cancellable context derived
  from the root context; it is idempotent while one runs.
- `Stop` cancels and forgets the manager.
- A manager that exits (crash or clean stop) clears its own registry entry via
  `forget` (matched by pointer identity so a fast stop/restart can't clobber a
  newer generation), so the next `Ensure` self-heals it. The periodic blueprint
  resync (5m) is the steady-state re-trigger.

The single provider-workspace manager hosts the Registrar and Sweeper; the
per-export managers host only the instance Reconciler and disable their metrics
server (one per blueprint would otherwise collide on the metrics port).

## Consequences

- krop serves an arbitrary, changing set of blueprints from one process, each
  behind its own virtual-workspace fan-in over all bound consumers.
- Manager lifecycle is explicit and event-driven (publish/delete/spec-change),
  with a resync-based self-heal for crashed managers.
- A spec change is handled by `Stop`+`Ensure` (see ADR 0009).
- The blast radius of one failing blueprint's manager is isolated from the others
  and from the provider manager; a terminal per-export error is logged at the
  supervisor boundary rather than taking down the process.
</content>
