# Cross-target dependencies

A recipe for krop's signature pattern: a **consumer-target** child that consumes a
**provider-target** child's `status` via CEL. Concretely, the
`AgentRequest → token → ConfigMap` flow — a provider-side fulfilment request whose
resulting token is surfaced to the tenant in a consumer-side ConfigMap.

This guide assumes you have done [writing your first blueprint](writing-your-first-blueprint.md)
(single consumer child) and understand target routing. For the internals of *why*
this pends and converges, see
[architecture.md §4.2](../architecture.md#42-instance-reconcile-dual-target--cross-target-cel).

---

## The pattern

Two children in one blueprint:

- a **provider-target** `AgentRequest` (lands in the provider workspace, written
  with the controller's own identity), and
- a **consumer-target** `ConfigMap` (lands in the consumer workspace, via the vw +
  accepted claim) whose `data.token` reads `${agentRequest.status.token}`.

The real fixture is
[`test/fixtures/blueprint-kubernetescluster-rgd.yaml`](../../test/fixtures/blueprint-kubernetescluster-rgd.yaml):

```yaml
apiVersion: krop.opendefense.cloud/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: kubernetescluster
spec:
  schema:
    apiVersion: v1alpha1
    kind: KubernetesCluster
    group: krop.opendefense.cloud
    spec:
      region: string
    status:
      agentToken: ${agentRequest.status.token}      # projection of the provider child's live status
  resources:
    - id: agentRequest                                # PROVIDER-target child
      template:
        apiVersion: fulfil.krop.opendefense.cloud/v1alpha1
        kind: AgentRequest
        metadata:
          name: ${schema.spec.region}-agent
          namespace: default
          annotations:
            krop.opendefense.cloud/target: provider
        spec:
          region: ${schema.spec.region}
    - id: config                                      # CONSUMER-target child
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.region}-cluster-config
          namespace: default
          annotations:
            krop.opendefense.cloud/target: consumer
        data:
          token: ${agentRequest.status.token}         # cross-target CEL read
```

The `${agentRequest.status.token}` reference in the ConsumerConfigMap creates a
**dependency edge** from `config` to `agentRequest`. kro topologically orders the
graph, so krop applies `agentRequest` first.

---

## Pend-until-ready behavior

`${agentRequest.status.token}` cannot resolve until the `AgentRequest` exists **and
something has populated its `status.token`**. krop does not fail on this — it
**pends**:

1. krop applies the provider `AgentRequest` and reads its object back (the
   [`SSAApplier` read-back](../architecture.md#the-applier-decorator-chain) is what
   makes the status observable).
2. `status.token` is still empty, so `GetDesired` on the consumer `ConfigMap`
   returns `ErrDataPending`. The reconcile reports **not complete**, sets the
   instance `Ready=False`/`Progressing`, and requeues (~30s).
3. Crucially, an incomplete pass **suppresses prune** — the already-applied
   provider child is never mistaken for the full desired set and reclaimed.
4. Once `status.token` is set, the next pass resolves the CEL, materializes the
   `ConfigMap`, and the instance flips to `Ready=True`.

So a consumer child that depends on a provider status will simply **not appear**
until that status exists. That is expected, not an error.

---

## What sets the provider child's status?

krop applies the provider `AgentRequest`, but it does **not** fill in
`status.token` — that is the job of **your** downstream fulfiller: the operator or
controller that watches provider-target resources and acts on them. The contract
is: krop creates the request, your system fulfils it and writes the status back,
krop propagates the result to the tenant.

In a demo (and in the e2e test) you simulate the fulfiller by patching the status
by hand. Note the provider child's **generated name** — many consumers' provider
children share the one provider workspace, so krop renames every provider-target
child collision-free to `<cluster>-<instance>-<originalName>-<hash>` (see
[blueprints.md](../blueprints.md#provider-child-naming-collision-free)). Address it
by listing, not by the template name:

```sh
# Find the renamed provider child (the template name eu-agent is NOT its real name)
AR=$(kubectl --context ${PROVIDER_WORKSPACE} -n default get agentrequests \
  -o jsonpath='{.items[0].metadata.name}')

# Simulate the downstream fulfiller writing the token to the STATUS subresource
kubectl --context ${PROVIDER_WORKSPACE} -n default patch agentrequest "$AR" \
  --subresource=status --type=merge -p '{"status":{"token":"tok-demo-42"}}'
```

> The CEL read still works across the rename: `${agentRequest.status.token}`
> resolves against the live renamed object, because you reference the node by its
> **`id`** (`agentRequest`), not its on-cluster name.

Now the consumer ConfigMap materializes with the propagated token, and the instance
status projection updates:

```sh
kubectl --context ${CONSUMER_WORKSPACE} -n default get configmap eu-cluster-config \
  -o jsonpath='{.data.token}{"\n"}'   # -> tok-demo-42

kubectl --context ${CONSUMER_WORKSPACE} -n default get kubernetescluster demo \
  -o jsonpath='agentToken={.status.agentToken}{"\n"}Ready={.status.conditions[?(@.type=="Ready")].status}{"\n"}'
# agentToken=tok-demo-42
# Ready=True
```

---

## Gotchas

- **The provider-child CRD must be served in the provider workspace before you
  create the blueprint.** kro type-checks `${agentRequest.status.token}` against
  the `AgentRequest` schema at graph-build time; if the CRD is not served, publish
  fails `Ready=False` reason `BuildFailed`. Apply
  [`test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml`](../../test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml)
  first.
- **Every provider-target GVK needs a least-privilege rule** in
  `config/kcp/rbac/provider-rbac.yaml` — the shipped rule only covers this
  example's `AgentRequest`. Never widen it to `*`/`*`. See
  [permissions.md](../permissions.md#c-kcp-provider-workspace--the-controllers-real-work).
- **The consumer must accept the claim** for the *consumer* child — here
  `configmaps`. The provider `AgentRequest` needs **no** claim (same ownership
  domain as the controller).
- **Instance stuck `Progressing` forever?** The provider child's status was never
  set — check your fulfiller. See the
  [troubleshooting guide](troubleshooting.md).

For the end-to-end run of exactly this flow (including binding and GC), see
[getting-started.md §4](../getting-started.md#4-consume-it).
