# Operations

For operators running krop-controller in production. Covers RBAC & identity,
configuration/flags, observability, garbage collection & the orphan sweep,
upgrades, and troubleshooting.

New here? Do [getting-started.md](getting-started.md) first, and read
[permissions.md](permissions.md) for the full authorization model (this page
summarizes and links it rather than duplicating it).

---

## RBAC & identity

krop does **almost nothing** against the cluster it is deployed on. It runs a kcp
multicluster-runtime manager and authorizes against **kcp** through a mounted,
workspace-scoped kubeconfig. The full model — the three-tier posture, how identity
is minted, the permissionClaims spine, and a checklist — is
[permissions.md](permissions.md). The essentials an operator must get right:

- **Hosting cluster: ServiceAccount only.** The chart renders **no**
  ClusterRole/Role in the hosting cluster (`helm template` → ServiceAccount +
  Deployment + CRD). The pod's SA exists only to mount the kubeconfig Secret. If
  you see the chart grant host-cluster RBAC, something is wrong.
- **The workspace-scoped kubeconfig is the identity.** The mounted kubeconfig's
  client cert authenticates as `system:serviceaccount:<ns>:<sa>` (written
  `${SA_IDENTITY}`). It must be **pinned to the provider workspace path** — the
  binary's `ValidateKubeconfig` rejects a non-workspace-scoped kubeconfig. Mint it
  with a kcp-operator `Kubeconfig` CR using `rootShardRef` and listing
  `system:authenticated` (see
  [permissions.md](permissions.md#minting-the-kubeconfig-identity)).
- **Two kcp RBAC fixtures.** Apply `config/kcp/rbac/root-rbac.yaml` into the
  **root** workspace (read-only: enter child workspaces, resolve paths) and
  `config/kcp/rbac/provider-rbac.yaml` into the **provider** workspace (the
  controller's real work). `${SA_IDENTITY}` in both must equal the `Kubeconfig`
  CR's `username` and the pod SA.
- **The deployment-specific provider-child rule you MUST extend.**
  `provider-rbac.yaml` ships an *example* rule
  (`fulfil.krop.opendefense.cloud/agentrequests`) that matches krop's example
  blueprint. The GVKs *your* blueprints write to the provider workspace are known
  only to you — replace/extend that rule with your **exact** provider-target GVKs.
  **Never** use `*`/`*`; that re-creates the broad grant this model removes. Keep
  the `configmaps` rule — the controller writes per-instance liveness records
  there for the orphan sweep.
- **Consumer workspaces get zero standing RBAC.** Consumer-target writes are
  authorized only by the consumer's **accepted** permissionClaims through the
  APIExport virtual workspace.

---

## Configuration & flags

The binary (`cmd/controller/main.go`) accepts:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--health-probe-bind-address` | `:8081` | healthz/readyz endpoint; also derives the container/probe port |
| `--metrics-bind-address` | `:8080` | the **provider** manager's metrics endpoint; `0` disables |
| `--leader-elect` | `false` | single active manager; creates a `coordination.k8s.io` Lease in the provider workspace |
| `--leader-election-namespace` | `$POD_NAMESPACE` → `default` | namespace of the leader-election Lease (in the provider workspace) |
| `--zap-log-level`, `--zap-devel`, `--zap-encoder`, … | production JSON | zap logging (controller-runtime) |

The kubeconfig is **not** a flag: the binary calls `ctrl.GetConfigOrDie()`, which
honors `KUBECONFIG` (set by the chart to `/etc/kcp/<key>` when a kubeconfig Secret
is configured) and otherwise falls back to in-cluster/default config — which is
**not** workspace-scoped and fails `ValidateKubeconfig`.

### Chart values that render them

`charts/krop-controller/values.yaml`:

| Value | Renders / effect |
| --- | --- |
| `controller.healthProbeBindAddress` | `--health-probe-bind-address` + the container/probe port |
| `controller.metricsBindAddress` | `--metrics-bind-address` (`"0"` disables) |
| `controller.leaderElect` | adds `--leader-elect` when true |
| `controller.extraArgs` | appended verbatim (use for `--zap-log-level`, `--zap-devel`, `--leader-election-namespace`, …) |
| `kcp.kubeconfigSecret.name` / `.key` | mounts the Secret at `/etc/kcp` and sets `KUBECONFIG=/etc/kcp/<key>`; **no mount** when name is empty |
| `image.repository` / `.tag` / `.pullPolicy` | container image (`ghcr.io/opendefense/krop-controller`; tag defaults to `appVersion`) |
| `serviceAccount.create` / `.name` | the pod SA whose identity `${SA_IDENTITY}` must match |
| `replicaCount` | pod replicas — set `>1` **only** with `controller.leaderElect=true` |

Metrics are served **only** by the single provider manager. The per-blueprint
instance managers keep metrics disabled (`BindAddress: "0"`) so that N published
blueprints do not all fight for `:8080`.

### High availability

Run HA by setting `replicaCount > 1` **together with**
`controller.leaderElect=true`, so exactly one manager is active at a time:

```sh
helm upgrade krop charts/krop-controller \
  --set replicaCount=2 --set controller.leaderElect=true \
  --set kcp.kubeconfigSecret.name=krop-kubeconfig
```

Leader election creates a Lease in the provider workspace in
`--leader-election-namespace` (default: the pod namespace via `POD_NAMESPACE`,
injected by the chart from the downward API). `replicaCount > 1` **without**
leader election runs multiple active managers and is unsupported.

---

## Observability

- **Metrics.** The provider manager exposes controller-runtime metrics on
  `controller.metricsBindAddress` (`:8080`). The chart does **not** create a
  Service/ServiceMonitor — wire your own scrape target (e.g. a Service +
  ServiceMonitor) at that port.
- **Health / readiness.** `livenessProbe` → `GET /healthz`, `readinessProbe` →
  `GET /readyz`, both on the health port derived from
  `controller.healthProbeBindAddress` (`:8081` → port `8081`). Both are
  `healthz.Ping` checks.
- **Logs.** Structured zap; production JSON by default. Tune with `--zap-log-level`
  / `--zap-devel` via `controller.extraArgs`. The entrypoint logs the effective
  `version`, bind addresses, leader-election settings, and the derived provider
  `workspace` on startup.
- **Status conditions** — the primary operational signal:

  | Object | Condition | Meaning |
  | --- | --- | --- |
  | Blueprint (RGD) `.status` | `Ready=True` reason `Published` | published; `.status.exportedAPI` names the APIExport, `.status.identityHash`/`.observedSpecHash` recorded |
  | Blueprint `.status` | `Ready=False` reason `BuildFailed` / `PublishFailed` / `ClaimIdentityUnresolved` / `HashFailed` | publish failed; read `.message` |
  | Instance `.status` | `Ready` | `True` when every included child passed readiness |
  | Instance `.status` | `Progressing` | `True` while still converging (e.g. a consumer child pending on a provider status) |

  Instances also carry the blueprint's `schema.status.*` CEL projections (e.g.
  `configMapName`, `agentToken`) alongside these conditions.

---

## Garbage collection & the orphan sweep

krop reclaims children through **two** mechanisms
([architecture.md](architecture.md#43-garbage-collection),
[decisions/0007-cross-workspace-gc-strategy.md](decisions/0007-cross-workspace-gc-strategy.md)).

### 1. Instance-delete GC (the common path)

Each instance carries a **finalizer**. On delete, the reconciler deletes the
instance's labeled children in **both** workspaces (consumer-target via the vw;
provider-target via the controller's own client) and then removes the finalizer.
Provider children are found by an **instance-UID label** (owner references cannot
cross workspaces), consumer children by label within the consumer workspace.

### 2. The orphan sweep (mid-life unbind)

The finalizer only fires while the instance is still **observable**. If a consumer
**unbinds** the APIExport mid-life, the virtual workspace disengages that logical
cluster: the instance reconciler stops running for it, the finalizer never fires,
and its provider-target children would orphan forever. The `Sweeper`
(`internal/controller/sweeper.go`, run on the provider manager) recovers them:

- On **every complete reconcile pass**, the reconciler upserts a per-instance
  **liveness record** — a `ConfigMap` in the record namespace (`default`), labeled
  `krop.opendefense.cloud/liveness=true`, carrying the instance-UID label, a
  `lastReconciled` RFC3339 timestamp, and the provider-child GVKs to delete. A
  live instance keeps its record fresh (the reconciler requeues ~every 30s).
- The Sweeper ticks every `SweepInterval` (1m). For any record whose
  `lastReconciled` is older than `StaleAfter` (5m), it deletes that instance's
  recorded provider children (by instance-UID label, per GVK) and then the record.
- A **startup grace period** of `StaleAfter` defers the first sweep: after any
  restart/redeploy/crashloop, every record is already stale while the controller
  re-discovers endpoint slices and re-reconciles the fleet to refresh records.
  Without the grace, the first tick would wrongly sweep still-live instances.

**Tuning invariant.** `StaleAfter` must comfortably exceed the **worst-case
fleet catch-up after a restart** (endpoint-slice rediscovery ≈30s each +
re-reconcile of every instance), not merely the ~30s heartbeat. The shipped 5m is
~10x the heartbeat and is intentionally conservative — the cost is a bounded delay
before an orphan is reclaimed; the benefit is never sweeping a live instance.
These values live as `orphanStaleAfter` / `orphanSweepInterval` in
`cmd/controller/main.go` and are not currently chart-exposed.

---

## Upgrades & blueprint changes

- **Chart / image upgrade.** `helm upgrade krop charts/krop-controller --set
  image.tag=<new>` (re-pass your existing `--set`s). With HA + leader election the
  rollout keeps a single active manager; on restart the controller re-discovers
  every published export's endpoint slice and resumes serving (and defers the
  orphan sweep by the startup grace period).
- **Live blueprint spec edit.** The Registrar detects the new spec hash and
  **restarts** that blueprint's per-export instance manager to serve the new
  compiled graph (`Stop`+`Ensure`); an unchanged 5m resync restarts nothing. This
  is single-version serving — an incompatible schema change re-serves existing
  instances under the new graph (see
  [blueprints.md](blueprints.md#schema-evolution-live-blueprint-edits) and the
  [known limitations](architecture.md#7-known-limitations--future-work)). For
  breaking changes, publish under a new `version`/`kind`.
- **Blueprint deletion.** The blueprint's finalizer cascade-unpublishes the API:
  it deletes the `APIExport` + `APIResourceSchema` and stops the instance manager.
  **Bound consumers lose the type** — treat blueprint deletion as an API
  withdrawal and coordinate with consumers first.

---

## Troubleshooting

| Symptom | Likely cause | What to check / do |
| --- | --- | --- |
| Consumer child never appears | claim not accepted | Consumer's APIBinding must set the derived claim to `state: Accepted` (not `Rejected`/absent). Compare `test/fixtures/apibinding-kubernetescluster.yaml` vs `...-noclaim.yaml`. |
| Consumer child never appears (foreign type) | foreign identity unresolved | The foreign consumer-target type's export must be **bound in the provider workspace** so its `identityHash` resolves. Otherwise publish fails `ClaimIdentityUnresolved`. |
| Consumer child **pends** indefinitely | cross-target CEL dependency unmet | The child reads a provider child's `status.*` that is not set. Set/await that status (downstream fulfilment). Instance stays `Progressing`. |
| APIExport endpoint slice empty | no consumer bound yet | Expected before the first bind — **bind first**. The instance-serving path engages a logical cluster only once bound. |
| Blueprint stuck `Ready=False` `BuildFailed` | graph build failed | A provider-target child CRD is not **served in the provider workspace**, or a CEL/schema error. kro type-checks CEL against served CRDs — apply the child CRD first. Read `.status.conditions[Ready].message`. |
| Blueprint stuck `Ready=False` `ClaimIdentityUnresolved` | foreign export not bound | Bind the foreign consumer-target type's export in the provider workspace; the resync republishes. |
| Blueprint stuck `Ready=False` `PublishFailed` | provider-workspace RBAC / apis.kcp.io access | Confirm `provider-rbac.yaml` is applied in the provider workspace and `${SA_IDENTITY}` matches. |
| Instance created but **nothing** happens | provider can't serve through the vw | The `apiexports/content` grant in `provider-rbac.yaml` is missing/mis-scoped → the endpoint watcher fails discovery with `access denied` (invisible to admin-context testing). See [permissions.md](permissions.md). |
| Provider child orphaned after a consumer unbind | sweeper/liveness | Check the liveness record ConfigMaps (`-l krop.opendefense.cloud/liveness=true` in the record namespace) and their `lastReconciled`; the Sweeper reclaims after `StaleAfter`. Verify the controller isn't crashlooping (which resets the startup grace). |
| Pod won't start: "kubeconfig must be workspace-scoped" | kubeconfig not pinned to a workspace | The mounted kubeconfig's server URL must contain `/clusters/<provider-path>`. Re-mint via the `Kubeconfig` CR and re-pin. |
| `replicaCount>1` but duplicate work / churn | leader election off | Set `controller.leaderElect=true`. |

For deeper internals behind any of these, see
[architecture.md](architecture.md) and the ADRs in [decisions/](decisions/).
