# 6. Cache compiled graphs instead of adopting kro's GraphRevision

## Status

Accepted.

## Context

Compiling a blueprint into a kro graph is expensive: `graph.NewBuilder` needs a
`rest.Config` that serves discovery/OpenAPI for every child GVK (kro type-checks
CEL against child schemas at build time), and the build validates naming, builds
the instance CRD, extracts CEL, and builds + cycle-checks the DAG. This cost is
per-blueprint, not per-instance, so it should be amortized. kro offers a
`GraphRevision` mechanism for compiled-graph versioning, but it is tied to kro's
dropped controller layer.

## Decision

Use a small in-process cache (`internal/registrar/cache.go`, `GraphCache`) keyed
by **(workspace, blueprint name, specHash)**. The Registrar looks up the graph by
`SpecHash(spec)` before building; a hit skips the build entirely. `Put` replaces
any prior hash for a blueprint (a spec edit supersedes the old graph), and
`Delete` drops the entry on blueprint withdrawal.

The specHash also serves change-detection: comparing the spec's hash to the
blueprint's `Status.ObservedSpecHash` tells the Registrar whether the served graph
actually changed (new/edited vs. an unchanged resync).

## Consequences

- Graph build is paid once per blueprint spec version and reused across every
  instance reconcile and every resync.
- The workspace dimension in the key avoids blueprint-name collisions across
  provider workspaces.
- No dependency on kro's `GraphRevision`/`internal.kro.run` types or its dropped
  controller layer.
- The cache is process-local (rebuilt on restart); it is a performance cache, not
  a source of truth — the blueprint object is.
</content>
