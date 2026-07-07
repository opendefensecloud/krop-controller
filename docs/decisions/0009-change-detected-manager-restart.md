# 9. Change-detected manager restart for live spec edits (over multi-version serving)

## Status

Accepted.

## Context

A blueprint's spec can be edited while instances exist. The per-export instance
manager holds a *compiled graph* captured at publish time; a live edit must not
keep serving the stale graph. kcp APIResourceSchemas are immutable and a spec edit
mints a new one, so a fully general answer is multi-version serving (old instances
stay on their bound version; new instances use the new schema) — but that is
substantial machinery.

## Decision

Handle a spec edit by **restarting the per-export manager** with the fresh graph.
The Registrar detects change by comparing `SpecHash(spec)` to
`Status.ObservedSpecHash` and passes a `changed` flag to `OnPublished`. The main
wiring always updates the served-graph registry, and when `changed` is true it
`Stop`s the running manager before `Ensure` restarts it, so the restarted manager
re-reads the updated graph. An unchanged 5-minute resync leaves `changed` false so
the manager is not torn down every resync.

Restarting rebuilds a controller under the **same** name. controller-runtime keeps
a process-global registry of controller names (to catch duplicate metric labels)
with no deregistration on manager stop, so the rebuild sets
`SkipNameValidation: true` — the old manager is fully cancelled before the new one
starts, so the name reuse is intentional and safe.

## Consequences

- Live blueprint spec edits take effect without a controller redeploy.
- This is **not** multi-version serving: an incompatible schema change across an
  in-place version is not served alongside the old one. Proper multi-version
  serving is a documented future enhancement.
- The `SkipNameValidation` escape hatch is required precisely because the restart
  reuses the controller name; it is safe only because Stop fully precedes Ensure.
- Restart briefly interrupts instance serving for that one export while the new
  manager rediscovers the endpoint slice and re-reconciles.
</content>
