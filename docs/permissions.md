# krop-controller authorization & least-privilege permission model

krop-controller does almost none of its work against the cluster it is deployed
on. It runs a kcp multicluster-runtime manager that authorizes against **kcp**
through a mounted, workspace-scoped kubeconfig. This document describes the
least-privilege posture, how identity is minted, how to apply the RBAC fixtures,
and the permissionClaims spine that carries cross-workspace writes.

It mirrors the model used by `opendefensecloud/dependency-controller` (see its
`docs/architecture.md` "Workspace Topology" and `docs/getting-started.md` RBAC
steps).

## The controller's identity

The controller connects to a kcp **provider workspace** with a workspace-scoped
kubeconfig, mounted from a Secret by the Helm chart (`kcp.kubeconfigSecret.*`,
rendered into `KUBECONFIG=/etc/kcp/<key>` in
`charts/krop-controller/templates/deployment.yaml`). The certificate in that
kubeconfig authenticates as:

```
system:serviceaccount:<namespace>:<serviceaccount>
```

where `<namespace>`/`<serviceaccount>` are the pod's Kubernetes ServiceAccount
(the chart creates a bare SA, e.g. `krop-system:krop-controller`). Everywhere
below this is written `${SA_IDENTITY}`.

## Posture by plane

| Tier | Where | What the SA identity gets | Source |
| --- | --- | --- | --- |
| (a) Hosting cluster | kind / management cluster | **ServiceAccount only — NO ClusterRole/Role.** Pod uses the SA solely to mount the kubeconfig Secret. | `charts/krop-controller/` |
| (b) kcp root workspace | `root` | `workspaces/content` `access` + `workspaces` get/list/watch. Enter child workspaces, resolve path→cluster. No writes. | `config/kcp/rbac/root-rbac.yaml` |
| (c) kcp provider workspace | provider ws | Own RGD blueprints + scoped `apis.kcp.io` + **deployment-specific** provider-target child GVKs. | `config/kcp/rbac/provider-rbac.yaml` |
| (d) Consumer workspaces | tenant ws | **NO standing RBAC.** Writes flow through the APIExport virtual-workspace identity, authorized by the consumer's accepted `permissionClaims`; consumer `externalRef` reads use a **read-only** claim. | APIExport + APIBinding |
| (e) Host cluster | the physical cluster the pod runs in | **Provider-managed, out of krop's scope.** krop ships **no** host ClusterRole; `target: host` writes/reads use the in-cluster / `--host-kubeconfig` host client, whose RBAC the provider grants out-of-band. | provider (not this chart) |

### (a) Hosting cluster — ServiceAccount only

The chart deliberately renders **no** `ClusterRole`/`ClusterRoleBinding`/`Role`.
`helm template` yields only `ServiceAccount` + `Deployment` (plus the blueprint
CRD from `crds/`). The controller never acts on the hosting cluster's API for
business logic, so it needs no authority there. (An earlier version of the chart
shipped a management-cluster ClusterRole with a broad `apiGroups:["*"]
resources:["*"]` full-CRUD rule — for resources that live in kcp, not the host
cluster. That was removed in M6.)

### (b) kcp root workspace — workspaces read

The manager enters child workspaces and resolves workspace paths to logical
cluster names. That is the entire root-workspace grant — two read-only rules, no
writes. Apply `config/kcp/rbac/root-rbac.yaml`.

### (c) kcp provider workspace — the controller's real work

Everything the controller does in the provider workspace it does as
`${SA_IDENTITY}`:

- **Blueprints (`krop.opendefense.cloud/resourcegraphdefinitions`)** — the
  Registrar watches the cluster-scoped RGDs
  (`internal/registrar/registrar.go`). It manages a teardown finalizer via
  `controllerutil.AddFinalizer`/`RemoveFinalizer` + `Client.Update` on the RGD
  object itself, so **`update` on `resourcegraphdefinitions`** (not a
  `/finalizers` subresource) is what the finalizer needs. It records publish
  status via `Status().Update`, so **`update` on
  `resourcegraphdefinitions/status`**.
- **`apis.kcp.io`** — the Registrar compiles each blueprint into an
  `APIResourceSchema` (create/read) and server-side-applies the `APIExport`
  (`Client.Apply` ⇒ `create` on first apply, `patch`/`update` thereafter) with
  its derived `permissionClaims` (`internal/registrar/publish.go`). It discovers
  the `APIExportEndpointSlice` for the virtual-workspace URL
  (`internal/kcp/endpointslice.go`) and lists `APIBindings` to resolve claim
  identity hashes — both read-only.
  - Scoping note: the export name is derived from the blueprint at runtime, so
    the `apiexports` rule is left unscoped for a fresh install. Once export
    names are known, tighten with `resourceNames`.
- **`apis.kcp.io/apiexports/content`** — the Supervisor serves instances
  **through** the published APIExport's virtual workspace (the apiexport
  multicluster provider connects to the endpoint-slice URL and reconciles the
  instance kind across every bound consumer). kcp's apiexport virtual-workspace
  authorizer gates that access on the request verb applied to the
  `apiexports/content` subresource **in the export's own workspace** — so without
  this grant the provider's endpoint watcher fails discovery with `access denied`
  and the instance-serving path never engages any consumer cluster. This gap is
  invisible to envtest (which connects as admin) and is surfaced only by the
  deployed-pod e2e running under the least-privilege SA identity.
- **Provider-target children (DEPLOYMENT-SPECIFIC)** — resources with
  `target: provider` are written into the
  provider workspace by the engine's `ProviderClient`
  (`internal/controller/reconciler.go`), with the controller's own identity —
  the same ownership domain, so **no permissionClaim is involved** (design §9).
  These GVKs are defined by the operator's blueprints and are not knowable at
  chart-install time. `provider-rbac.yaml` ships an **example** rule matching
  krop's example blueprint
  (`config/kcp/examples/blueprint-kubernetescluster.yaml`), which emits a
  `fulfil.krop.opendefense.cloud/AgentRequest`. Operators **must** replace/extend
  this with the exact provider-target GVKs their blueprints emit. **Do not** use
  a `*`/`*` wildcard — that re-creates the broad grant this model exists to
  remove.
- **Liveness records + orphan sweep (design §11)** — provider-target children
  orphan if a consumer unbinds the APIExport mid-life: the instance reconciler
  stops running for that logical cluster, so its finalizer never fires. To recover,
  the reconciler upserts a per-instance **liveness record** (a `ConfigMap` in the
  `default` namespace, labeled `krop.opendefense.cloud/liveness=true`) on every
  complete pass, and the `Sweeper` (`internal/controller/sweeper.go`, run on the
  provider manager) deletes the recorded provider children + the record once the
  record goes stale. Both run with the controller's own provider-workspace
  identity, so **no extra RBAC** is required beyond the `configmaps`
  create/get/list/watch/update/patch/delete grant `provider-rbac.yaml` already
  ships (the same rule used for provider-target ConfigMaps).

Apply `config/kcp/rbac/provider-rbac.yaml`.

### (d) Consumer workspaces — zero standing RBAC

The controller has **no** RBAC in consumer (tenant) workspaces. The instance
kind and any consumer-target children (`target: consumer`, the default) are
written by the per-request consumer client, which is the
**APIExport virtual workspace** — the write runs as the APIExport identity and is
authorized only when the consumer's `APIBinding` has **accepted** the
corresponding `permissionClaim`. Without acceptance, the write is rejected. This
is why there is nothing to grant here.

### (e) Host cluster — provider-managed, out of krop's scope

The `host` target writes children into (and reads external refs from) the
**physical cluster the controller pod runs in**. This plane is deliberately
**outside krop's least-privilege model**: the chart ships **no** host-cluster
ClusterRole/Role of its own (tier (a) is unchanged — still ServiceAccount only),
and granting the host client whatever RBAC the `target: host` GVKs need is the
**provider's** responsibility, arranged out-of-band. This matches the product
direction that host-cluster authorization is the provider's concern, not krop's.
See [host target and the host client](#host-target-and-the-host-client).

## Host target and the host client

`target: host` resources are written/read through a dedicated **host client**,
sourced by `cmd/controller/main.go`:

1. `--host-kubeconfig <path>` — an explicit kubeconfig for the host cluster
   (dev / out-of-cluster runs, or a host cluster distinct from the pod's own); else
2. `rest.InClusterConfig()` — the pod's own ServiceAccount against the cluster it
   runs in (the default in-cluster deployment).

If **neither** yields a config, the host client is **nil**: no host applier/reader
is registered and any blueprint routing to `host` **fails loudly** with the
engine's "no applier/reader configured for target host" error (fail-closed — never
a silent drop). The chart exposes this as the `hostKubeconfig` value (see
[the chart](#reference-files)); its in-cluster default needs no host ClusterRole
**from krop** — host-cluster RBAC is provider-managed (tier (e)).

Host children are named collision-free (`ProviderChildName`) and carry the same GC
labels as provider children, so the finalizer path and the orphan
[Sweeper](operations.md#garbage-collection--the-orphan-sweep) reclaim them too
(the Sweeper deletes host children via the host client from the liveness record's
`hostChildGVKs`).

## External refs and the foreign-export invariant

`externalRef` resources are **read-only** on every plane — krop Gets/Lists them,
never applies, labels, owns, or GCs them:

- **consumer** external ref → read through the virtual workspace under a
  **read-only** permissionClaim (`get,list,watch`) the consumer must accept;
- **provider** external ref → read with the controller's own provider client (no
  claim);
- **host** external ref → read with the host client (no claim).

**Foreign-export invariant (inherited, unchanged).** Reading a *foreign* (non-core)
external CRD type through kcp still requires that type's owning `APIExport` to be
**bound in the provider workspace** so its `identityHash` resolves — the exact same
constraint that governs foreign consumer-target *writes*. If it is not bound,
`validateClaims` rejects the derived read-only claim and publish fails
`ClaimIdentityUnresolved`. Core types (group `""`) carry an empty identityHash and
are allowed. This invariant is not relaxed by external refs; it is documented here
because external refs are a new way to hit it.

## Minting the kubeconfig (identity)

Mint the controller's workspace-scoped kubeconfig with a kcp-operator
`Kubeconfig` CR. Use `target.rootShardRef` (not `frontProxyRef`) so the client
certificate is signed by `root-client-ca` — trusted by both the front-proxy and
every shard directly, which is required because the multicluster-provider
connects to APIExport virtual-workspace URLs that point straight at shards. List
`system:authenticated` explicitly, because kcp's front-proxy uses request-header
impersonation and impersonated identities do not otherwise get that group. (This
is the pattern from dependency-controller's `docs/getting-started.md` Step 4.)

```yaml
apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: krop-controller-kubeconfig
  namespace: kcp-system
spec:
  username: "system:serviceaccount:krop-system:krop-controller"   # == ${SA_IDENTITY}
  groups:
    - "system:authenticated"
    - "system:serviceaccounts"
    - "system:serviceaccounts:krop-system"
  validity: 8766h
  secretRef:
    name: krop-controller-kubeconfig
  target:
    rootShardRef:
      name: root
```

Rewrite the server URL to the front-proxy pinned to the **provider workspace
path** (`/clusters/root:<provider>`) before mounting the resulting kubeconfig as
the Secret referenced by `kcp.kubeconfigSecret.name`. The `${SA_IDENTITY}` in the
RBAC fixtures must match this `username` exactly.

## Applying the RBAC fixtures

Substitute the placeholders and apply each file **into its kcp workspace** (not
the hosting cluster) with a privileged setup identity (e.g. `system:kcp:admin`
via the front-proxy):

```sh
SA_IDENTITY="system:serviceaccount:krop-system:krop-controller"

# ROOT workspace
sed "s#\${SA_IDENTITY}#${SA_IDENTITY}#g" config/kcp/rbac/root-rbac.yaml \
  | kubectl --context root apply -f -

# PROVIDER workspace
sed "s#\${SA_IDENTITY}#${SA_IDENTITY}#g" config/kcp/rbac/provider-rbac.yaml \
  | kubectl --context root:<provider> apply -f -
```

`${PROVIDER_WORKSPACE}` in `provider-rbac.yaml` is documentation only — the
binding is scoped by virtue of being applied into that workspace.

## The permissionClaims spine

Cross-workspace writes require a claim/acceptance handshake:

1. **Provider side (auto-derived).** The Registrar derives one
   `permissionClaim` per foreign consumer-target GroupResource and publishes them
   on the blueprint's APIExport (`internal/registrar/claims.go`
   `DeriveClaims`/`ForeignConsumerGRs`). Verbs are split by node kind: writable
   consumer children get full CRUD (`get,list,watch,create,update,patch,delete`),
   consumer `externalRef` reads get read-only (`get,list,watch`). See
   `config/kcp/apiexport-krop-m1.yaml` for the shape (M1 hand-wrote the
   `configmaps` claim; M4 automates it from the blueprint graph).
2. **Consumer side (acceptance).** The consumer's `APIBinding` must set the
   matching claim to `state: Accepted`
   (`test/fixtures/apibinding-kubernetescluster.yaml`). Until then, writes to
   the claimed type through the virtual workspace are rejected.

The instance kind itself is served through the same APIExport; its own writes and
status updates use the accepted binding. Provider- and host-target children bypass
this entirely — they are written with the provider client (tier (c)) / the host
client (tier (e)), not a claim.

## Least-privilege checklist for operators

- [ ] Chart grants the pod **no** hosting-cluster ClusterRole/Role — confirm
      `helm template` renders only ServiceAccount + Deployment (+ CRD).
- [ ] `${SA_IDENTITY}` in both RBAC fixtures matches the `username` in the
      kcp-operator `Kubeconfig` CR and the pod's ServiceAccount.
- [ ] Kubeconfig uses `rootShardRef` and lists `system:authenticated`.
- [ ] `root-rbac.yaml` applied in the root workspace (read-only).
- [ ] `provider-rbac.yaml` applied in the provider workspace.
- [ ] The provider-target child rule in `provider-rbac.yaml` is replaced with the
      **exact** GVKs your blueprints emit — **no `*`/`*`**.
- [ ] `apiexports` rule tightened with `resourceNames` once export names are
      known.
- [ ] No RBAC granted in any consumer workspace — verify consumer-target writes
      succeed only via accepted APIBinding `permissionClaims`.
- [ ] Consumer `externalRef` reads use a **read-only** (`get,list,watch`) claim the
      consumer accepts; foreign external CRD types are **bound in the provider
      workspace** so their identityHash resolves.
- [ ] If any blueprint routes to `host`: the controller has a host client
      (in-cluster or `--host-kubeconfig` / `hostKubeconfig` chart value) and the
      **provider** has granted it host-cluster RBAC for those GVKs (krop ships no
      host ClusterRole).

## Reference files

- `charts/krop-controller/` — chart (SA only; NOTES.txt documents this model).
- `config/kcp/rbac/root-rbac.yaml`, `config/kcp/rbac/provider-rbac.yaml` — the
  fixtures.
- `config/kcp/apiexport-krop-m1.yaml`,
  `test/fixtures/apibinding-kubernetescluster.yaml` — claims + acceptance.
- `internal/registrar/claims.go`, `internal/registrar/publish.go`,
  `internal/controller/reconciler.go`, `internal/kcp/endpointslice.go` — the code
  the RBAC is scoped to.
- `cmd/controller/main.go` (`--host-kubeconfig` / in-cluster host client),
  `charts/krop-controller/values.yaml` (`hostKubeconfig`) — the host plane wiring.
- `docs/superpowers/specs/2026-07-06-krop-controller-design.md` §9 — provider vs
  consumer ownership domains.
