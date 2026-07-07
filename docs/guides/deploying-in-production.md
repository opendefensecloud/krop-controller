# Deploying in production

A concise deploy recipe for running krop-controller for real: mint the
workspace-scoped identity, grant least-privilege kcp RBAC, and `helm install` with
HA and metrics. This guide is the short path — for the full model and depth, follow
the links to [operations.md](../operations.md) and
[permissions.md](../permissions.md) rather than re-reading it all here.

> Placeholders: `${NS}` = hosting namespace (e.g. `krop-system`),
> `${PROVIDER_WORKSPACE}` = the provider workspace path (e.g. `root:krop-provider`),
> `${SA_IDENTITY}` = `system:serviceaccount:${NS}:krop-controller`.

---

## 1. Mint the workspace-scoped kubeconfig

The controller authenticates to kcp as `${SA_IDENTITY}` with a client certificate,
pinned to the provider workspace path. Mint it with a kcp-operator `Kubeconfig` CR
using **`rootShardRef`** and listing **`system:authenticated`** — both are
mandatory (the reasons are in
[permissions.md](../permissions.md#minting-the-kubeconfig-identity)):

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

Rewrite the resulting kubeconfig's server URL so it is **pinned to the provider
workspace** (`.../clusters/${PROVIDER_WORKSPACE}`). The binary rejects a
non-workspace-scoped kubeconfig (`ValidateKubeconfig` requires a `/clusters/`
segment).

---

## 2. Create the kubeconfig Secret

The chart does **not** template this Secret — provide it out-of-band, with the
kubeconfig under the key `kubeconfig`:

```sh
kubectl -n ${NS} create secret generic krop-kubeconfig \
  --from-file=kubeconfig=./controller.kubeconfig
```

---

## 3. Apply least-privilege kcp RBAC

Apply the two shipped fixtures **into their kcp workspaces** (not the hosting
cluster) as a privileged setup identity:

```sh
SA_IDENTITY="system:serviceaccount:${NS}:krop-controller"

# ROOT workspace — read-only: enter child workspaces, resolve paths.
sed "s#\${SA_IDENTITY}#${SA_IDENTITY}#g" config/kcp/rbac/root-rbac.yaml \
  | kubectl --context root apply -f -

# PROVIDER workspace — the controller's real authority.
sed "s#\${SA_IDENTITY}#${SA_IDENTITY}#g" config/kcp/rbac/provider-rbac.yaml \
  | kubectl --context ${PROVIDER_WORKSPACE} apply -f -
```

> ⚠️ **Extend the provider-child rule for YOUR blueprints.**
> `config/kcp/rbac/provider-rbac.yaml` ships an *example* rule
> (`fulfil.krop.opendefense.cloud/agentrequests`) that matches krop's example
> blueprint only. Replace/extend it with the **exact** provider-target GVKs your
> blueprints emit — **never** `*`/`*`. Keep the `configmaps` rule: the controller
> writes per-instance liveness records there for the orphan sweep. Full checklist:
> [permissions.md](../permissions.md#c-kcp-provider-workspace--the-controllers-real-work).

---

## 4. Install with HA and metrics

Run HA by setting `replicaCount > 1` **together with** leader election, so exactly
one manager is active at a time:

```sh
helm install krop charts/krop-controller \
  --namespace ${NS} --create-namespace \
  --set image.tag=<your-tag> \
  --set kcp.kubeconfigSecret.name=krop-kubeconfig \
  --set replicaCount=2 \
  --set controller.leaderElect=true
```

Notes:

- **`replicaCount > 1` without `controller.leaderElect=true` is unsupported** — it
  runs multiple active managers. Leader election creates a Lease in the provider
  workspace in `--leader-election-namespace` (defaults to the pod namespace via
  `POD_NAMESPACE`).
- **Metrics** are served only by the single provider manager on
  `controller.metricsBindAddress` (`:8080`). The chart does **not** create a
  Service/ServiceMonitor — wire your own scrape target at that port. Set
  `--set controller.metricsBindAddress=0` to disable.
- **Health probes** hit `/healthz` and `/readyz` on the port derived from
  `controller.healthProbeBindAddress` (`:8081`).
- **Logging / extra flags** go through `controller.extraArgs` (e.g.
  `--zap-log-level`, `--zap-devel`, `--leader-election-namespace`).

Confirm the least-privilege posture — the chart renders **no** host-cluster
ClusterRole/Role:

```sh
helm template krop charts/krop-controller --set kcp.kubeconfigSecret.name=x \
  | grep -E '^kind:' | sort | uniq -c
# ->  1 CustomResourceDefinition   1 Deployment   1 ServiceAccount

kubectl -n ${NS} rollout status deploy/krop
```

---

## 5. Publish blueprints

Install the blueprint CRD into the provider workspace and create your blueprints —
covered in [getting-started.md §3](../getting-started.md#3-author--publish-a-blueprint)
and the [authoring reference](../blueprints.md). Each provider-target GVK a
blueprint emits must have a matching RBAC rule from step 3.

---

## Where next

- [operations.md](../operations.md) — the full flag/values table, observability,
  garbage collection & the orphan sweep tuning, upgrades, and troubleshooting.
- [permissions.md](../permissions.md) — the complete least-privilege model and
  operator checklist.
- [Troubleshooting](troubleshooting.md) — when something doesn't come up.
