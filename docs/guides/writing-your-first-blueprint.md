# Writing your first blueprint

A from-scratch tutorial. You will build the **simplest possible** blueprint — one
that materializes a single consumer-target `ConfigMap` from an instance's
`spec.region` — publish it, bind a consumer, create an instance, and see the child
appear. Nothing pends, nothing crosses to the provider workspace: just the core
loop, explained piece by piece.

This is deliberately simpler than the worked
[`KubernetesCluster` example](../blueprints.md#worked-example-annotated). Once it
clicks, read the full [authoring reference](../blueprints.md) and then the
[cross-target dependencies guide](cross-target-dependencies.md).

**Prerequisites.** A running krop-controller pinned to a provider workspace, and a
consumer workspace — exactly the setup from
[getting-started.md](../getting-started.md) steps 1–2. This guide reuses the
placeholders `${PROVIDER_WORKSPACE}` and `${CONSUMER_WORKSPACE}` from there.

---

## 1. The blueprint, piece by piece

A blueprint is a `krop.opendefense.cloud/v1alpha1 ResourceGraphDefinition`. Save
this as `regionconfig-rgd.yaml`:

```yaml
apiVersion: krop.opendefense.cloud/v1alpha1   # krop's group…
kind: ResourceGraphDefinition                 # …but kro's RGD spec verbatim
metadata:
  name: regionconfig                           # blueprint name (cluster-scoped)
spec:
  schema:                                       # (1) the generated instance API
    apiVersion: v1alpha1
    kind: RegionConfig
    group: krop.opendefense.cloud
    spec:
      region: string                            # (2) the instance's input
    status:
      configName: ${config.metadata.name}       # (3) a CEL projection onto the instance status
  resources:
    - id: config                                # (4) one child node
      target: consumer                          # (6) route to the consumer workspace
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.region}-config    # (5) name derived from the instance input
          namespace: default
        data:
          region: ${schema.spec.region}          # (7) CEL: copy the instance input in
```

What each piece does:

1. **`spec.schema`** is a kro **SimpleSchema**. Its `group`/`apiVersion`/`kind`
   name the new API krop will generate and serve — here `RegionConfig` in group
   `krop.opendefense.cloud`, so the published APIExport will be
   `regionconfigs.krop.opendefense.cloud`.
2. **`spec.schema.spec`** is the instance's input surface. `region: string` is
   SimpleSchema shorthand (other forms: `integer`, `boolean`, `[]string`,
   `string | required=true`, `string | default=...`).
3. **`spec.schema.status`** is a set of `${...}` CEL projections krop writes back
   onto each instance's `.status`. `configName` will report the child's name.
4. **`spec.resources[]`** is the child graph. Each entry has an **`id`** (the CEL
   handle other nodes and the status use) and a **`template`** (a full manifest of
   the object to apply). We have exactly one child.
5. **`${schema.spec.*}`** reads the instance's own input. This makes the child's
   name a function of the instance — `spec.region: eu` → `eu-config`.
6. **`target: consumer`** is krop's per-resource routing field; it routes the
   child to the **consumer** workspace. (`consumer` is also the default when
   `target` is omitted — we set it explicitly here for clarity. The other values
   are `provider` and `host`.) It is a sibling of `template`, not a field inside
   it, so it never lands on the ConfigMap.
7. Any template field can be a `${...}` CEL expression.

That's the whole surface: a schema, one resource, a `target`, and a couple of
`${schema.spec.*}` reads. See [blueprints.md](../blueprints.md) for the full set of
fields (`target`, `externalRef`, `readyWhen`, `includeWhen`, `forEach`).

---

## 2. Publish it

Create the blueprint in the **provider** workspace:

```sh
kubectl --context ${PROVIDER_WORKSPACE} apply -f regionconfig-rgd.yaml
```

The Registrar compiles it into an `APIResourceSchema`, derives the
`permissionClaims` (here: one `configmaps` claim, because our only consumer-target
child is a core ConfigMap), server-side-applies the `APIExport`, and writes status.
Wait for `Ready`:

```sh
kubectl --context ${PROVIDER_WORKSPACE} get rgd regionconfig \
  -o jsonpath='{.status.exportedAPI}{"\t"}{.status.conditions[?(@.type=="Ready")].reason}{"\n"}'
# regionconfigs.krop.opendefense.cloud   Published

kubectl --context ${PROVIDER_WORKSPACE} get apiexport regionconfigs.krop.opendefense.cloud
```

If `Ready=False`, read `.status.conditions[?(@.type=="Ready")].message` and see the
[troubleshooting guide](troubleshooting.md).

---

## 3. Bind a consumer — accepting the claim

Because the child is consumer-target, krop writes it through the APIExport's
**virtual workspace**, and that write is authorized only if the consumer's
`APIBinding` **accepts** the auto-derived `configmaps` claim. Save as
`regionconfig-binding.yaml`:

```yaml
apiVersion: apis.kcp.io/v1alpha2
kind: APIBinding
metadata:
  name: regionconfigs
spec:
  reference:
    export:
      path: ${PROVIDER_WORKSPACE}                  # e.g. root:krop-provider
      name: regionconfigs.krop.opendefense.cloud
  permissionClaims:
    - group: ""
      resource: configmaps
      verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
      selector:
        matchAll: true
      state: Accepted        # <-- acceptance authorizes the cross-workspace write
```

```sh
kubectl --context ${CONSUMER_WORKSPACE} apply -f regionconfig-binding.yaml

kubectl --context ${CONSUMER_WORKSPACE} get apibinding regionconfigs \
  -o jsonpath='{.status.phase}{"\n"}'   # -> Bound
```

> If you set the claim to `Rejected` (or omit it), the binding still reaches
> `Bound`, but the ConfigMap write is denied and the child never appears — this is
> the intended least-privilege behavior.

---

## 4. Create an instance and see the child

```sh
cat <<'EOF' | kubectl --context ${CONSUMER_WORKSPACE} apply -f -
apiVersion: krop.opendefense.cloud/v1alpha1
kind: RegionConfig
metadata:
  name: demo
  namespace: default
spec:
  region: eu
EOF
```

The child materializes in the **consumer** workspace, and the instance status
carries the projection:

```sh
kubectl --context ${CONSUMER_WORKSPACE} -n default get configmap eu-config \
  -o jsonpath='{.data.region}{"\n"}'   # -> eu

kubectl --context ${CONSUMER_WORKSPACE} -n default get regionconfig demo \
  -o jsonpath='status.configName={.status.configName}{"\n"}Ready={.status.conditions[?(@.type=="Ready")].status}{"\n"}'
# status.configName=eu-config
# Ready=True
```

`status.configName` is the `schema.status` CEL projection; `Ready=True` is the
condition the reconciler writes once every child passed readiness (this single
ConfigMap has no `readyWhen`, so it is ready as soon as it applies).

---

## 5. Clean up

Deleting the instance runs its finalizer, which garbage-collects the child:

```sh
kubectl --context ${CONSUMER_WORKSPACE} -n default delete regionconfig demo
kubectl --context ${CONSUMER_WORKSPACE} -n default get configmap eu-config   # -> NotFound
```

---

## Where next

- Add a **second child** and route it to the provider workspace, then feed its
  status into this one: [cross-target dependencies](cross-target-dependencies.md).
- The full authoring surface — `readyWhen`/`includeWhen`/`forEach`, provider-child
  naming, pruning, schema evolution: [blueprints.md](../blueprints.md).
- How publication, dual-target apply, and GC actually work:
  [architecture.md](../architecture.md).
