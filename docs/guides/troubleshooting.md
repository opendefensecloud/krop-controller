# Troubleshooting

A problem → cause → fix guide for the most common krop-controller symptoms, with
concrete `kubectl` checks. This complements the summary table in
[operations.md](../operations.md#troubleshooting) — start there for the quick
reference, come here for the step-by-step diagnosis.

> Uses the placeholders `${PROVIDER_WORKSPACE}` / `${CONSUMER_WORKSPACE}` from
> [getting-started.md](../getting-started.md).

---

## Consumer child never appears

**Symptom.** An instance exists, but its consumer-target child (e.g. a ConfigMap)
never materializes in the consumer workspace.

**Cause A — the claim was not accepted.** Consumer-target writes go through the
APIExport virtual workspace and are authorized only by an **accepted**
permissionClaim on the consumer's `APIBinding`. A `Rejected` or absent claim →
the write is silently denied.

```sh
# The binding reaches Bound even with a rejected claim — check the claim STATE, not just phase:
kubectl --context ${CONSUMER_WORKSPACE} get apibinding <name> \
  -o jsonpath='phase={.status.phase}{"\n"}{range .spec.permissionClaims[*]}{.resource}={.state}{"\n"}{end}'
# want: phase=Bound  and  configmaps=Accepted
```

Fix: set the claim to `state: Accepted` (compare
[`test/fixtures/apibinding-kubernetescluster.yaml`](../../test/fixtures/apibinding-kubernetescluster.yaml)
against the negative `...-noclaim.yaml`).

**Cause B — a foreign consumer-target type's identity is unresolved.** If the
consumer child is a *foreign* (non-core) type, its group's `identityHash` must
resolve, which requires that type's export to be **bound in the provider
workspace**. If it isn't, the blueprint won't even publish (see
[blueprint stuck not-Ready](#blueprint-stuck-not-ready), reason
`ClaimIdentityUnresolved`).

---

## Consumer child pends indefinitely (stays Progressing)

**Symptom.** The instance sits at `Ready=False`/`Progressing`; a consumer child
that reads `${someNode.status.*}` never appears.

**Cause.** A cross-target CEL dependency is unmet — the provider child's
`status.*` the consumer child reads was never populated. krop applies the provider
child but does **not** fill its status; a downstream fulfiller must.

```sh
# Find the (renamed) provider child and inspect its status:
kubectl --context ${PROVIDER_WORKSPACE} -n default get agentrequests \
  -o jsonpath='{range .items[*]}{.metadata.name}  status.token={.status.token}{"\n"}{end}'
```

Fix: have the downstream controller/operator set that status (in a demo, patch it
by hand — see [cross-target dependencies](cross-target-dependencies.md)). This is
expected behavior, not a bug: the child materializes as soon as the status appears.

---

## APIExport endpoint slice empty

**Symptom.** The `APIExportEndpointSlice` has no virtual-workspace URL, and
instances aren't being served.

**Cause.** No consumer has bound the export yet. The instance-serving path engages
a logical cluster only **once at least one consumer is bound** — the slice's URL is
empty until then.

```sh
kubectl --context ${PROVIDER_WORKSPACE} get apiexportendpointslice
```

Fix: **bind first.** Create a consumer `APIBinding` (accepting the claims); the URL
populates and the manager engages.

---

## Blueprint stuck not-Ready

**Symptom.** `kubectl get rgd <name>` shows `Ready=False`. Always read the message:

```sh
kubectl --context ${PROVIDER_WORKSPACE} get rgd <name> \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}: {.status.conditions[?(@.type=="Ready")].message}{"\n"}'
```

| Reason | Cause | Fix |
| --- | --- | --- |
| `BuildFailed` | a provider-target child CRD is **not served in the provider workspace** (kro type-checks the graph's CEL against served CRDs), or a CEL/schema error | Apply the child CRD into the provider workspace **before** the blueprint (e.g. `crd-agentrequests...yaml`), then let the resync retry. |
| `ClaimIdentityUnresolved` | a *foreign* consumer-target type's export is not **bound in the provider workspace**, so its `identityHash` can't resolve | Bind that type's export in the provider workspace; the next resync republishes. |
| `PublishFailed` | provider-workspace RBAC / `apis.kcp.io` access is missing or `${SA_IDENTITY}` mismatches | Confirm `config/kcp/rbac/provider-rbac.yaml` is applied in the provider workspace and its `${SA_IDENTITY}` equals the kubeconfig `username` + pod SA. |
| `HashFailed` | the spec could not be hashed | Read the message; usually an internal/spec issue — re-check the RGD spec. |

---

## Provider child orphaned after a consumer unbind

**Symptom.** A consumer deleted its `APIBinding` mid-life; its provider-target
children linger in the provider workspace.

**Cause & recovery.** Owner references can't cross workspaces, and once the
consumer unbinds, the virtual workspace disengages that cluster — the instance
reconciler stops running for it, so the delete finalizer never fires. The
**Sweeper** reclaims these via the per-instance **liveness record**: a ConfigMap
that goes stale when reconciles stop, after which the Sweeper deletes the recorded
provider children (default: reclaimed after `StaleAfter` = 5m).

```sh
# Inspect liveness records and their freshness:
kubectl --context ${PROVIDER_WORKSPACE} -n default get configmaps \
  -l krop.opendefense.cloud/liveness=true \
  -o jsonpath='{range .items[*]}{.metadata.name}  lastReconciled={.data.lastReconciled}{"\n"}{end}'
```

Fix / checks:
- Wait out `StaleAfter` (5m) plus the startup grace period.
- **Verify the controller isn't crashlooping** — every restart resets the startup
  grace, deferring the first sweep. `kubectl -n <ns> get pods -l app.kubernetes.io/name=krop-controller`.
- Details + tuning invariant:
  [operations.md](../operations.md#garbage-collection--the-orphan-sweep).

---

## Controller pod can't reach kcp

**Symptom.** The pod won't start, or logs `kubeconfig must be workspace-scoped`, or
does nothing after an instance is created.

**Cause A — kubeconfig not workspace-scoped.** The mounted kubeconfig's server URL
must contain `/clusters/<provider-path>`. The binary's `ValidateKubeconfig`
rejects anything else.

```sh
kubectl -n <ns> get secret krop-kubeconfig -o jsonpath='{.data.kubeconfig}' \
  | base64 -d | grep -E 'server:'
# the server URL must contain /clusters/<provider-workspace-path>
```

Fix: re-mint via the kcp-operator `Kubeconfig` CR and **re-pin** the server URL to
the provider workspace path (see [deploying in production](deploying-in-production.md)).

**Cause B — instance created but nothing happens.** The provider can't serve
through the virtual workspace because the `apis.kcp.io/apiexports/content` grant in
`provider-rbac.yaml` is missing/mis-scoped → the endpoint watcher fails discovery
with `access denied` (invisible when testing as admin). Confirm the fixture is
applied unmodified and `${SA_IDENTITY}` matches. See
[permissions.md](../permissions.md#c-kcp-provider-workspace--the-controllers-real-work).

---

## Duplicate work / churn with multiple replicas

**Symptom.** `replicaCount > 1` and you see duplicated reconciles or status churn.

**Cause.** Leader election is off, so multiple managers are active.

**Fix.** Set `controller.leaderElect=true` (required for any `replicaCount > 1`).
See [deploying in production §4](deploying-in-production.md#4-install-with-ha-and-metrics).

---

For internals behind any of these, see [architecture.md](../architecture.md) and
the [decision records](../decisions/).
