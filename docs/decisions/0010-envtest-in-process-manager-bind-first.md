# 10. envtest e2e drives an in-process manager against the vw with bind-first ordering

## Status

Accepted (context / historical).

## Context

krop's behavior depends on real kcp virtual-workspace mechanics — APIExport
publication, endpoint-slice discovery, the multicluster fan-in over bound
consumers, permissionClaim acceptance, and cross-workspace apply/GC. Unit tests
with fakes cannot exercise these. A full kind + kcp-operator + Helm deployment is
slow and heavy for the inner loop. A middle tier is needed that runs real kcp but
stays in-process.

## Decision

Run an **envtest** tier (`make test`) against a real **in-process kcp** binary
(v0.30.0, matching what multicluster-provider v0.8.0 was tested against — distinct
from the production kcp version). The harness publishes a blueprint's APIExport,
binds a consumer workspace, then builds an `apiexport` provider + multicluster
manager **in-process** and drives the same `Reconciler` the deployed controller
uses. It exercises dual-target apply, prune, spec-change restart, and the orphan
sweep as dynamic specs.

The harness observes the **bind-first ordering rule**: an
`APIExportEndpointSlice`'s virtual-workspace URL is empty until at least one
consumer `APIBinding` consumes it, so tests must bind a consumer before the
instance manager can engage the virtual workspace.

## Consequences

- The kcp integration path is covered without a container/kind spin-up, so it runs
  in the normal `go test`/`make test` loop.
- The envtest tier connects as admin, so it does **not** exercise the
  least-privilege SA identity or the `apiexports/content` grant — those are only
  caught by the full-stack deployed-pod e2e (`make test-e2e`).
- Tests must encode the bind-first ordering (poll the endpoint slice URL until a
  binding populates it) rather than assuming the endpoint exists at publish time.
- The envtest kcp binary version is pinned separately from the production target;
  both must be tracked when upgrading kcp.
</content>
