# Architecture decision records

Short, numbered records of the significant design decisions behind
krop-controller. Each states Context / Decision / Consequences / Status and is
grounded in the code on `main`.

| # | Decision |
| --- | --- |
| [0001](0001-embed-kro-as-a-library.md) | Embed kro as a library, not as a controller |
| [0002](0002-per-resource-target-annotation.md) | Route children by a per-resource annotation on a kro-identical RGD |
| [0003](0003-crdtoapiresourceschema-publication.md) | Publish the generated instance CRD as an APIResourceSchema via the kcp SDK |
| [0004](0004-auto-derived-permission-claims.md) | Auto-derive permissionClaims from consumer-target node GVRs |
| [0005](0005-supervisor-of-per-export-managers.md) | One multicluster manager per APIExport, owned by a Supervisor |
| [0006](0006-compiled-graph-cache.md) | Cache compiled graphs instead of adopting kro's GraphRevision |
| [0007](0007-cross-workspace-gc-strategy.md) | Cross-workspace GC via labels + finalizer + ownerRef backstop + liveness sweep |
| [0008](0008-least-privilege-service-account.md) | Least-privilege: no host-cluster ClusterRole; authorize via kcp RBAC + claims |
| [0009](0009-change-detected-manager-restart.md) | Change-detected manager restart for live spec edits (over multi-version serving) |
| [0010](0010-envtest-in-process-manager-bind-first.md) | envtest e2e drives an in-process manager against the vw with bind-first ordering |
</content>
