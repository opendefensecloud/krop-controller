# 8. Least-privilege: no host-cluster ClusterRole; authorize via kcp RBAC + claims

## Status

Accepted.

## Context

krop is deployed as a pod in a hosting/management cluster, but it does almost none
of its work there — it drives kcp through a multicluster-runtime manager. A naive
chart would grant the pod a broad hosting-cluster ClusterRole (an earlier version
shipped an `apiGroups:["*"] resources:["*"]` full-CRUD rule for resources that
actually live in kcp). That is unnecessary authority in the wrong place.

## Decision

Grant the pod **no hosting-cluster ClusterRole/Role**. The Helm chart renders only
a ServiceAccount + Deployment (+ the blueprint CRD); the SA exists solely to mount
a workspace-scoped kcp kubeconfig Secret. Authorization happens entirely in kcp:

- **Root workspace:** read-only (enter child workspaces, resolve path → cluster).
- **Provider workspace:** own blueprints, scoped `apis.kcp.io`,
  `apiexports/content` (to serve instances through the virtual workspace), the
  deployment-specific provider-target child GVKs, and liveness ConfigMaps.
- **Consumer workspaces:** *zero* standing RBAC — consumer-target writes flow
  through the APIExport virtual-workspace identity, authorized only by the
  consumer's **accepted permissionClaims**.

The identity is minted with a kcp-operator `Kubeconfig` CR signed by the root
shard CA. This mirrors `opendefensecloud/dependency-controller`'s model. Full
detail lives in [../permissions.md](../permissions.md).

## Consequences

- The controller's authority in each workspace is exactly what it needs and no
  more, and consumer-side authority is consent-based and revocable.
- The provider-target child RBAC rule is deployment-specific (the operator must
  supply the exact GVKs their blueprints emit — never a wildcard).
- The `apiexports/content` grant is easy to miss because envtest connects as admin
  and never exercises it; only the deployed-pod e2e under the real SA identity
  surfaces its absence.
- Operators have a checklist (in permissions.md) to verify the posture, including
  confirming `helm template` renders no host-cluster RBAC.
</content>
