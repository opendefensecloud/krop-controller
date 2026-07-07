# M3 — Cross-Target CEL + Status Mapping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A consumer-target child resolves a value from a provider-target child's **status** across workspaces via CEL — proven end-to-end including the realistic **async** flow (the consumer child does not materialize until the provider child's status is populated), plus instance status mapped from a child status.

**Architecture:** No engine changes. The engine already drives nodes in global topological order across targets, feeds each applied child's read-back observed state (both targets) into the one kro runtime context, resolves cross-resource CEL from that observed state, and — when a dependency's referenced field is not yet present — returns `Ready=false, Requeue=true` **without** creating the dependent child (no partial write). M3 proves this with formalized unit tests and an async e2e, and introduces a status-bearing provider child type (a minimal `AgentRequest` CRD), because kro type-checks CEL against child schemas at graph-build time and a core `ConfigMap` has no arbitrary `status`.

**Tech Stack:** Go 1.26, kro v0.9.2, kcp-dev/sdk v0.32.2, multicluster-provider v0.8.0, multicluster-runtime v0.24.1, controller-runtime v0.24.1, k8s v0.36.2. Envtest kcp binary v0.30.0.

**Grounding (verified via a throwaway scratch before this plan):**
- With provider=VPC, consumer=Subnet and `subnet.spec.vpcID = ${vpc.status.vpcID}`: topological order `[vpc subnet]`; when the provider applier injects `status.vpcID` on read-back, the consumer child resolves it (cross-target CEL works) and instance status projects from the provider child.
- When the provider child is observed but its `status` field is **absent**: `GetDesired` on the consumer node pends → engine returns `Ready=false, Requeue=true` and the consumer child is **not** applied (count 0). No partial write. This is the async behavior M3's e2e exercises against real kcp.
- kro's `Builder` type-checks `${x.status.field}` against `x`'s OpenAPI schema at build time — so the referenced provider child must be a type whose schema declares that status field (a CRD, not core ConfigMap).

**Scope:**
- **In scope:** a cross-target dependency (consumer child reads provider child status), the async pending→converge flow, instance status mapped from a child status, the provider-rename × CEL interaction (the test patches the *renamed* provider object; consumer references resolve against the renamed observed object). Proven by unit tests + a real-kcp e2e.
- **Deferred:** claims auto-derivation (M4), GC/finalizers (M5), realistic downstream fulfilment controllers (the e2e itself simulates the downstream by patching the AgentRequest status).

---

## File structure

| File | Responsibility |
|---|---|
| `test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml` | Minimal provider-side CRD: `AgentRequest` with `spec.region` (string) + `status.token` (string), status subresource. Installed in the provider ws by the e2e. |
| `internal/engine/crosstarget_test.go` | Formalized cross-target unit tests (VPC→Subnet via the fake resolver): resolution + async pending. No engine code. |
| `internal/engine/embedded/blueprint-kubernetescluster.yaml` + `config/kcp/examples/blueprint-kubernetescluster.yaml` (modify) | Evolve to the cross-target shape: provider child = `AgentRequest`, consumer child = `ConfigMap` reading `${agentRequest.status.token}`; instance `status.agentToken` mapped from it. |
| `internal/engine/blueprint_test.go` (modify) | Update `TestLoadExampleBlueprint` for the new resource shape. |
| `internal/controller/dualtarget_test.go` (modify) | Evolve the e2e: install the AgentRequest CRD, honor requeue, prove the async cross-target flow. |

No changes to `internal/engine/{engine,apply,route,naming,status,graphsource,blueprint}.go`, `internal/controller/reconciler.go`, or `cmd/controller/main.go` — the engine and reconcile glue already do everything M3 needs. (`main.go` already maps `res.Requeue → RequeueAfter`.)

---

## Task 1: AgentRequest CRD fixture

**Files:**
- Create: `test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml`

A plain CRD (installed directly into the provider workspace — no APIExport needed, provider-target children live in the provider's own ownership domain). It must have a `status` subresource so the e2e can patch `status.token`, and a `spec.region` field.

- [ ] **Step 1: Write the CRD**

```yaml
# test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: agentrequests.fulfil.krop.opendefense.cloud
spec:
  group: fulfil.krop.opendefense.cloud
  scope: Namespaced
  names:
    plural: agentrequests
    singular: agentrequest
    kind: AgentRequest
    listKind: AgentRequestList
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                region:
                  type: string
            status:
              type: object
              properties:
                token:
                  type: string
```

- [ ] **Step 2: Commit**

```bash
git add test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml
git commit --no-verify -m "kcp: AgentRequest CRD fixture (status-bearing provider child) (M3)"
```

---

## Task 2: Cross-target engine unit tests (no engine code)

**Files:**
- Create: `internal/engine/crosstarget_test.go`

Formalizes the scratch proof as permanent tests, using the fake resolver's status-bearing EC2 types (the fake resolver knows VPC/Subnet with `status`; it does NOT know our AgentRequest CRD, so unit tests can't use AgentRequest — that's the e2e's job).

- [ ] **Step 1: Write the tests**

```go
// internal/engine/crosstarget_test.go
// Copyright 2026 opendefense contributors
// ... (Apache header — copy the Copyright block from route.go) ...

package engine

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// crossTargetRGD: provider-target vpc (VPC.status.vpcID is a real schema field in
// the fake resolver) and consumer-target subnet reading ${vpc.status.vpcID}.
func crossTargetRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"xtarget",
		generator.WithSchema("XTarget", "v1alpha1",
			map[string]interface{}{"region": "string"},
			map[string]interface{}{"vpcID": "${vpc.status.vpcID}"},
		),
		generator.WithResource("vpc", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "VPC",
			"metadata": map[string]interface{}{
				"name":        "${schema.spec.region}-vpc",
				"annotations": map[string]interface{}{TargetAnnotation: string(TargetProvider)},
			},
			"spec": map[string]interface{}{"cidrBlocks": []interface{}{"10.0.0.0/16"}},
		}, []string{"${vpc.status.vpcID != ''}"}, nil),
		generator.WithResource("subnet", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "Subnet",
			"metadata": map[string]interface{}{
				"name":        "${schema.spec.region}-subnet",
				"annotations": map[string]interface{}{TargetAnnotation: string(TargetConsumer)},
			},
			"spec": map[string]interface{}{"vpcID": "${vpc.status.vpcID}", "cidrBlock": "10.0.1.0/24"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"
	return rgd
}

func crossTargetRuntime(t *testing.T) *runtime.Runtime {
	t.Helper()
	g := buildTestGraph(t, crossTargetRGD())
	inst := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "XTarget",
		"metadata": map[string]interface{}{"name": "demo", "namespace": "default"},
		"spec":     map[string]interface{}{"region": "eu"},
	}}
	rt, err := runtime.FromGraph(g, inst, graph.RGDConfig{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000})
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}
	return rt
}

// Provider status present → consumer resolves it, instance status projects.
func TestReconcile_CrossTargetCEL_ResolvesFromProviderStatus(t *testing.T) {
	rt := crossTargetRuntime(t)
	consumer := &fakeApplier{}
	provider := &fakeApplier{mutate: func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, "vpc-abc123", "status", "vpcID")
	}}

	res, err := New().Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer, TargetProvider: provider,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 1 {
		t.Fatalf("consumer applied %d, want 1", len(consumer.applied))
	}
	if got, _, _ := unstructured.NestedString(consumer.applied[0].Object, "spec", "vpcID"); got != "vpc-abc123" {
		t.Fatalf("cross-target CEL: subnet.spec.vpcID = %q, want vpc-abc123", got)
	}
	di, err := ProjectStatus(rt)
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if got, _, _ := unstructured.NestedString(di.Object, "status", "vpcID"); got != "vpc-abc123" {
		t.Fatalf("instance status.vpcID = %q, want vpc-abc123", got)
	}
	if !res.Ready {
		t.Fatalf("want Ready, got %+v", res)
	}
}

// Provider status absent → consumer child NOT created (no partial write), requeue.
func TestReconcile_CrossTargetPendsUntilProviderReady(t *testing.T) {
	rt := crossTargetRuntime(t)
	consumer := &fakeApplier{}
	provider := &fakeApplier{} // no status injected

	res, err := New().Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer, TargetProvider: provider,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 0 {
		t.Fatalf("partial write: consumer applied %d, want 0 (should pend)", len(consumer.applied))
	}
	if res.Ready || !res.Requeue {
		t.Fatalf("want Ready=false Requeue=true, got %+v", res)
	}
	// The provider child WAS applied (it has no cross-target dep of its own).
	if len(provider.applied) != 1 {
		t.Fatalf("provider applied %d, want 1", len(provider.applied))
	}
}
```

- [ ] **Step 2: Run to verify they pass**

Run: `go test ./internal/engine/ -run TestReconcile_CrossTarget -v`
Expected: PASS (both). These prove the engine already does cross-target resolution + async pending — no engine code changed.

- [ ] **Step 3: Commit**

```bash
git add internal/engine/crosstarget_test.go
git commit --no-verify -m "engine: formalize cross-target CEL + async-pending tests (M3)"
```

---

## Task 3: Evolve the blueprint to a cross-target shape

**Files:**
- Modify: `internal/engine/embedded/blueprint-kubernetescluster.yaml`
- Modify: `config/kcp/examples/blueprint-kubernetescluster.yaml`
- Modify: `internal/engine/blueprint_test.go`

Replace M2's independent provider `ConfigMap` (`providerRecord`) with a provider `AgentRequest`, and make the consumer `ConfigMap` read the AgentRequest's status. Keep both YAML copies byte-identical except the header sync-comment.

- [ ] **Step 1: Rewrite the blueprint body (BOTH copies)**

Set `spec.schema` and `spec.resources` to:

```yaml
spec:
  schema:
    apiVersion: v1alpha1
    kind: KubernetesCluster
    group: krop.opendefense.cloud
    spec:
      region: string
    status:
      configMapName: ${config.metadata.name}
      agentToken: ${agentRequest.status.token}
  resources:
    - id: agentRequest
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
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.region}-cluster-config
          namespace: default
          annotations:
            krop.opendefense.cloud/target: consumer
        data:
          region: ${schema.spec.region}
          token: ${agentRequest.status.token}
```

(Keep the header comment lines each copy already has; only the body below them changes. The `config` node now depends on `agentRequest.status.token`, forcing order `[agentRequest, config]` and making the consumer child pend until the AgentRequest has a status token.)

- [ ] **Step 2: Update the loader test**

`TestLoadExampleBlueprint` in `internal/engine/blueprint_test.go` currently asserts resources `config` + `providerRecord`. Change the expected ids to `config` + `agentRequest`:

```go
	if !ids["config"] || !ids["agentRequest"] {
		t.Fatalf("want resources config+agentRequest, got %v", ids)
	}
```

(The count is still 2; keep the `len(...) != 2` check.)

- [ ] **Step 3: Verify parse + full engine package**

Run: `go test ./internal/engine/ -v`
Expected: PASS. Note: nothing in the engine package builds a graph from the embedded blueprint (the fake resolver doesn't know `AgentRequest`), so only `TestLoadExampleBlueprint` (a pure parse) touches it — it passes with the updated assertion. `sampleRGD`-based tests are unaffected (they still use ConfigMap dual-target).

- [ ] **Step 4: Confirm byte-identity of the two blueprint copies (bodies)**

Run: `diff <(grep -v '^#' internal/engine/embedded/blueprint-kubernetescluster.yaml) <(grep -v '^#' config/kcp/examples/blueprint-kubernetescluster.yaml)`
Expected: no output (identical bodies).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/embedded/blueprint-kubernetescluster.yaml config/kcp/examples/blueprint-kubernetescluster.yaml internal/engine/blueprint_test.go
git commit --no-verify -m "blueprint: cross-target consumer reads provider AgentRequest status (M3)"
```

---

## Task 4: Async cross-target e2e

**Files:**
- Modify: `internal/controller/dualtarget_test.go`

Evolve the M2 vw e2e to the cross-target async flow. The manager watches the instance, so after the test patches the AgentRequest status the reconcile must be re-driven — the e2e reconciler must honor `res.Requeue` with a short `RequeueAfter` (the M2 e2e ignored `res`; fix that). Read the current `dualtarget_test.go` before editing.

- [ ] **Step 1: Install the AgentRequest CRD in the provider workspace**

In `BeforeAll`, after creating the provider workspace and BEFORE building the graph (the graph build type-checks `${agentRequest.status.token}` against the CRD's schema, so it must exist first), apply the CRD and wait for it to be Established:

```go
		applyFixtureFromFile(ctx, cli, providerPath, "../../test/fixtures/crd-agentrequests.fulfil.krop.opendefense.cloud.yaml", nil)
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
			if err := cli.Cluster(providerPath).Get(ctx, client.ObjectKey{Name: "agentrequests.fulfil.krop.opendefense.cloud"}, crd); err != nil {
				return false, err.Error()
			}
			conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
			for _, c := range conds {
				cm, _ := c.(map[string]interface{})
				if cm["type"] == "Established" && cm["status"] == "True" {
					return true, ""
				}
			}
			return false, "AgentRequest CRD not Established"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "AgentRequest CRD not established")
```

(The scheme registers apiextensions via clientgoscheme? No — register it in `suite_test.go` if a typed client is used; here we use unstructured for the CRD, so no scheme change is needed. If `cli.Cluster(...).Get` on the unstructured CRD fails to map, register `apiextensionsv1` in `suite_test.go`'s `init()` — check and add if needed.)

- [ ] **Step 2: Honor requeue in the e2e reconciler**

Change the `mcreconcile.Func` closure so a requeue re-drives the reconcile (needed for the async convergence after the status patch):

```go
			res, err := reconciler.Reconcile(ctx, cl.GetClient(), string(req.ClusterName), req.NamespacedName)
			if err != nil {
				return ctrl.Result{}, err
			}
			if res.Requeue {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, nil
```

- [ ] **Step 3: Replace the It(...) with the async cross-target assertions**

Replace the M2 `It("materializes ...")` body. The new flow: create the instance → the provider `AgentRequest` is created (renamed) → the consumer `ConfigMap` does NOT appear yet (pends on the absent status) → the test patches the AgentRequest's `status.token` → a requeue reconcile creates the consumer `ConfigMap` with the token → instance status maps `agentToken`.

```go
	It("pends the consumer child until the provider AgentRequest status is set, then propagates it", func() {
		instance := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "KubernetesCluster",
			"metadata": map[string]interface{}{"name": "demo", "namespace": "default"},
			"spec":     map[string]interface{}{"region": "eu"},
		}}
		Expect(cli.Cluster(consumerPath).Create(ctx, instance)).To(Succeed())

		// The provider-target AgentRequest is created in the provider ws (collision-named).
		agentName := kropengine.ProviderChildName(consumerWS.Spec.Cluster, "demo", "eu-agent")
		agentKey := client.ObjectKey{Namespace: "default", Name: agentName}
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			ar := &unstructured.Unstructured{}
			ar.SetGroupVersionKind(schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"})
			err := cli.Cluster(providerPath).Get(ctx, agentKey, ar)
			return err == nil, "agentrequest not created"
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "provider AgentRequest not created")

		// The consumer ConfigMap must NOT exist yet — it pends on the absent status.token.
		Consistently(func() bool {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm)
			return err != nil // still not found
		}, 2*time.Second, 200*time.Millisecond).Should(BeTrue(), "consumer child must pend until provider status is set")

		// Simulate the downstream fulfilment controller: patch the AgentRequest status.
		ar := &unstructured.Unstructured{}
		ar.SetGroupVersionKind(schema.GroupVersionKind{Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest"})
		Expect(cli.Cluster(providerPath).Get(ctx, agentKey, ar)).To(Succeed())
		Expect(unstructured.SetNestedField(ar.Object, "tok-xyz789", "status", "token")).To(Succeed())
		Expect(cli.Cluster(providerPath).Status().Update(ctx, ar)).To(Succeed())

		// Now (on a requeue reconcile) the consumer ConfigMap appears with the propagated token.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "eu-cluster-config"}, cm); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(cm.Object, "data", "token")
			return tok == "tok-xyz789", "consumer cm data.token="+tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "cross-target token did not propagate to the consumer child")

		// Instance status maps agentToken from the provider child status.
		envtest.Eventually(GinkgoT(), func() (bool, string) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(instance.GroupVersionKind())
			if err := cli.Cluster(consumerPath).Get(ctx, client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
				return false, err.Error()
			}
			tok, _, _ := unstructured.NestedString(got.Object, "status", "agentToken")
			return tok == "tok-xyz789", "status.agentToken="+tok
		}, wait.ForeverTestTimeout, 200*time.Millisecond, "instance status.agentToken not mapped")
	})
```

Implementer notes:
- `eu-agent` is the AgentRequest template name (`${schema.spec.region}-agent`), renamed by `ProviderChildName(consumerWS.Spec.Cluster, "demo", "eu-agent")` — patch the renamed object, since that's what exists in the provider ws.
- Ensure `time` and `Consistently` (Gomega) are imported. The `Describe` label/text can be updated from "M2 dual-target reconcile" to reflect M3 cross-target if you like, but keep `TestControllerIntegration` as the suite entry.
- Remove any now-unused M2 provider-ConfigMap assertions (the provider child is now an AgentRequest, asserted above).

- [ ] **Step 4: Run hermetic then real**

Hermetic: `go build ./... && go vet ./... && go test ./... 2>&1 | tail` → controller SKIPs, rest PASS.
Real: `make bin/kcp` (v0.30.0), `TEST_KCP_ASSETS=$(pwd)/bin go test ./internal/controller/ -v -timeout 360s`.
Expected: the cross-target spec PASSES — AgentRequest created, consumer pends, status patched, token propagates, instance status maps. If the graph build fails type-checking `${agentRequest.status.token}`, the CRD wasn't Established before the build — fix the ordering (Task 4 Step 1), not the blueprint. Never weaken assertions.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/dualtarget_test.go internal/controller/suite_test.go
git commit --no-verify -m "controller: async cross-target e2e (consumer pends on provider status) (M3)"
```

---

## Definition of done (M3)

- `go build ./...`, `go vet ./...`, `go test ./...` green (controller SKIPs hermetically).
- The real envtest e2e green: a `KubernetesCluster` creates a provider `AgentRequest`; the consumer `ConfigMap` **pends** until the AgentRequest's `status.token` is set; on convergence the token propagates cross-workspace into the consumer child and the instance `status.agentToken` maps from it.
- No engine or reconcile-glue code changed (M3 is tests + fixtures + blueprint); the cross-target mechanism was already present and is now proven.
- Unit tests cover cross-target resolution and async pending (no kcp needed).

## Self-review notes
- The provider child is now an `AgentRequest` (a CRD with status), not a core ConfigMap — required because kro type-checks `${agentRequest.status.token}` against the child schema at build time.
- The e2e simulates the downstream fulfilment controller by patching the AgentRequest status; a real downstream (api-syncagent turning AgentRequest into a running agent) is out of scope (idea.md §15, later).
- Requeue-driven convergence: the e2e reconciler now honors `res.Requeue` (1s) so the post-status-patch reconcile runs; production `main.go` already maps `res.Requeue → RequeueAfter`.
- The provider child is renamed by `QualifyingApplier`; the test patches the renamed object and the consumer's `${agentRequest.status.token}` resolves from the renamed observed object — exercising naming × CEL together.
