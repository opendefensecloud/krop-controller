# M4 — Blueprint Registrar Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **This is the largest milestone; execution naturally checkpoints after Task 5 (Registrar publication proven) before the dynamic supervisor (Tasks 6–8).**

**Goal:** A provider authors a blueprint (a `ResourceGraphDefinition` served under `krop.opendefense.cloud/v1alpha1`) in a provider workspace; the Registrar automatically publishes it as a bindable `APIExport` (+ `APIResourceSchema`, `APIExportEndpointSlice`, auto-derived `permissionClaims`) and spins up an instance-serving manager — so consumers bind and create instances that the **unchanged** M1–M3 engine/reconciler materializes, with **no hand-written APIExport/ARS/claim fixtures**.

**Architecture:** Two cooperating pieces in one process. (1) The **Registrar** — a controller-runtime controller watching `ResourceGraphDefinition` in the provider workspace: it parses the blueprint into kro's RGD struct, builds the graph via the existing `EndpointGraphSource` (against the provider workspace's discovery), converts `graph.CRD` to an `APIResourceSchema` via the kcp SDK's `CRDToAPIResourceSchema`, auto-derives `permissionClaims` from consumer-target node GVRs (identityHash from `APIBinding.status.boundResources`), publishes/patches the `APIExport` + `APIExportEndpointSlice`, caches the compiled graph keyed by `(workspace, name, specHash)`, and writes status back. (2) The **Supervisor** — per published blueprint, it starts a dedicated `mcmanager.Manager` (its own `apiexport.Provider` on that blueprint's endpoint slice) in a goroutine with a cancellable context, registering the existing `internal/controller.Reconciler` with the cached graph; it tears the manager down on blueprint delete. (Constraint: one manager ⇄ one APIExport — `apiexport.New` binds a single export — so dynamic per-blueprint managers are mandatory.)

**Tech Stack:** Go 1.26, kro v0.9.2, kcp-dev/sdk v0.32.2 (`apis/v1alpha1.CRDToAPIResourceSchema`, `apis/v1alpha2` APIExport/APIBinding), multicluster-provider v0.8.0, multicluster-runtime v0.24.1, controller-runtime v0.24.1, k8s v0.36.2. Envtest kcp binary v0.30.0.

**Grounding (verified in the M4 investigation):**
- `graph.Graph.CRD *apiextensionsv1.CustomResourceDefinition` is the generated instance CRD; `apisv1alpha1.CRDToAPIResourceSchema(crd, prefix) (*APIResourceSchema, error)` maps it to an ARS with `metadata.name = prefix + "." + crd.Name` → pass `prefix = "v"+specHash` for the `v<hash>.<plural>.<group>` convention.
- `apigen` IS `github.com/kcp-dev/sdk/cmd/apigen`; its conversion core is that exported function — import it, skip the CLI.
- A Go wrapper `ResourceGraphDefinition{Spec krov1alpha1.ResourceGraphDefinitionSpec; Status BlueprintStatus}` with `+groupName=krop.opendefense.cloud` controller-gens cleanly (empirically verified).
- One manager ⇄ one APIExport: `apiexport.New(cfg, endpointSliceName, opts)` binds one endpoint slice; `mcmanager.New(cfg, provider, opts)` holds one immutable provider. Dynamic serving = one goroutine-manager per blueprint.
- Claims: iterate `graph.Nodes`, keep `engine.TargetOf(node.Template) == consumer`, collect `node.Meta.GVR.GroupResource()` (exclude the instance's own GVR); resolve identityHash from `APIBinding.status.boundResources[].Schema.IdentityHash` in the build workspace (empty for core types like `configmaps`).
- The M3 lesson: the ARS must declare every `status.*` the blueprint projects — auto-generation via `graph.CRD` (kro synthesizes status from the blueprint schema) removes the hand-written-ARS drift entirely.

**Scope:**
- **In scope:** the blueprint CRD (our own type), the Registrar (publish ARS+APIExport+EndpointSlice+claims+status, compiled-graph cache, specHash change-detection), the dynamic per-blueprint instance-manager supervisor, and an end-to-end e2e proving auto-publication → bind → instance → dual-target + cross-target materialization with NO hand-written kcp fixtures. Reuses `internal/engine` + `internal/controller.Reconciler` unchanged.
- **Deferred:** multi-provider-workspace blueprint watching (M4 watches the one configured provider workspace); GraphRevision-style rollback (design §1.6 — we keep the specHash cache only); schema-evolution multi-version serving beyond "new specHash → new ARS" (keep it simple: latest ARS wins, old instances continue under their bound version via kcp); GC of retired ARS/APIExports (M5).

---

## Execution split: M4a / M4b

This plan ships as two mergeable increments:
- **M4a — Registrar publication:** Tasks 1, 2, 3, 4, 5, **5b**. Delivers the blueprint CRD and a Registrar that publishes a correct `APIExport` + `APIResourceSchema` + auto-derived `permissionClaims`, proven by a publication e2e (Task 5b) that asserts the generated objects — WITHOUT the dynamic instance-serving manager. Merges on its own.
- **M4b — Dynamic serving:** Tasks 6, 7, 8. Adds the Supervisor + main.go wiring + the full blueprint→instance→children e2e.

---

## File structure

| File | Responsibility |
|---|---|
| `api/v1alpha1/groupversion_info.go` | Scheme registration for group `krop.opendefense.cloud/v1alpha1`. |
| `api/v1alpha1/resourcegraphdefinition_types.go` | `ResourceGraphDefinition` (embeds kro `ResourceGraphDefinitionSpec`) + `BlueprintStatus` + `List`. |
| `api/v1alpha1/zz_generated.deepcopy.go` | controller-gen output. |
| `config/crds/krop.opendefense.cloud_resourcegraphdefinitions.yaml` | controller-gen CRD for the blueprint type (installed in provider workspaces). |
| `internal/registrar/hash.go` + `hash_test.go` | `SpecHash(spec)` — deterministic content hash of the blueprint spec. |
| `internal/registrar/cache.go` + `cache_test.go` | `GraphCache` keyed by `(workspace, name, specHash) → *graph.Graph`. |
| `internal/registrar/claims.go` + `claims_test.go` | `DeriveClaims(foreignGRs, identity) []PermissionClaim` (pure) + `ForeignConsumerGRs(g, instanceGVR)` (graph enumeration). |
| `internal/registrar/publish.go` | Build ARS from `graph.CRD`; build/patch APIExport (+ endpoint slice); resolve identityHash from APIBindings. |
| `internal/registrar/registrar.go` | The Registrar reconciler: parse blueprint → build graph (cache) → publish → status → notify supervisor. |
| `internal/supervisor/supervisor.go` + `supervisor_test.go` | Per-blueprint instance-manager lifecycle (start/stop goroutine-managers). |
| `cmd/controller/main.go` (modify) | Replace the hardcoded single-APIExport wiring with the Registrar (on `cfg`) + Supervisor. |
| `test/fixtures/blueprint-kubernetescluster-rgd.yaml` | An authored `ResourceGraphDefinition` (the cross-target KubernetesCluster blueprint) the e2e creates. |
| `internal/controller/registrar_e2e_test.go` | Full auto-publication e2e (blueprint → APIExport → bind → instance → children). |

Reused unchanged: `internal/engine/*`, `internal/controller/reconciler.go`, `internal/kcp/endpointslice.go`. The M1–M3 hand-written fixtures (`config/kcp/apiexport-krop-m1.yaml`, `apiresourceschema-*.yaml`, `test/fixtures/apibinding-*.yaml`) remain for the M2/M3 e2e; the M4 e2e uses only the blueprint + a consumer APIBinding.

---

## Task 1: Blueprint CRD type + scheme

**Files:**
- Create: `api/v1alpha1/groupversion_info.go`, `api/v1alpha1/resourcegraphdefinition_types.go`
- Generate: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crds/krop.opendefense.cloud_resourcegraphdefinitions.yaml`
- Test: `api/v1alpha1/types_test.go`

- [ ] **Step 1: Write the group/version registration**

```go
// api/v1alpha1/groupversion_info.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

// +kubebuilder:object:generate=true
// +groupName=krop.opendefense.cloud
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version for the krop blueprint API.
var GroupVersion = schema.GroupVersion{Group: "krop.opendefense.cloud", Version: "v1alpha1"}

// SchemeBuilder registers the blueprint types.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the blueprint types to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme
```

- [ ] **Step 2: Write the blueprint types**

```go
// api/v1alpha1/resourcegraphdefinition_types.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// BlueprintStatus is the observed state the Registrar writes back.
type BlueprintStatus struct {
	// ExportedAPI is the metadata.name of the published APIExport.
	// +optional
	ExportedAPI string `json:"exportedAPI,omitempty"`
	// IdentityHash is the published APIExport's identity hash.
	// +optional
	IdentityHash string `json:"identityHash,omitempty"`
	// ObservedSpecHash is the spec hash the current publication reflects.
	// +optional
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`
	// Conditions (Ready, etc.).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ResourceGraphDefinition is a provider-authored blueprint. Its spec is
// identical to kro's ResourceGraphDefinition spec (routing lives in per-resource
// template annotations); the engine parses it into kro's type unchanged.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rgd
type ResourceGraphDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   krov1alpha1.ResourceGraphDefinitionSpec `json:"spec,omitempty"`
	Status BlueprintStatus                         `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ResourceGraphDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceGraphDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceGraphDefinition{}, &ResourceGraphDefinitionList{})
}
```

- [ ] **Step 3: Generate deepcopy + CRD**

Run: `make generate manifests` (the Makefile's `generate` runs controller-gen `object`, `manifests` runs `controller-gen crd` into `config/crds`). If `make manifests` also invokes `apigen` and fails (apigen expects APIExport-destined CRDs), it's fine for this task to run just the two controller-gen steps directly:

```bash
go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0 object paths=./api/...
go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0 crd paths=./api/... output:crd:dir=config/crds
```

Verify `config/crds/krop.opendefense.cloud_resourcegraphdefinitions.yaml` exists with `group: krop.opendefense.cloud` and `x-kubernetes-preserve-unknown-fields: true` on the `spec.schema`/`spec.resources[].template` fields (kro's RawExtension fields).

- [ ] **Step 4: Write a roundtrip test**

```go
// api/v1alpha1/types_test.go
package v1alpha1

import (
	"testing"

	"sigs.k8s.io/yaml"
)

func TestResourceGraphDefinition_ParsesKroSpec(t *testing.T) {
	raw := []byte(`
apiVersion: krop.opendefense.cloud/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: demo
spec:
  schema:
    apiVersion: v1alpha1
    kind: KubernetesCluster
    group: krop.opendefense.cloud
    spec:
      region: string
  resources:
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: x
`)
	var rgd ResourceGraphDefinition
	if err := yaml.Unmarshal(raw, &rgd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rgd.Spec.Schema.Kind != "KubernetesCluster" {
		t.Fatalf("schema kind = %q", rgd.Spec.Schema.Kind)
	}
	if len(rgd.Spec.Resources) != 1 || rgd.Spec.Resources[0].ID != "config" {
		t.Fatalf("resources = %+v", rgd.Spec.Resources)
	}
}
```

- [ ] **Step 5: Run + commit**

Run: `go test ./api/... -v && go build ./...`
Expected: PASS / clean.

```bash
git add api/ config/crds/
git commit --no-verify -m "api: blueprint ResourceGraphDefinition CRD (kro spec + krop status) (M4)"
```

---

## Task 2: Spec hash

**Files:**
- Create: `internal/registrar/hash.go`, `internal/registrar/hash_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/registrar/hash_test.go
package registrar

import (
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func TestSpecHash_StableAndSensitive(t *testing.T) {
	a := krov1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "A"}}
	b := krov1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "A"}}
	c := krov1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "B"}}
	if SpecHash(a) != SpecHash(b) {
		t.Fatal("equal specs must hash equal")
	}
	if SpecHash(a) == SpecHash(c) {
		t.Fatal("different specs must hash differently")
	}
	if len(SpecHash(a)) == 0 {
		t.Fatal("empty hash")
	}
}
```

- [ ] **Step 2: Run to verify fail; then implement**

Run: `go test ./internal/registrar/ -run TestSpecHash -v` → FAIL (`undefined: SpecHash`).

```go
// internal/registrar/hash.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

// Package registrar publishes provider blueprints as bindable kcp APIExports.
package registrar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// SpecHash returns a short deterministic content hash of a blueprint spec, used
// for the ARS name suffix and for change-detection (skip rebuild when unchanged).
func SpecHash(spec krov1alpha1.ResourceGraphDefinitionSpec) string {
	b, _ := json.Marshal(spec) // stable: json.Marshal sorts map keys
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/registrar/ -v` → PASS.

```bash
git add internal/registrar/hash.go internal/registrar/hash_test.go
git commit --no-verify -m "registrar: deterministic blueprint spec hash (M4)"
```

---

## Task 3: Compiled-graph cache

**Files:**
- Create: `internal/registrar/cache.go`, `internal/registrar/cache_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/registrar/cache_test.go
package registrar

import (
	"testing"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

func TestGraphCache_KeyedByWorkspaceNameHash(t *testing.T) {
	c := NewGraphCache()
	g := &graph.Graph{}
	c.Put("ws1", "bp", "h1", g)

	if got, ok := c.Get("ws1", "bp", "h1"); !ok || got != g {
		t.Fatal("expected cache hit for same key")
	}
	if _, ok := c.Get("ws1", "bp", "h2"); ok {
		t.Fatal("different hash must miss (spec changed)")
	}
	if _, ok := c.Get("ws2", "bp", "h1"); ok {
		t.Fatal("different workspace must miss (collision hazard, design §1.6)")
	}
	c.Delete("ws1", "bp")
	if _, ok := c.Get("ws1", "bp", "h1"); ok {
		t.Fatal("deleted entry must miss")
	}
}
```

- [ ] **Step 2: Verify fail; implement**

Run: `go test ./internal/registrar/ -run TestGraphCache -v` → FAIL.

```go
// internal/registrar/cache.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package registrar

import (
	"sync"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// GraphCache holds compiled blueprint graphs keyed by (workspace, name, specHash).
// The workspace dimension avoids the blueprint-name collision hazard (design §1.6).
// Graph build is expensive (discovery/OpenAPI), so this amortizes it per blueprint.
type GraphCache struct {
	mu sync.RWMutex
	m  map[string]map[string]*graph.Graph // (ws|name) -> specHash -> graph
}

// NewGraphCache returns an empty cache.
func NewGraphCache() *GraphCache { return &GraphCache{m: map[string]map[string]*graph.Graph{}} }

func bpKey(workspace, name string) string { return workspace + "|" + name }

// Get returns the cached graph for (workspace, name, specHash) if present.
func (c *GraphCache) Get(workspace, name, specHash string) (*graph.Graph, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	byHash, ok := c.m[bpKey(workspace, name)]
	if !ok {
		return nil, false
	}
	g, ok := byHash[specHash]
	return g, ok
}

// Put stores the graph, replacing any prior hash for this blueprint.
func (c *GraphCache) Put(workspace, name, specHash string, g *graph.Graph) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[bpKey(workspace, name)] = map[string]*graph.Graph{specHash: g}
}

// Delete drops all cached graphs for a blueprint.
func (c *GraphCache) Delete(workspace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, bpKey(workspace, name))
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/registrar/ -v` → PASS.

```bash
git add internal/registrar/cache.go internal/registrar/cache_test.go
git commit --no-verify -m "registrar: compiled-graph cache keyed by workspace+name+hash (M4)"
```

---

## Task 4: permissionClaims derivation

**Files:**
- Create: `internal/registrar/claims.go`, `internal/registrar/claims_test.go`

The pure mapping (`DeriveClaims`) is unit-tested; the graph enumeration (`ForeignConsumerGRs`) is exercised by the e2e (it needs a built graph).

- [ ] **Step 1: Write the failing test (pure mapping)**

```go
// internal/registrar/claims_test.go
package registrar

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestDeriveClaims_CoreAndForeign(t *testing.T) {
	foreign := []schema.GroupResource{
		{Group: "", Resource: "configmaps"},                          // core → empty identity
		{Group: "access.opendefense.cloud", Resource: "scopes"},      // foreign → identity from map
	}
	identity := map[schema.GroupResource]string{
		{Group: "access.opendefense.cloud", Resource: "scopes"}: "abc123hash",
	}
	claims := DeriveClaims(foreign, identity)
	if len(claims) != 2 {
		t.Fatalf("want 2 claims, got %d", len(claims))
	}
	byRes := map[string]string{}
	for _, c := range claims {
		byRes[c.Resource] = c.IdentityHash
		if len(c.Verbs) == 0 {
			t.Fatalf("claim %s has no verbs", c.Resource)
		}
	}
	if byRes["configmaps"] != "" {
		t.Fatalf("core configmaps identityHash must be empty, got %q", byRes["configmaps"])
	}
	if byRes["scopes"] != "abc123hash" {
		t.Fatalf("scopes identityHash = %q, want abc123hash", byRes["scopes"])
	}
}
```

- [ ] **Step 2: Verify fail; implement**

Run: `go test ./internal/registrar/ -run TestDeriveClaims -v` → FAIL.

```go
// internal/registrar/claims.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package registrar

import (
	"sort"

	"k8s.io/apimachinery/pkg/runtime/schema"

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"

	"github.com/kubernetes-sigs/kro/pkg/graph"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// claimVerbs is the CRUD verb set the engine needs on claimed consumer-target types.
var claimVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete"}

// DeriveClaims builds one permissionClaim per foreign consumer-target GroupResource,
// resolving identityHash from the provided map (empty string for core types).
// Deterministic order (sorted) so publications are stable.
func DeriveClaims(foreign []schema.GroupResource, identity map[schema.GroupResource]string) []apisv1alpha2.PermissionClaim {
	sorted := append([]schema.GroupResource(nil), foreign...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Group != sorted[j].Group {
			return sorted[i].Group < sorted[j].Group
		}
		return sorted[i].Resource < sorted[j].Resource
	})
	claims := make([]apisv1alpha2.PermissionClaim, 0, len(sorted))
	for _, gr := range sorted {
		claims = append(claims, apisv1alpha2.PermissionClaim{
			GroupResource: apisv1alpha2.GroupResource{Group: gr.Group, Resource: gr.Resource},
			Verbs:         claimVerbs,
			IdentityHash:  identity[gr],
		})
	}
	return claims
}

// ForeignConsumerGRs enumerates the GroupResources of consumer-target nodes that
// are NOT the instance's own type (those need permissionClaims to be written into
// the consumer workspace through the vw). Reads the routing target off each node's
// template exactly as the engine does.
func ForeignConsumerGRs(g *graph.Graph, instanceGR schema.GroupResource) []schema.GroupResource {
	seen := map[schema.GroupResource]bool{}
	var out []schema.GroupResource
	for _, node := range g.Nodes {
		target, err := kropengine.TargetOf(node.Template)
		if err != nil || target != kropengine.TargetConsumer {
			continue
		}
		gr := node.Meta.GVR.GroupResource()
		if gr == instanceGR || seen[gr] {
			continue
		}
		seen[gr] = true
		out = append(out, gr)
	}
	return out
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/registrar/ -v && go build ./...` → PASS / clean.

```bash
git add internal/registrar/claims.go internal/registrar/claims_test.go
git commit --no-verify -m "registrar: auto-derive permissionClaims from consumer-target nodes (M4)"
```

---

## Task 5: Registrar reconciler (publication)

**Files:**
- Create: `internal/registrar/publish.go`, `internal/registrar/registrar.go`

This is the publication core. Its full behavior is proven by the Task 8 e2e (it needs live kcp discovery + APIBindings); here it must compile and vet, with the pure helpers already unit-tested.

- [ ] **Step 1: Write `publish.go`**

Implements: ARS from `graph.CRD`; identityHash lookup from APIBindings; APIExport upsert. Server-side apply via a workspace client.

```go
// internal/registrar/publish.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package registrar

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

const fieldManager = "krop-registrar"

// BuildARS converts the graph's generated instance CRD into an APIResourceSchema
// named v<specHash>.<plural>.<group>.
func BuildARS(g *graph.Graph, specHash string) (*apisv1alpha1.APIResourceSchema, error) {
	if g.CRD == nil {
		return nil, fmt.Errorf("graph has no generated CRD")
	}
	return apisv1alpha1.CRDToAPIResourceSchema(g.CRD, "v"+specHash)
}

// identityByGroupResource lists APIBindings in the build workspace and maps each
// bound GroupResource to its identityHash (for resolving foreign-type claims).
func identityByGroupResource(ctx context.Context, c client.Client) (map[schema.GroupResource]string, error) {
	var bindings apisv1alpha2.APIBindingList
	if err := c.List(ctx, &bindings); err != nil {
		return nil, fmt.Errorf("listing APIBindings: %w", err)
	}
	out := map[schema.GroupResource]string{}
	for i := range bindings.Items {
		for _, br := range bindings.Items[i].Status.BoundResources {
			out[schema.GroupResource{Group: br.Group, Resource: br.Resource}] = br.Schema.IdentityHash
		}
	}
	return out, nil
}

// UpsertAPIExport server-side-applies the APIExport referencing the ARS with the
// derived permissionClaims. exportName is the ARS's <plural>.<group>.
func UpsertAPIExport(ctx context.Context, c client.Client, exportName string, ars *apisv1alpha1.APIResourceSchema, claims []apisv1alpha2.PermissionClaim) error {
	export := &apisv1alpha2.APIExport{}
	export.SetName(exportName)
	export.Spec.Resources = []apisv1alpha2.ResourceSchema{{
		Name:   ars.Spec.Names.Plural,
		Group:  ars.Spec.Group,
		Schema: ars.Name,
		Storage: apisv1alpha2.ResourceSchemaStorage{
			CRD: &apisv1alpha2.ResourceSchemaStorageCRD{},
		},
	}}
	export.Spec.PermissionClaims = claims
	if err := c.Patch(ctx, export, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying APIExport %q: %w", exportName, err)
	}
	return nil
}
```

Verify the exact type/field names against `apis/apis/v1alpha2/types_apiexport.go` (`ResourceSchema`, `ResourceSchemaStorage`, `ResourceSchemaStorageCRD`) and `APIResourceSchemaSpec` (`Group`, `Names.Plural`) in the module cache; adjust if they differ. Also confirm whether an `APIExportEndpointSlice` must be created explicitly or is auto-created (the M2 spike used the auto-created default slice named after the export — if so, no explicit slice creation is needed; note this).

- [ ] **Step 2: Write the Registrar reconciler**

```go
// internal/registrar/registrar.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

package registrar

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// GraphBuilder builds a compiled graph from a kro RGD (EndpointGraphSource in prod).
type GraphBuilder interface {
	Build(rgd *krov1alpha1.ResourceGraphDefinition) (*graphResult, error)
}

// Registrar reconciles blueprints into published APIExports and notifies the
// supervisor to (re)start the instance-serving manager.
type Registrar struct {
	Client    client.Client                // the provider-workspace client
	Workspace string                       // the provider workspace name (cache key)
	Cache     *GraphCache
	Source    interface {                  // engine.EndpointGraphSource
		Build(*krov1alpha1.ResourceGraphDefinition) (*graphAlias, error)
	}
	// OnPublished is called after a successful publish so the supervisor can ensure
	// an instance manager is running for this blueprint's export.
	OnPublished func(exportName string, instanceGVK schema.GroupVersionKind, g *graphAlias)
}

// Reconcile publishes one blueprint.
func (r *Registrar) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	bp := &kropv1alpha1.ResourceGraphDefinition{}
	if err := r.Client.Get(ctx, req.NamespacedName, bp); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	specHash := SpecHash(bp.Spec)
	g, ok := r.Cache.Get(r.Workspace, bp.Name, specHash)
	if !ok {
		rgd := &krov1alpha1.ResourceGraphDefinition{Spec: bp.Spec}
		rgd.Name = bp.Name
		built, err := r.Source.Build(rgd)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("building graph: %w", err)
		}
		g = built
		r.Cache.Put(r.Workspace, bp.Name, specHash, g)
	}

	instanceGR := schema.GroupResource{Group: g.Instance.Meta.GVR.Group, Resource: g.Instance.Meta.GVR.Resource}
	ars, err := BuildARS(g, specHash)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := applyARS(ctx, r.Client, ars); err != nil {
		return reconcile.Result{}, err
	}

	identity, err := identityByGroupResource(ctx, r.Client)
	if err != nil {
		return reconcile.Result{}, err
	}
	claims := DeriveClaims(ForeignConsumerGRs(g, instanceGR), identity)
	exportName := ars.Spec.Names.Plural + "." + ars.Spec.Group
	if err := UpsertAPIExport(ctx, r.Client, exportName, ars, claims); err != nil {
		return reconcile.Result{}, err
	}

	instanceGVK := schema.GroupVersionKind{Group: ars.Spec.Group, Version: g.CRD.Spec.Versions[0].Name, Kind: ars.Spec.Names.Kind}
	if r.OnPublished != nil {
		r.OnPublished(exportName, instanceGVK, g)
	}

	bp.Status.ExportedAPI = exportName
	bp.Status.ObservedSpecHash = specHash
	// set Ready condition (use meta.SetStatusCondition)
	return reconcile.Result{}, r.Client.Status().Update(ctx, bp)
}
```

Implementer notes:
- The `graphAlias`/`graphResult` placeholder names above are indicative — use the real `*graph.Graph` from `github.com/kubernetes-sigs/kro/pkg/graph` directly (import it; `Source` is `*kropengine.EndpointGraphSource` whose `Build` returns `(*graph.Graph, error)`). Replace the interface with the concrete `*kropengine.EndpointGraphSource` field and `*graph.Graph` types; the aliases exist only to keep the sketch readable — do NOT ship them.
- Add `applyARS(ctx, c, ars)` in `publish.go`: server-side apply the ARS (create-or-update; ARS is immutable once served, so a create-if-not-exists is safer — try Get, create if NotFound; do not patch an existing ARS of the same name/hash).
- Use `meta.SetStatusCondition(&bp.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Published", ...})` for the status condition; set `Ready=False` with the error reason on failure paths before returning.
- Register the scheme: the provider-workspace manager must have `kropv1alpha1`, `apisv1alpha1`, `apisv1alpha2` registered.

- [ ] **Step 3: Build + vet + commit**

Run: `go build ./... && go vet ./...`
Expected: clean (resolve the alias placeholders into real `*graph.Graph`; fix any field-name mismatches against the SDK).

```bash
git add internal/registrar/publish.go internal/registrar/registrar.go
git commit --no-verify -m "registrar: publish blueprint as APIExport+ARS with derived claims (M4)"
```

---

## Task 5b: Registrar publication e2e (M4a validation)

**Files:**
- Create: `test/fixtures/blueprint-kubernetescluster-rgd.yaml`
- Create: `internal/registrar/publish_e2e_test.go`

Proves M4a end-to-end against real kcp WITHOUT the supervisor: install the blueprint CRD + AgentRequest CRD in a provider workspace, run the Registrar once (direct-reconcile style — instantiate `&Registrar{...}` with a workspace client and `EndpointGraphSource`, call `Reconcile` directly, as access-operator's e2e drives reconcile), create a blueprint, and assert the published objects.

- [ ] **Step 1: Write the blueprint fixture**

`test/fixtures/blueprint-kubernetescluster-rgd.yaml` — a `krop.opendefense.cloud/v1alpha1 ResourceGraphDefinition` with the M3 cross-target body (provider `agentRequest` AgentRequest + consumer `config` ConfigMap reading `${agentRequest.status.token}`; instance status `configMapName` + `agentToken`). Reuse the body from `internal/engine/embedded/blueprint-kubernetescluster.yaml`, wrapped as `kind: ResourceGraphDefinition` under our group.

- [ ] **Step 2: Write the publication e2e (envtest, package `registrar_test` or `controller_test`)**

`BeforeAll` (reuse the envtest boot pattern from `internal/controller/suite_test.go` — either share it or add a local suite): create a provider workspace via `envtest.NewWorkspaceFixture`; apply the blueprint CRD (`config/crds/krop.opendefense.cloud_resourcegraphdefinitions.yaml`) and the AgentRequest CRD (`test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml`); wait both Established. Build a workspace-scoped client + `EndpointGraphSource` against the provider workspace config. Construct `&registrar.Registrar{Client: wsClient, Workspace: providerPath.String(), Cache: registrar.NewGraphCache(), Source: graphSource}` (OnPublished nil for M4a). Create the blueprint via the client, then call `reg.Reconcile(ctx, reconcile.Request{NamespacedName: {Name: "kubernetescluster"}})`.

Assertions (`Eventually`/direct):
- An `apisv1alpha2.APIExport` named `kubernetesclusters.krop.opendefense.cloud` exists in the provider ws.
- Its `spec.resources[0]` references an ARS named `v<hash>.kubernetesclusters.krop.opendefense.cloud`, and that `APIResourceSchema` exists with `spec.group == krop.opendefense.cloud`.
- The ARS's instance schema declares `status.agentToken` AND `status.configMapName` (auto-generated from the blueprint — the M3 pruning drift cannot recur).
- `spec.permissionClaims` contains `{group: "", resource: configmaps}` (auto-derived; core → empty identityHash).
- The blueprint's `status.exportedAPI == kubernetesclusters.krop.opendefense.cloud` and `status.observedSpecHash` is set; Ready condition True.

`t.Skip` unless `TEST_KCP_ASSETS`. Register `kropv1alpha1` + `apisv1alpha1` + `apisv1alpha2` + `apiextensionsv1` on the suite scheme.

- [ ] **Step 3: Run hermetic then real**

Hermetic: `go build ./... && go vet ./... && go test ./... 2>&1 | tail` → registrar e2e SKIPs.
Real: `make bin/kcp`, `TEST_KCP_ASSETS=$(pwd)/bin go test ./internal/registrar/ -v -timeout 360s`. Report actual output. If the graph build fails (AgentRequest not discoverable), ensure the AgentRequest CRD is Established before `Reconcile`. Never weaken assertions.

- [ ] **Step 4: Commit**

```bash
git add test/fixtures/blueprint-kubernetescluster-rgd.yaml internal/registrar/publish_e2e_test.go
git commit --no-verify -m "registrar: publication e2e — blueprint auto-produces APIExport+ARS+claims (M4a)"
```

**== M4a COMPLETE (mergeable). M4b begins below. ==**

---

## Task 6: Instance-manager supervisor

**Files:**
- Create: `internal/supervisor/supervisor.go`, `internal/supervisor/supervisor_test.go`

Owns per-blueprint instance-serving managers. Constraint: one manager ⇄ one APIExport, so each published blueprint gets its own goroutine-manager, torn down on delete.

- [ ] **Step 1: Write the failing test (lifecycle bookkeeping)**

```go
// internal/supervisor/supervisor_test.go
package supervisor

import (
	"context"
	"testing"
)

func TestSupervisor_EnsureIsIdempotent_AndStopReleases(t *testing.T) {
	started := 0
	s := New(func(ctx context.Context, export string) error { started++; return nil })

	s.Ensure(context.Background(), "export-a")
	s.Ensure(context.Background(), "export-a") // second call: already running, no new start
	if started != 1 {
		t.Fatalf("start called %d times, want 1 (idempotent)", started)
	}
	if !s.Running("export-a") {
		t.Fatal("export-a should be running")
	}
	s.Stop("export-a")
	if s.Running("export-a") {
		t.Fatal("export-a should be stopped")
	}
	s.Ensure(context.Background(), "export-a") // restart after stop
	if started != 2 {
		t.Fatalf("start called %d, want 2 after restart", started)
	}
}
```

- [ ] **Step 2: Verify fail; implement**

Run: `go test ./internal/supervisor/ -run TestSupervisor -v` → FAIL.

```go
// internal/supervisor/supervisor.go
// Copyright 2026 opendefense contributors
// ... (Apache header) ...

// Package supervisor owns one instance-serving manager per published blueprint
// APIExport. A single multicluster manager binds exactly one APIExport (apiexport.New
// takes one endpoint slice), so serving N blueprints means N managers — started in
// goroutines and torn down by cancelling their context.
package supervisor

import (
	"context"
	"sync"
)

// StartFunc launches a blocking instance-serving manager for one APIExport; it
// returns when the passed context is cancelled (or on fatal error).
type StartFunc func(ctx context.Context, exportName string) error

// Supervisor tracks running per-export managers.
type Supervisor struct {
	start StartFunc

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// New returns a Supervisor that uses start to launch each manager.
func New(start StartFunc) *Supervisor {
	return &Supervisor{start: start, running: map[string]context.CancelFunc{}}
}

// Ensure starts a manager for exportName if not already running (idempotent).
func (s *Supervisor) Ensure(parent context.Context, exportName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[exportName]; ok {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.running[exportName] = cancel
	go func() {
		// start blocks until ctx is cancelled; ignore the returned error here
		// (the caller observes liveness via Running / logs inside start).
		_ = s.start(ctx, exportName)
	}()
}

// Running reports whether a manager is active for exportName.
func (s *Supervisor) Running(exportName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[exportName]
	return ok
}

// Stop cancels and forgets the manager for exportName.
func (s *Supervisor) Stop(exportName string) {
	s.mu.Lock()
	cancel, ok := s.running[exportName]
	delete(s.running, exportName)
	s.mu.Unlock()
	if ok {
		cancel()
	}
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/supervisor/ -v` → PASS.

```bash
git add internal/supervisor/
git commit --no-verify -m "supervisor: per-blueprint instance-manager lifecycle (M4)"
```

---

## Task 7: Wire Registrar + Supervisor into main.go

**Files:**
- Modify: `cmd/controller/main.go`

Replace the hardcoded single-APIExport wiring with: a controller-runtime manager on `cfg` running the Registrar (watching `ResourceGraphDefinition`), whose `OnPublished` calls `supervisor.Ensure(exportName)`; the supervisor's `StartFunc` builds an `apiexport.Provider` for that export's endpoint slice + `mcmanager` + registers `internal/controller.Reconciler` with the cached graph + `mgr.Start(ctx)` (the current main.go body, parameterized by export name + graph).

- [ ] **Step 1: Refactor main.go**

Extract the current `apiexport.New → mcmanager.New → ControllerManagedBy → mgr.Start` sequence into a `StartFunc` closure `func(ctx, exportName) error` that: discovers the endpoint slice for `exportName` (retry while empty — no binding yet), builds the provider/manager, looks up the graph + instanceGVK for that export (from a map the Registrar populated on publish), registers `&kropctrl.Reconciler{Graph: g, ProviderClient: providerClient, InstanceGVK: gvk}`, and starts. Build the provider-workspace controller-runtime manager for the Registrar (scheme: kropv1alpha1 + apis v1alpha1/v1alpha2 + clientgoscheme), register the Registrar with `For(&kropv1alpha1.ResourceGraphDefinition{})`, wire `OnPublished` to store `(graph, gvk)` and call `supervisor.Ensure`. Start both.

Full code is substantial and integration-specific; write it against the existing main.go and the two references (`cmd/controller/main.go` M3 form for the mc wiring, `internal/registrar` + `internal/supervisor` APIs above). Keep the M1–M3 constants only if still needed; the export name is now dynamic.

- [ ] **Step 2: Build + vet**

Run: `go build ./... && go vet ./...` → clean.

- [ ] **Step 3: Commit**

```bash
git add cmd/controller/main.go
git commit --no-verify -m "controller: run Registrar + Supervisor (dynamic per-blueprint serving) (M4)"
```

---

## Task 8: Auto-publication e2e

**Files:**
- Create: `test/fixtures/blueprint-kubernetescluster-rgd.yaml`
- Create: `internal/controller/registrar_e2e_test.go`

Proves the whole M4: create a blueprint → the Registrar publishes an APIExport → a consumer binds it and creates an instance → the engine materializes the children — with **no hand-written APIExport/ARS/claim fixtures** (only the blueprint + a consumer APIBinding that accepts the auto-derived claim).

- [ ] **Step 1: Write the blueprint fixture**

A `krop.opendefense.cloud/v1alpha1 ResourceGraphDefinition` with the M3 cross-target shape (provider AgentRequest + consumer ConfigMap reading its status). Reuse the body from `internal/engine/embedded/blueprint-kubernetescluster.yaml`, wrapped as our CRD kind.

- [ ] **Step 2: Write the e2e (envtest, package `controller_test`)**

Modeled on `dualtarget_test.go` but starting from a blueprint. `BeforeAll`: provider ws → install the blueprint CRD (`config/crds/krop.opendefense.cloud_resourcegraphdefinitions.yaml`) + the AgentRequest CRD → run the Registrar (a controller-runtime manager on the provider-workspace config, or call `registrar.Reconcile` directly once, per the access-operator direct-reconcile style) → create the blueprint → assert an `APIExport` `kubernetesclusters.krop.opendefense.cloud` is published with a `configmaps` permissionClaim → consumer ws binds it (accepting the claim) → start the instance manager (via the supervisor's StartFunc or directly) → create a `KubernetesCluster` → assert the consumer ConfigMap + provider AgentRequest (async, patch status) materialize, as in M3. `t.Skip` unless `TEST_KCP_ASSETS`.

Full test code follows the M2/M3 e2e patterns (bind-first, `apiexport.New` on the auto-published export's endpoint slice, `envtest.Eventually`). The key new assertions: (a) the APIExport + ARS exist after the blueprint is created (Registrar published them), (b) the ARS has `status.agentToken` (auto-generated from the blueprint schema — the M3 pruning bug cannot recur), (c) the derived `permissionClaims` contain `configmaps`.

- [ ] **Step 3: Run hermetic then real**

Hermetic: `go build ./... && go vet ./... && go test ./... 2>&1 | tail` → controller SKIPs.
Real: `make bin/kcp`, `TEST_KCP_ASSETS=$(pwd)/bin go test ./internal/controller/ -run TestControllerIntegration -v -timeout 420s` (or the registrar suite entry). Report actual output; debug real failures, never weaken assertions.

- [ ] **Step 4: Commit**

```bash
git add test/fixtures/blueprint-kubernetescluster-rgd.yaml internal/controller/registrar_e2e_test.go
git commit --no-verify -m "controller: auto-publication e2e (blueprint -> APIExport -> instance) (M4)"
```

---

## Definition of done (M4)

- `go build ./...`, `go vet ./...`, `go test ./...` green (controller SKIPs hermetically).
- The real e2e green: creating a `ResourceGraphDefinition` blueprint auto-produces a bindable `APIExport` (+ ARS with correct status fields, + `configmaps` permissionClaim), and a consumer that binds it can create an instance the **unchanged** M1–M3 engine/reconciler materializes across both workspaces.
- Compiled-graph cache keyed by `(workspace, name, specHash)`; specHash change-detection skips rebuilds.
- Dynamic per-blueprint instance managers via the Supervisor (one manager ⇄ one APIExport).
- No hand-written APIExport/ARS/claim fixtures in the M4 path — the M3 ARS-drift bug is structurally impossible (ARS auto-generated from `graph.CRD`).

## Self-review notes
- Watches only the configured provider workspace (single-provider-ws); multi-provider-workspace blueprint discovery is deferred.
- ARS is treated as immutable-once-served: a new specHash mints a new ARS name; retiring old ARS/APIExports is M5 GC.
- The Supervisor start must tolerate an empty endpoint slice (no consumer binding yet) — retry until the slice populates (design §2: URL empty until a binding exists), matching the M2-spike bind-first rule.
- identityHash for foreign types comes from `APIBinding.status.boundResources` in the provider workspace (the blueprint must bind the foreign types for the graph build to type-check them); core types (configmaps) get empty identityHash. Re-read each reconcile handles identityHash drift (design §13).
