# Cross-target dependencies

A recipe for krop's signature pattern: a child on **one** plane that consumes
another node's live object via CEL, **across** the plane boundary. The shipped
[`HostedDatabase`](../blueprints.md#worked-example-annotated) blueprint threads two
such edges through a single graph:

- the **host**-target `db` Deployment reads `${vpc.status.vpcID}` from a
  **consumer**-target VPC `externalRef` (consumer read → host write), and
- the **consumer**-target `connection` ConfigMap reads `${db.metadata.name}` from
  the **host** Deployment (host → consumer).

This guide assumes you have done
[writing your first blueprint](writing-your-first-blueprint.md) (single consumer
child) and understand target routing. For the internals of *why* these pend and
converge, see
[architecture.md §4.2](../architecture.md#42-instance-reconcile-dual-target--cross-target-cel).

---

## The pattern

The relevant slice of
[`config/kcp/examples/blueprint-hosteddatabase.yaml`](../../config/kcp/examples/blueprint-hosteddatabase.yaml):

```yaml
apiVersion: krop.opendefense.cloud/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: hosteddatabase
spec:
  schema:
    apiVersion: v1alpha1
    kind: HostedDatabase
    group: krop.opendefense.cloud
    spec:
      name: string
      engine: string | default="postgres"
    status:
      endpoint: ${connection.data.endpoint}       # projection of the consumer child's data
      vpcID: ${vpc.status.vpcID}                   # projection of the external-ref's live status
  resources:
    - id: vpc                                       # CONSUMER-target external ref (read-only)
      target: consumer
      externalRef:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: VPC
        metadata:
          name: ${schema.spec.name}-vpc
    - id: db                                        # HOST-target child (physical host cluster)
      target: host
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          # prefixed with the consumer-cluster annotation → collision-free (below)
          name: ${schema.metadata.annotations["krop.opendefense.cloud/consumer-cluster"]}-${schema.spec.name}
          namespace: databases
        spec:
          replicas: 1
          selector: { matchLabels: { app: ${schema.spec.name} } }
          template:
            metadata: { labels: { app: ${schema.spec.name} } }
            spec:
              containers:
                - name: db
                  image: ${schema.spec.engine}:16
                  env:
                    - name: VPC_ID
                      value: ${vpc.status.vpcID}    # consumer read → host write
    - id: connection                                # CONSUMER-target child
      target: consumer
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.name}-connection
          namespace: default
        data:
          endpoint: ${db.metadata.name}.databases.svc.cluster.local   # host → consumer CEL
          vpcID: ${vpc.status.vpcID}
```

Each `${...}` reference creates a **dependency edge**: `db` depends on `vpc`, and
`connection` depends on both `db` and `vpc`. kro topologically orders the graph, so
krop reads the VPC first, then applies `db`, then `connection`.

---

## Pend-until-ready behavior

`${vpc.status.vpcID}` cannot resolve until the VPC exists **and something has
populated its `status.vpcID`**. krop does not fail on this — it **pends**:

1. krop reads the consumer VPC `externalRef` (Get through the virtual workspace
   under the read-only claim). NotFound, or `status.vpcID` still empty, ⇒
   `GetDesired` on `db` returns `ErrDataPending`.
2. The reconcile reports **not complete**, sets the instance
   `Ready=False`/`Progressing`, and requeues (~30s).
3. Crucially, an incomplete pass **suppresses prune** — any child that *was*
   applied is never mistaken for the full desired set and reclaimed.
4. Once `status.vpcID` is set, the next pass resolves the CEL, applies the host
   `db` Deployment, then materializes the consumer `connection` ConfigMap (which in
   turn only needs `${db.metadata.name}`, available as soon as `db` applies), and
   the instance flips to `Ready=True`.

So a child that depends on another plane's object simply **does not appear** until
that object (and the referenced field) exists. That is expected, not an error.

---

## What populates the referenced field?

krop only ever **reads** an `externalRef` — it never creates the VPC or writes its
status. That is the job of whatever owns the object: here, the tenant's own VPC
controller (e.g. the ACK EC2 controller) reconciling the VPC and writing
`status.vpcID`. The contract is: the external system provisions and reports status,
krop reads it and funnels it cross-plane.

The same pend-until-ready machinery applies when the upstream node is a
**provider-target `template`** child krop *writes* but whose `status` a **downstream
fulfiller** fills in: krop creates the object, your controller acts on it and writes
the status back, and krop propagates the result to the dependent child on the next
pass. Either way, the dependent child pends until the referenced field appears.

In a demo (and in the e2e test) you simulate the owner by patching the status by
hand. Note that a consumer `externalRef` keeps its literal `metadata.name`, so you
address it directly:

```sh
# Simulate the VPC controller writing status.vpcID to the STATUS subresource
kubectl --context ${CONSUMER_WORKSPACE} -n default patch vpc mydb-vpc \
  --subresource=status --type=merge -p '{"status":{"vpcID":"vpc-0abc123"}}'
```

Now `db` resolves and applies to the host cluster, and `connection` materializes
with the propagated endpoint. The instance status projection updates:

```sh
kubectl --context ${CONSUMER_WORKSPACE} -n default get configmap mydb-connection \
  -o jsonpath='{.data.endpoint}{"\n"}'   # -> <cluster>-mydb.databases.svc.cluster.local

kubectl --context ${CONSUMER_WORKSPACE} -n default get hosteddatabase demo \
  -o jsonpath='vpcID={.status.vpcID}{"\n"}Ready={.status.conditions[?(@.type=="Ready")].status}{"\n"}'
# vpcID=vpc-0abc123
# Ready=True
```

> The host `db` child is addressed cross-target by its **`id`**, not its on-cluster
> name: `${db.metadata.name}` resolves against the live object even though its name
> is prefixed with the consumer-cluster annotation (below).

---

## Collision-free naming across tenants

Many tenants' host children land in **one** host cluster, so a literal Deployment
name like `db` would collide. The example prefixes the host child's name with the
consumer's kcp logical-cluster name — globally unique + immutable — which the
reconciler stamps onto each instance as an annotation:

```yaml
name: ${schema.metadata.annotations["krop.opendefense.cloud/consumer-cluster"]}-${schema.spec.name}
```

Two tenants both creating a `HostedDatabase` named `db` yield `<clusterA>-db` and
`<clusterB>-db` in the shared `databases` namespace — no collision. (Provider-target
children are additionally renamed collision-free by krop automatically; host- and
consumer-target children keep their template name, which is why this prefix
matters.) See
[consumer workspace info in CEL](../blueprints.md#consumer-workspace-info-in-cel).

---

## Reading an input from another plane: `externalRef` → host write

The `vpc` → `db` edge above **is** krop's api-syncagent pattern: the input is not a
child krop writes but an existing object it only **reads**, and its observed status
funnels into a written child on another plane. The VPC is declared with
`externalRef` (instead of `template`), so it is read — never created or GC'd — and
survives instance deletion (krop does not own it).

Convergence is identical to any status flow: `db` depends on `vpc`, so krop reads
the VPC first; until it (and its `status.vpcID`) exists the Deployment **pends**,
and the VPC is never modified or deleted. For the field reference and the
read-only-claim / host-client details, see
[blueprints.md](../blueprints.md#externalref-reading-objects-krop-does-not-own) and
[permissions.md](../permissions.md#host-target-and-the-host-client).

---

## Gotchas

- **A foreign external-ref (or provider-target) type's export must be bound in the
  provider workspace** before you create the blueprint, so its `identityHash`
  resolves; otherwise publish fails `Ready=False` reason `ClaimIdentityUnresolved`.
  Core types (group `""`, e.g. `configmaps`) need no binding. kro also type-checks
  every `${...}` against the referenced schema at graph-build time — if a type is
  not served, publish fails `BuildFailed`.
- **Every provider-target GVK needs a least-privilege rule** in
  `config/kcp/rbac/provider-rbac.yaml`; the example's provider `registration`
  ConfigMap is a core type already covered by the required `configmaps` rule. Never
  widen it to `*`/`*`. See
  [permissions.md](../permissions.md#c-kcp-provider-workspace--the-controllers-real-work).
- **The consumer must accept the derived claims** for the *consumer* nodes — here a
  read-only `get,list,watch` claim for the VPC external ref and a full-CRUD
  `configmaps` claim for the `connection` ConfigMap. The host `db` and provider
  `registration` children are in the controller's own ownership domain and need
  **no** claim.
- **Instance stuck `Progressing` forever?** The referenced field was never set —
  check the object's owner (the VPC controller, or your provider-target fulfiller).
  See the [troubleshooting guide](troubleshooting.md).

For the end-to-end run of the provider→consumer flow (including binding and GC), see
[getting-started.md §4](../getting-started.md#4-consume-it).
