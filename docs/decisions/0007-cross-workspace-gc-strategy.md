# 7. Cross-workspace GC via labels + finalizer + ownerRef backstop + liveness sweep

## Status

Accepted.

## Context

One instance materializes children in two workspaces. Kubernetes owner references
are **workspace-local**, so a consumer-workspace instance cannot own its
provider-workspace children — the standard GC handle does not reach across
workspaces. Worse, if a consumer **unbinds** the APIExport mid-life, the virtual
workspace disengages that logical cluster: the reconciler stops running for the
instance and its finalizer can *never* fire, orphaning provider children.

## Decision

Layer four mechanisms (`internal/engine/labels.go`,
`internal/controller/reconciler.go`, `internal/controller/sweeper.go`):

1. **GC labels** on every child (`instance-uid`, `consumer-cluster`, `blueprint`)
   — the cross-workspace handle: children are enumerable and deletable by label in
   either workspace.
2. **Instance finalizer** (`krop.opendefense.cloud/gc`), added **before any child
   is applied**. On delete it lists children by `instance-uid` in *both*
   workspaces and deletes them, then drops the finalizer.
3. **OwnerRef backstop** on consumer-target children only (same workspace as the
   instance): a safety net for kcp's per-workspace collector if the finalizer path
   is ever bypassed (e.g. force-delete). Provider children can't use it.
4. **Liveness record + Sweeper** for the mid-life-unbind orphan: on every apply
   pass the reconciler upserts a provider-workspace ConfigMap
   (`krop.opendefense.cloud/liveness=true`) carrying the `instance-uid` label, a
   `lastReconciled` timestamp, and the provider-child GVKs. A live instance keeps
   it fresh; an unbound instance's record goes stale. The Sweeper reclaims the
   recorded provider children (by label) and the record once it exceeds
   `StaleAfter`, after a startup grace period.

Prune (drift/`forEach`-shrink/`includeWhen`-exclude) uses the same label
enumeration minus the just-applied set, and runs only after a complete pass.

## Consequences

- Provider children are reliably reclaimed on both the normal-delete path and the
  invisible mid-life-unbind path.
- The liveness record must be refreshed even on incomplete (pending-dependency)
  passes, because a provider child is created before a cross-target consumer
  dependency resolves and needs a record during the pending window.
- The sweep is timing-based (`StaleAfter` + startup grace), so orphan reclaim is
  eventually-consistent, not immediate — a deliberate trade to never sweep a live
  instance (see ADR 0010 and the architecture doc's limitations).
- Prune must be gated on a complete pass; pruning on a partial (pending) pass would
  delete still-desired children.
</content>
