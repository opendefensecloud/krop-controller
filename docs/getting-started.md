# Getting started

This is a hands-on walkthrough. By the end you will have:

1. deployed the krop-controller into a hosting cluster with a kcp identity,
2. published your first `ResourceGraphDefinition` blueprint as a bindable
   `APIExport`,
3. bound a consumer workspace and created an instance,
4. watched the instance's children materialize across the consumer and provider
   workspaces, and
5. cleaned everything up.

krop-controller runs a kcp multicluster-runtime manager over a **provider
workspace**: it watches blueprints, compiles each into an `APIExport`, and
reconciles the generated instance kind across every **consumer workspace** that
binds it. No Go controller per offering — one declarative blueprint per API.

For the "why", read [architecture.md](architecture.md). For the authorization
model in full, read [permissions.md](permissions.md); this guide summarizes only
what you need to run the steps.

> Placeholders used below — substitute your own values:
>
> | Placeholder | Meaning | Example |
> | --- | --- | --- |
> | `${PROVIDER_WORKSPACE}` | the kcp workspace path the controller is pinned to | `root:krop-provider` |
> | `${CONSUMER_WORKSPACE}` | a tenant workspace that binds the export | `root:krop-consumer` |
> | `${SA_IDENTITY}` | the controller's kcp identity | `system:serviceaccount:krop-system:krop-controller` |
> | `${NS}` | the hosting-cluster namespace krop runs in | `krop-system` |

---

## 1. Prerequisites

- A **kcp instance** with a front-proxy you can reach. The reference way to stand
  one up is [kcp-operator](https://docs.kcp.io/) on a Kubernetes cluster (etcd +
  `RootShard` + `FrontProxy`), which is exactly what krop's own end-to-end test
  provisions.
- A **provider workspace** under `root` (e.g. `root:krop-provider`) that the
  controller will own.
- `kubectl` and `helm` v3.
- The controller image: `ghcr.io/opendefense/krop-controller` (override the tag
  with `--set image.tag=...`; the chart's `appVersion` is `latest`).

> **Want a full, reproducible kcp-on-kind stack?** `make test-e2e`
> (`test/e2e/suite_test.go`) builds a kind cluster, installs cert-manager and
> kcp-operator, creates a `RootShard` + `FrontProxy` + etcd, mints the
> controller's workspace-scoped kubeconfig, creates the provider/consumer
> workspaces, applies the CRDs + RBAC + blueprint, and deploys this very Helm
> chart as a pod. It is the authoritative, executable version of this guide — run
> it with `E2E_SKIP_CLEANUP=1` to keep the cluster and poke around.

Throughout, `kubectl` against kcp is pointed at a **workspace** (via a
workspace-scoped kubeconfig context, or `--server .../clusters/root:<ws>`). This
guide writes those as `kubectl --context ${PROVIDER_WORKSPACE}` etc. — map them to
however your kubeconfig addresses kcp workspaces.

---

## 2. Deploy the controller

The controller does **no** business logic against the hosting cluster. It
authenticates to kcp through a mounted, workspace-scoped kubeconfig and does all
its work there. So deployment is three moves: mint an identity, grant it
least-privilege kcp RBAC, install the chart.

### 2a. Mint the controller's kcp kubeconfig

The controller connects to the provider workspace as `${SA_IDENTITY}` using a
client certificate. Mint that certificate with a kcp-operator `Kubeconfig` CR —
the full pattern, and *why* it must use `rootShardRef` and list
`system:authenticated`, is in [permissions.md](permissions.md#minting-the-kubeconfig-identity).
In brief:

```yaml
apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: krop-controller-kubeconfig
  namespace: kcp-system
spec:
  username: "${SA_IDENTITY}"          # e.g. system:serviceaccount:krop-system:krop-controller
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
workspace path** (`.../clusters/${PROVIDER_WORKSPACE}`). The binary rejects a
kubeconfig that is not workspace-scoped (`ValidateKubeconfig` requires a
`/clusters/` segment), so this pinning is mandatory.

### 2b. Create the kubeconfig Secret in the hosting cluster

The chart does **not** template the Secret — you provide it out-of-band. Put the
workspace-scoped kubeconfig under the key `kubeconfig`:

```sh
kubectl -n ${NS} create secret generic krop-kubeconfig \
  --from-file=kubeconfig=./controller.kubeconfig
```

### 2c. Grant least-privilege kcp RBAC

Apply the two shipped fixtures **into their kcp workspaces** (not the hosting
cluster) as a privileged setup identity, substituting `${SA_IDENTITY}`:

```sh
SA_IDENTITY="system:serviceaccount:${NS}:krop-controller"

# ROOT workspace — read-only: enter child workspaces + resolve paths.
sed "s#\${SA_IDENTITY}#${SA_IDENTITY}#g" config/kcp/rbac/root-rbac.yaml \
  | kubectl --context root apply -f -

# PROVIDER workspace — the controller's real authority.
sed "s#\${SA_IDENTITY}#${SA_IDENTITY}#g" config/kcp/rbac/provider-rbac.yaml \
  | kubectl --context ${PROVIDER_WORKSPACE} apply -f -
```

> ⚠️ **Read `config/kcp/rbac/provider-rbac.yaml` before applying it.** Its
> provider-target child rule ships an *example* (`fulfil.krop.opendefense.cloud/
> agentrequests`) matching krop's example blueprint. You **must** replace/extend
> it with the exact GVKs *your* blueprints write to the provider workspace —
> never a `*`/`*` wildcard. Details + the full least-privilege checklist:
> [permissions.md](permissions.md#c-kcp-provider-workspace--the-controllers-real-work).

### 2d. Install the chart

```sh
helm install krop charts/krop-controller \
  --namespace ${NS} --create-namespace \
  --set image.tag=<your-tag> \
  --set kcp.kubeconfigSecret.name=krop-kubeconfig
```

The blueprint CRD in `charts/krop-controller/crds/` installs with the chart on
the hosting cluster; you separately install it **into the provider workspace** in
step 3.

**Confirm the least-privilege posture** — the chart renders only a
ServiceAccount + Deployment (plus the CRD), and **no** ClusterRole/Role:

```sh
helm template krop charts/krop-controller --set kcp.kubeconfigSecret.name=x \
  | grep -E '^kind:' | sort | uniq -c
# ->  1 CustomResourceDefinition   1 Deployment   1 ServiceAccount
```

The Deployment mounts the Secret at `/etc/kcp` and sets
`KUBECONFIG=/etc/kcp/kubeconfig`; the pod's ServiceAccount exists only to mount
that Secret. Verify it is Ready:

```sh
kubectl -n ${NS} rollout status deploy/krop
kubectl -n ${NS} get sa,deploy
```

---

## 3. Author & publish a blueprint

### 3a. Install the blueprint CRD into the provider workspace

```sh
kubectl --context ${PROVIDER_WORKSPACE} apply -f \
  charts/krop-controller/crds/krop.opendefense.cloud_resourcegraphdefinitions.yaml
```

Our worked example's provider-target child is an `AgentRequest`. Its CRD must be
**served in the provider workspace before you create the blueprint**, because kro
type-checks the CEL expression `${agentRequest.status.token}` against it when it
builds the graph:

```sh
kubectl --context ${PROVIDER_WORKSPACE} apply -f \
  test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml
```

### 3b. Create the blueprint

`test/fixtures/blueprint-kubernetescluster-rgd.yaml` is a
`krop.opendefense.cloud/v1alpha1 ResourceGraphDefinition` whose spec *is* kro's
RGD spec: a `schema` (region in, `configMapName`/`agentToken` out), a
**provider-target** `AgentRequest` child, and a **consumer-target** `ConfigMap`
that reads `${agentRequest.status.token}`. See [blueprints.md](blueprints.md) for
the full anatomy.

```sh
kubectl --context ${PROVIDER_WORKSPACE} apply -f \
  test/fixtures/blueprint-kubernetescluster-rgd.yaml
```

### 3c. Watch it publish

The Registrar compiles the blueprint into an `APIResourceSchema`, server-side
applies an `APIExport` named `<plural>.<group>` (here
`kubernetesclusters.krop.opendefense.cloud`), and derives its `permissionClaims`.
The blueprint's `.status` reaches `Ready` with `exportedAPI` set:

```sh
kubectl --context ${PROVIDER_WORKSPACE} get apiexport
# NAME
# kubernetesclusters.krop.opendefense.cloud

kubectl --context ${PROVIDER_WORKSPACE} get apiresourceschema

kubectl --context ${PROVIDER_WORKSPACE} get rgd kubernetescluster \
  -o jsonpath='{.status.exportedAPI}{"\t"}{.status.conditions[?(@.type=="Ready")].reason}{"\n"}'
# kubernetesclusters.krop.opendefense.cloud   Published
```

If `Ready` is `False`, read the condition `reason`/`message` — see
[operations.md](operations.md#troubleshooting) (common causes: the provider-child
CRD is not served in the provider workspace → `BuildFailed`; a foreign
consumer-target type is not bound in the provider workspace →
`ClaimIdentityUnresolved`).

---

## 4. Consume it

### 4a. Bind the export — accepting the auto-derived claim

The blueprint's consumer-target `ConfigMap` means the controller writes into the
consumer workspace **through the APIExport's virtual workspace**. That write is
authorized only if the consumer's `APIBinding` has **accepted** the auto-derived
`configmaps` permissionClaim (`test/fixtures/apibinding-kubernetescluster.yaml`):

```yaml
apiVersion: apis.kcp.io/v1alpha2
kind: APIBinding
metadata:
  name: kubernetesclusters
spec:
  reference:
    export:
      path: ${PROVIDER_WORKSPACE}                        # e.g. root:krop-provider
      name: kubernetesclusters.krop.opendefense.cloud
  permissionClaims:
    - group: ""
      resource: configmaps
      verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
      selector:
        matchAll: true
      state: Accepted          # <-- acceptance is what authorizes the cross-workspace write
```

```sh
sed "s#\${PROVIDER_PATH}#${PROVIDER_WORKSPACE}#g" \
  test/fixtures/apibinding-kubernetescluster.yaml \
  | kubectl --context ${CONSUMER_WORKSPACE} apply -f -

kubectl --context ${CONSUMER_WORKSPACE} get apibinding kubernetesclusters \
  -o jsonpath='{.status.phase}{"\n"}'   # -> Bound
```

> **Why acceptance matters:** a binding that sets the claim to `Rejected` still
> reaches `Bound`, but the cross-workspace ConfigMap write is denied and the
> child never appears. `test/fixtures/apibinding-kubernetescluster-noclaim.yaml`
> is exactly that negative case.

> **Endpoint slice is empty until someone binds.** The APIExport's instance-serving
> path engages a consumer's logical cluster only once that consumer is bound.
> If the endpoint slice looks empty, that is expected before the first bind —
> **bind first.**

### 4b. Create an instance

```sh
cat <<'EOF' | kubectl --context ${CONSUMER_WORKSPACE} apply -f -
apiVersion: krop.opendefense.cloud/v1alpha1
kind: KubernetesCluster
metadata:
  name: demo
  namespace: default
spec:
  region: eu
EOF
```

### 4c. Watch the children appear across workspaces

The reconcile is dual-target and CEL-linked. Here is the flow:

```
consumer ws                          provider ws
-----------                          -----------
KubernetesCluster{region: eu}
   │  (served via APIExport vw)
   ├─ provider-target child ───────► AgentRequest "eu-agent"   (controller identity)
   │                                     │  status.token empty → consumer child PENDS
   │                                     ▼
   │                                 [downstream sets status.token]
   ▼                                     │
ConfigMap "eu-cluster-config" ◄──────────┘  materializes once token is set
   (written through vw, authorized by the accepted claim; data.token = ${agentRequest.status.token})
```

The provider `AgentRequest` appears immediately (written by the controller's own
identity into the provider workspace, `default` namespace, under a collision-free
generated name):

```sh
kubectl --context ${PROVIDER_WORKSPACE} -n default get agentrequests
```

The consumer `ConfigMap` **pends** because it reads `${agentRequest.status.token}`
and that status is not set yet. In production a downstream fulfiller sets it; here
we simulate that:

```sh
AR=$(kubectl --context ${PROVIDER_WORKSPACE} -n default get agentrequests \
  -o jsonpath='{.items[0].metadata.name}')
kubectl --context ${PROVIDER_WORKSPACE} -n default patch agentrequest "$AR" \
  --subresource=status --type=merge -p '{"status":{"token":"tok-demo-42"}}'
```

Now the consumer ConfigMap materializes with the propagated token, and the
instance status maps the provider child's status:

```sh
kubectl --context ${CONSUMER_WORKSPACE} -n default get configmap eu-cluster-config \
  -o jsonpath='{.data.token}{"\n"}'                        # -> tok-demo-42

kubectl --context ${CONSUMER_WORKSPACE} -n default get kubernetescluster demo \
  -o jsonpath='status.agentToken={.status.agentToken}{"\n"}status.configMapName={.status.configMapName}{"\n"}Ready={.status.conditions[?(@.type=="Ready")].status}{"\n"}'
# status.agentToken=tok-demo-42
# status.configMapName=eu-cluster-config
# Ready=True
```

`status.agentToken`/`status.configMapName` are the blueprint's
`schema.status.*` CEL projections; `Ready`/`Progressing` are conditions the
reconciler writes from the engine result (see
[operations.md](operations.md#observability)).

---

## 5. Clean up

Delete the instance. Its finalizer runs a **cross-workspace** garbage collection:
the provider `AgentRequest` and the consumer `ConfigMap` are both removed, then
the instance itself.

```sh
kubectl --context ${CONSUMER_WORKSPACE} -n default delete kubernetescluster demo

kubectl --context ${PROVIDER_WORKSPACE} -n default get agentrequests    # -> gone
kubectl --context ${CONSUMER_WORKSPACE} -n default get configmap eu-cluster-config  # -> NotFound
```

Deleting the **blueprint** cascade-unpublishes the API (its finalizer deletes the
`APIExport` + `APIResourceSchema`); bound consumers lose the type:

```sh
kubectl --context ${PROVIDER_WORKSPACE} delete rgd kubernetescluster
```

Uninstall the controller:

```sh
helm uninstall krop -n ${NS}
```

---

## Where next

- [blueprints.md](blueprints.md) — author your own blueprints: schema, resources,
  CEL, and consumer/provider target routing.
- [operations.md](operations.md) — run krop in production: flags, HA,
  observability, garbage collection, upgrades, and troubleshooting.
- [permissions.md](permissions.md) — the complete least-privilege model and
  checklist.
- [architecture.md](architecture.md) — how it is built and why.
