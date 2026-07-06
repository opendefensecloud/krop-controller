# M0 — kro-embedding spike findings

**Date:** 2026-07-06
**Status:** Complete. Retires the §6.1 / §16.3 kro-Builder-under-kcp risk.
**Companion:** `2026-07-06-krop-controller-design.md` (§1.6, §1.7, §2, §4), `idea.md` (§6.1, §12).
**Spike code:** `internal/spike/spike.go`, `internal/spike/spike_test.go` (THROWAWAY).

All claims below are backed by running code (`go test ./internal/spike/... -v`, output at the
bottom) or by direct source reads of kro **v0.9.2** in the module cache at
`$(go env GOMODCACHE)/github.com/kubernetes-sigs/kro@v0.9.2` (abbreviated `KRO/` below).

---

## TL;DR / recommendation

- **Q1 (client-free runtime): CONFIRMED, with a nuance.** `pkg/runtime`'s **direct** imports are
  client-free; the whole reconcile core (`FromGraph` + `Node.GetDesired/SetObserved/CheckReadiness/
  IsIgnored`, plus cross-resource and instance-status CEL) runs to completion with **zero cluster
  access** using fake observed data. The nuance: the *transitive* closure of `pkg/runtime` **does**
  pull in `client-go/rest` + `controller-runtime/apiutil` — because `pkg/runtime` imports
  `pkg/graph`, and `pkg/graph` also houses the `Builder`. This is a compile-time coupling only;
  nothing on the execution path calls a client. The design's "client-free runtime" claim is accurate
  as an *execution* claim, not a *transitive-dependency* claim.
- **Q2 (Builder schema resolution): the spec's §16.3 read is exactly right.** `graph.Builder`'s
  `schemaResolver` and `restMapper` are **unexported** interface fields; the only exported
  constructor `graph.NewBuilder(*rest.Config, *http.Client)` eagerly builds a **live**
  discovery/OpenAPI resolver + dynamic REST mapper from the config. There is **no** exported seam to
  inject a resolver.
- **Q3 (pure helpers): CONFIRMED.** `simpleschema.ToOpenAPISpec` and `crd.SynthesizeCRD` are
  importable and run with no client (one gotcha: `SynthesizeCRD` dereferences its `rgSchema` arg —
  pass a non-nil `*api/v1alpha1.Schema`, not `nil`).
- **§16.3 recommendation: go with "point `NewBuilder` at a schema-complete workspace endpoint"
  (option A) for M1–M4; keep the small upstream exported-constructor PR as a ready fallback.** The
  spike proves graph build is fully functional the moment a `SchemaResolver`/`RESTMapper` pair is
  available — the endpoint route supplies exactly that from a real `*rest.Config`. No fork is
  required to *ship*. A fork/PR is only required if we ever need to build a graph **without** a live
  discovery endpoint (e.g. purely from cached OpenAPI). See §16.3 section below.

---

## Deps: what resolved, what was pinned

`go get github.com/kubernetes-sigs/kro@v0.9.2` then `go mod tidy` resolved **cleanly with no version
conflicts and no manual `replace`/pin required.** Everything followed kro v0.9.2's own transitive
pins:

| module | resolved | design §12 target | task-prompt target | note |
|---|---|---|---|---|
| `github.com/kubernetes-sigs/kro` | `v0.9.2` | v0.9.2 | v0.9.2 | ✓ |
| `github.com/google/cel-go` | `v0.27.0` | v0.27 | v0.27 | ✓ |
| `k8s.io/{api,apimachinery,client-go,apiserver,apiextensions-apiserver}` | `v0.35.0` | **v0.35.x** | v0.36.x | kro v0.9.2 pins **v0.35.0**; spec §12 also says v0.35.x. The task prompt's "v0.36.x" is **inconsistent with kro v0.9.2's actual pins** — I did **not** force-bump, as that would risk breaking kro against an untested k8s minor. Flagging for a design-doc reconciliation. |
| `sigs.k8s.io/controller-runtime` | `v0.23.1` | (implied) | v0.24 | Followed kro's pin; not force-bumped for the same reason. Only pulled in transitively via `pkg/graph`'s Builder — not used by the runtime path. |

kcp SDK / `multicluster-provider` / `multicluster-runtime` are **not yet added** — M0 needs none of
them (no manager wiring until M1). They land in M1.

**Recommendation on the k8s/ctrl-runtime version gap:** keep kro's pins (v0.35.0 / v0.23.1) until M1
introduces the kcp SDK + multicluster-provider, then re-resolve the whole set together and update the
design doc's §12 table to whatever is mutually compatible. Bumping k8s in isolation now buys nothing
and risks a kro incompatibility.

---

## Q1 — client-free runtime (the load-bearing claim)

### Q1a — import audit
`pkg/runtime`'s **direct** imports (from `go list -f '{{.Imports}}'`, asserted in
`TestQ1_RuntimeIsClientFree`):

```
errors  fmt  maps  slices  strings  time
github.com/kubernetes-sigs/kro/pkg/cel
github.com/kubernetes-sigs/kro/pkg/cel/unstructured
github.com/kubernetes-sigs/kro/pkg/graph
github.com/kubernetes-sigs/kro/pkg/graph/variable
github.com/kubernetes-sigs/kro/pkg/metadata
github.com/kubernetes-sigs/kro/pkg/metrics
github.com/kubernetes-sigs/kro/pkg/runtime/resolver
k8s.io/apimachinery/pkg/apis/meta/v1/unstructured
k8s.io/apiserver/pkg/cel/openapi
k8s.io/kube-openapi/pkg/validation/spec
```

No `client-go/{dynamic,kubernetes,rest,discovery}`, no `controller-runtime`. Matches
`KRO/pkg/runtime/runtime.go` and `.../node.go` import blocks read directly.

**Transitive nuance (also asserted):** `pkg/graph` *does* import `k8s.io/client-go/rest` and
`sigs.k8s.io/controller-runtime/pkg/client/apiutil` (see `KRO/pkg/graph/builder.go:53-70`). Because
`pkg/runtime` imports `pkg/graph` for the `Graph`/`variable` types, `go list -deps` on `pkg/runtime`
transitively lists client-go. **This is compile-time only** — `runtime.FromGraph` takes an
already-built `*graph.Graph` (a plain data struct: `KRO/pkg/graph/graph.go:26`) and never constructs
or calls a Builder/client. If we ever want a hermetically client-free runtime binary, that would
require kro to split the `Builder` out of `pkg/graph` — **not needed for us**; we build graphs in the
Registrar (M4) where a client is expected anyway.

### Q1b — driving the reconcile core with no cluster
`TestQ1_DriveRuntimeNoCluster` builds a real `*graph.Graph` from an in-memory
`api/v1alpha1.ResourceGraphDefinition` (SimpleSchema `spec{name,cidr}`, two children: `vpc` reads
`${schema.spec.name}`, `subnet` reads `${vpc.status.vpcID}` → forces edge `vpc→subnet`), then drives
the runtime with **fake** observed data:

- Topological order comes out `[vpc subnet]` — dependency ordering works.
- `vpc.GetDesired()` resolves `metadata.name = "acme-vpc"` from the **instance object alone**
  (`${schema.spec.name}` = `"acme"`).
- `subnet.GetDesired()` **before** `vpc` is observed → returns `ErrDataPending`
  ("dependent node vpc not ready: no observed state"). Confirms the runtime gates on observed state
  (`KRO/pkg/runtime/node.go:216-231`).
- After `vpc.SetObserved(fakeVPC{status.vpcID:"vpc-0xDEADBEEF"})`:
  - `vpc.CheckReadiness()` passes from observed state alone (readyWhen `${vpc.status.vpcID != ''}`).
  - `subnet.GetDesired()` now resolves `spec.vpcID = "vpc-0xDEADBEEF"` — **cross-resource CEL
    resolved purely from `SetObserved`, no client.**
  - `instance.GetDesired()` projects `status.vpcID = "vpc-0xDEADBEEF"` — status aggregation is a pure
    CEL projection.
- `subnet.IsIgnored()` returns `(false, nil)` with no cluster.

This is the concrete proof behind design §2 / §7: **target selection and apply live in our code; the
kro core needs only in-memory desired/observed unstructured.** Dual-target apply needs no kro fork.

**How the graph was built with no cluster:** since the Builder fields are unexported (Q2), the spike
injects kro's own `pkg/testutil/k8s.NewFakeResolver()` (a public, non-`internal/` test helper that
ships pre-registered ACK EC2 VPC/Subnet schemas + a fake discovery — `KRO/pkg/testutil/k8s/
discovery.go:78,505`) into a `&graph.Builder{}` via **`unsafe` reflection**
(`spike.go:BuildGraphNoCluster` / `setUnexportedField`). The need for `unsafe` here from an external
module **is itself the Q2 finding** — see below.

---

## Q2 — Builder schema resolution (resolves §16.3 / idea.md §6.1)

Verified by source read + `TestQ2_BuilderCoupling` (reflection).

**`graph.Builder` struct** — `KRO/pkg/graph/builder.go:94-98`:
```go
type Builder struct {
	schemaResolver resolver.SchemaResolver // k8s.io/apiserver/pkg/cel/openapi/resolver
	restMapper     meta.RESTMapper         // k8s.io/apimachinery/pkg/api/meta
}
```
Both fields **unexported** (reflection confirms `PkgPath != ""` for each). No exported setter.

**Only exported constructor** — `KRO/pkg/graph/builder.go:53-72`:
```go
func NewBuilder(clientConfig *rest.Config, httpClient *http.Client) (*Builder, error) {
	schemaResolver, err := schemaresolver.NewCombinedResolver(clientConfig, httpClient) // live discovery+OpenAPI
	rm, err := apiutil.NewDynamicRESTMapper(clientConfig, httpClient)                    // live discovery
	return &Builder{schemaResolver: schemaResolver, restMapper: rm}, nil
}
```
`NewCombinedResolver` immediately calls `discovery.NewDiscoveryClientForConfigAndClient(config, …)`
(`KRO/pkg/graph/schema/resolver/resolver.go:31`) — it **dereferences the config** at construction, so
`NewBuilder(nil, nil)` *panics* (observed live). The REST mapper is a *deferred* discovery mapper
(lazy), but the resolver is not: **the config must serve discovery/OpenAPI.**

**Where the Builder uses them** — `KRO/pkg/graph/builder.go:418,423`, per child resource during
`NewResourceGraphDefinition`:
```go
resourceSchema, err := b.schemaResolver.ResolveSchema(gvk)          // OpenAPI for each child GVK
mapping, err := b.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version) // GVK → GVR/scope
```
So graph build type-checks CEL against **every child GVK's schema**, fetched from the endpoint. This
is **per-blueprint, not per-instance** (`NewResourceGraphDefinition` is called once per blueprint),
so the cost is amortized (design §1.6 / §6.1).

**Interfaces are satisfiable externally.** `resolver.SchemaResolver` and `meta.RESTMapper` are plain
public interfaces; a kcp-aware implementation *can* be written outside kro. The **only** blocker to
injecting one is the unexported fields — there is no `NewBuilderWithResolver(...)` seam. kro's own
tests construct `Builder{schemaResolver: fake, restMapper: fake}` **from inside package `graph`**
(`KRO/pkg/graph/builder_test.go:272`), which external code cannot do.

### §16.3 decision: point-at-endpoint (A) vs. upstream-constructor (B)

| | A — point `NewBuilder` at a schema-complete workspace endpoint | B — upstream an exported constructor |
|---|---|---|
| kro fork? | **No** | Yes (small PR; fields already interfaces) |
| needs live discovery/OpenAPI at build time | **Yes** — a `*rest.Config` for a workspace with all child CRDs/APIs bound | No |
| control over schema source | endpoint-defined | full (inject any resolver, incl. cached/offline) |
| fits kcp model | Registrar already runs *in* a provider workspace with a real `rest.Config` for it | same, but decoupled from live discovery |

**Recommendation: A for M1–M4.** The Registrar (M4) runs against a provider workspace that already
has a working `*rest.Config` and has (or can bind) the child APIs — which is exactly what
`NewBuilder` wants. Nothing about kcp's virtual-workspace/discovery surface is incompatible with
`NewCombinedResolver` (it's ordinary discovery + OpenAPI v3, which kcp workspaces serve). So we can
call `graph.NewBuilder(providerWorkspaceRestConfig, httpClient)` unchanged, no fork. **This must be
validated end-to-end against a real kcp workspace endpoint in M4** — the spike could not (no live kcp
here), so this remains the one residual assumption. If M4 discovers a kcp discovery/OpenAPI quirk
that `NewCombinedResolver` chokes on, fall back to **B**: a ~10-line upstream PR adding
`NewBuilderWithResolver(sr resolver.SchemaResolver, rm meta.RESTMapper) *Builder` (fields are already
interfaces), letting us feed a kcp-aware resolver. B is a clean, low-risk PR and worth opening
proactively so it's merged before we need it. Do **not** ship the `unsafe` injection used in the
spike — it is throwaway.

---

## Q3 — pure schema helpers for the Registrar (M4)

`TestQ3_PureSchemaHelpers`, both callable with no client:

- `simpleschema.ToOpenAPISpec(map[string]any{"name":"string","cidr":"string"}, nil)` →
  `*extv1.JSONSchemaProps` with `properties{name,cidr}` (`KRO/pkg/simpleschema/simpleschema.go:31`).
- `crd.SynthesizeCRD("example.com","v1alpha1","Network", *spec, extv1.JSONSchemaProps{}, false,
  extv1.NamespaceScoped, rgSchema)` → full CRD `networks.example.com`
  (`KRO/pkg/graph/crd/crd.go:31`).
  **Gotcha:** `SynthesizeCRD` dereferences its final `rgSchema *api/v1alpha1.Schema` arg
  (`.AdditionalPrinterColumns`, `.Metadata`) — passing `nil` panics. Pass a real
  `&v1alpha1.Schema{...}`. The Registrar will already have this object in hand.

Both are pure transforms — no discovery, no client. These are the pieces the M4 Registrar chains:
`SimpleSchema → ToOpenAPISpec → SynthesizeCRD → APIResourceSchema`.

---

## Test output (`go test ./internal/spike/... -v`)

```
=== RUN   TestQ1_RuntimeIsClientFree
    pkg/runtime direct imports (16) are client-free: [... no client-go/controller-runtime ...]
    transitive client-go enters via pkg/graph (Builder), not the runtime path
--- PASS: TestQ1_RuntimeIsClientFree
=== RUN   TestQ1_DriveRuntimeNoCluster
    graph built with no cluster; topological order = [vpc subnet]
    vpc desired name = "acme-vpc" (from ${schema.spec.name})
    subnet.GetDesired correctly pending pre-observation: ... waiting for readiness (data pending)
    vpc ready from observed status alone (no cluster)
    cross-resource CEL resolved: subnet.spec.vpcID = "vpc-0xDEADBEEF" (from vpc.SetObserved, NO client)
    instance status.vpcID projected = "vpc-0xDEADBEEF"
--- PASS: TestQ1_DriveRuntimeNoCluster
=== RUN   TestQ2_BuilderCoupling
    graph.Builder.schemaResolver type=resolver.SchemaResolver unexported=true
    graph.Builder.restMapper type=meta.RESTMapper unexported=true
    graph.NewBuilder signature: func(*rest.Config, *http.Client) (*graph.Builder, error)
--- PASS: TestQ2_BuilderCoupling
=== RUN   TestQ3_PureSchemaHelpers
    ToOpenAPISpec produced spec with props: [name cidr]
    SynthesizeCRD produced CRD "networks.example.com" (group=example.com, plural=networks)
--- PASS: TestQ3_PureSchemaHelpers
PASS
ok  	go.opendefense.cloud/krop-controller/internal/spike	0.126s
```

`go build ./...` and `go vet ./...` both clean (exit 0).

---

## Residual risks / follow-ups into M1+

1. **Endpoint route (A) not validated against live kcp.** The spike used kro's fake resolver; it did
   **not** prove `NewCombinedResolver` is happy against a real kcp workspace's discovery/OpenAPI v3.
   This is the single remaining piece of the §6.1 risk — validate first thing when M4 stands up a
   real provider workspace. Fallback B (exported constructor PR) is ready.
2. **Version table drift.** k8s v0.35.0 / controller-runtime v0.23.1 (kro's pins) vs. the task
   prompt's v0.36.x / v0.24. Reconcile the design §12 table once the kcp SDK + multicluster-provider
   are added in M1 and the full set is re-resolved together.
3. **`unsafe` injection is throwaway.** Only `internal/spike` uses it; delete the whole `internal/
   spike` tree once M1–M4 land the real Builder wiring.
