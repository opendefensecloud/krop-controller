// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	memory "k8s.io/client-go/discovery/cached/memory"
	restmapper "k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// conditionsRGD is a two-node blueprint (consumer + provider ConfigMap, no
// readyWhen) that reconciles fully Ready in one pass, so the projected Ready
// condition should be True.
func conditionsRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"kubernetescluster",
		generator.WithSchema("KubernetesCluster", "v1alpha1",
			map[string]any{"region": "string"},
			map[string]any{"configMapName": "${config.metadata.name}"},
		),
		generator.WithResource("config", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.spec.region}-cluster-config",
				"namespace": "default",
			},
			"data": map[string]any{"region": "${schema.spec.region}"},
		}, nil, nil),
		generator.WithResource("providerRecord", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.spec.region}-provider-record",
				"namespace": "default",
			},
			"data": map[string]any{"region": "${schema.spec.region}"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

// buildConditionsGraph compiles conditionsRGD against kro's fake resolver (no
// live cluster) by injecting it into a graph.Builder via unsafe (test-only).
func buildConditionsGraph(t *testing.T) *krograph.Graph {
	t.Helper()

	return buildFakeGraph(t, conditionsRGD())
}

// buildFakeGraph compiles rgd against kro's fake resolver (see buildConditionsGraph).
func buildFakeGraph(t *testing.T, rgd *krov1alpha1.ResourceGraphDefinition) *krograph.Graph {
	t.Helper()
	fakeResolver, fakeDiscovery := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDiscovery))
	b := &krograph.Builder{}
	setBuilderField(b, "schemaResolver", fakeResolver)
	setBuilderField(b, "restMapper", meta.RESTMapper(rm))
	g, err := b.NewResourceGraphDefinition(rgd, krograph.Config{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		t.Fatalf("buildFakeGraph: %v", err)
	}

	return g
}

func setBuilderField(obj any, name string, val any) {
	rv := reflect.ValueOf(obj).Elem().FieldByName(name)
	rv = reflect.NewAt(rv.Type(), unsafe.Pointer(rv.UnsafeAddr())).Elem()
	rv.Set(reflect.ValueOf(val))
}

// TestReconcile_SetsReadyCondition drives a full reconcile with fake clients and
// asserts the Ready condition is written onto the instance's status.conditions
// (I2). The instance already carries the finalizer so the pass reaches apply.
func TestReconcile_SetsReadyCondition(t *testing.T) {
	inst := mkInstance("demo", false, kropengine.Finalizer)
	_ = unstructured.SetNestedField(inst.Object, "eu", "spec", "region")

	consumer := fake.NewClientBuilder().
		WithObjects(inst).
		WithStatusSubresource(inst).
		Build()
	provider := fake.NewClientBuilder().Build()

	r := &Reconciler{
		Graph:          buildConditionsGraph(t),
		ProviderClient: provider,
		InstanceGVK:    testGVK,
		BlueprintName:  "bp",
		Routing:        map[string]kropengine.Target{"config": kropengine.TargetConsumer, "providerRecord": kropengine.TargetProvider},
	}

	res, err := r.Reconcile(context.Background(), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "demo"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Ready {
		t.Fatalf("want engine Ready=true for a no-readyWhen blueprint, got %+v", res)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(testGVK)
	if gerr := consumer.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "demo"}, got); gerr != nil {
		t.Fatalf("get instance: %v", gerr)
	}

	conds := readConditions(got)
	ready := meta.FindStatusCondition(conds, conditionReady)
	if ready == nil {
		t.Fatalf("Ready condition not set on instance; status=%v", got.Object["status"])
	}
	if ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %q, want True", ready.Status)
	}
	// A fully-ready pass is not progressing.
	prog := meta.FindStatusCondition(conds, conditionProgressing)
	if prog == nil || prog.Status != metav1.ConditionFalse {
		t.Fatalf("Progressing condition = %+v, want False", prog)
	}
	// The CEL-projected status field must survive alongside the conditions.
	if name, _, _ := unstructured.NestedString(got.Object, "status", "configMapName"); name != "eu-cluster-config" {
		t.Fatalf("status.configMapName = %q, want eu-cluster-config", name)
	}
}

// TestReconcile_BlueprintDeclaredStateWins verifies that a blueprint mapping
// status.state itself gets its projected value, not krop's state and not the
// value persisted by an earlier pass.
func TestReconcile_BlueprintDeclaredStateWins(t *testing.T) {
	rgd := conditionsRGD()
	rgd.Spec.Schema.Status.Raw = []byte(`{"state":"${config.data.region}"}`)

	inst := mkInstance("demo", false, kropengine.Finalizer)
	_ = unstructured.SetNestedField(inst.Object, "eu", "spec", "region")
	_ = unstructured.SetNestedField(inst.Object, "stale", "status", "state")
	consumer := fake.NewClientBuilder().WithObjects(inst).WithStatusSubresource(inst).Build()

	r := &Reconciler{
		Graph:          buildFakeGraph(t, rgd),
		ProviderClient: fake.NewClientBuilder().Build(),
		InstanceGVK:    testGVK,
		BlueprintName:  "bp",
		Routing:        map[string]kropengine.Target{"config": kropengine.TargetConsumer, "providerRecord": kropengine.TargetProvider},
	}
	if _, err := r.Reconcile(context.Background(), consumer, "cluster1", client.ObjectKey{Namespace: "default", Name: "demo"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(testGVK)
	if err := consumer.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if state, _, _ := unstructured.NestedString(got.Object, "status", "state"); state != "eu" {
		t.Fatalf("status.state = %q, want the projected eu", state)
	}
}

// TestApplyResultConditions_NotReady covers the progressing/not-ready branch and
// round-trips through the unstructured read/write helpers.
func TestApplyResultConditions_NotReady(t *testing.T) {
	inst := &unstructured.Unstructured{Object: map[string]any{}}

	var conds []metav1.Condition
	applyResultConditions(&conds, kropengine.Result{Ready: false, Requeue: true}, readyReport{reason: reasonInProgress}, 3)
	if err := writeConditions(inst, conds); err != nil {
		t.Fatalf("writeConditions: %v", err)
	}

	rt := readConditions(inst)
	ready := meta.FindStatusCondition(rt, conditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonInProgress {
		t.Fatalf("Ready = %+v, want False/InProgress", ready)
	}
	if ready.ObservedGeneration != 3 {
		t.Fatalf("Ready.ObservedGeneration = %d, want 3", ready.ObservedGeneration)
	}
	prog := meta.FindStatusCondition(rt, conditionProgressing)
	if prog == nil || prog.Status != metav1.ConditionTrue {
		t.Fatalf("Progressing = %+v, want True", prog)
	}
}

// TestApplyResultConditions_ProgressPercent verifies that the Progressing message
// is the share of ready nodes alone, which the portal's list view shows: no node
// names and no node counts, which would expose the blueprint's makeup.
func TestApplyResultConditions_ProgressPercent(t *testing.T) {
	cases := []struct {
		name string
		res  kropengine.Result
		want string
	}{
		{"one of three", kropengine.Result{Nodes: 3, ReadyNodes: 1, Node: "bucket", Dependency: "objectadmin"}, "33%"},
		{"two of three", kropengine.Result{Nodes: 3, ReadyNodes: 2, Node: "credentials"}, "66%"},
		{"no nodes", kropengine.Result{Node: "bucket"}, "0%"},
		{"ready", kropengine.Result{Ready: true, Nodes: 3, ReadyNodes: 3}, "100%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var conds []metav1.Condition
			applyResultConditions(&conds, tc.res, readyReport{reason: reasonInProgress}, 1)
			if got := meta.FindStatusCondition(conds, conditionProgressing).Message; got != tc.want {
				t.Fatalf("Progressing message = %q, want %q", got, tc.want)
			}
			if tc.res.Ready {
				if got := meta.FindStatusCondition(conds, conditionReady).Message; strings.ContainsAny(got, "0123456789") {
					t.Fatalf("Ready message %q gives a node count", got)
				}
			}
		})
	}
}

// TestApplyResultConditions_PreservesTransitionTime verifies that a second pass
// with an unchanged Ready status keeps the original lastTransitionTime (the
// read-before-project ordering in Reconcile depends on this).
func TestApplyResultConditions_PreservesTransitionTime(t *testing.T) {
	var conds []metav1.Condition
	applyResultConditions(&conds, kropengine.Result{Ready: true}, readyReport{}, 1)
	first := meta.FindStatusCondition(conds, conditionReady).LastTransitionTime

	time.Sleep(10 * time.Millisecond)
	applyResultConditions(&conds, kropengine.Result{Ready: true}, readyReport{}, 1)
	second := meta.FindStatusCondition(conds, conditionReady).LastTransitionTime

	if !first.Equal(&second) {
		t.Fatalf("lastTransitionTime changed on an unchanged status: %v -> %v", first, second)
	}
}

// A failed pass keeps the status fields it could not resolve, also inside an
// object whose other fields it did resolve; resolved values win. It follows the
// blueprint's schema.status: an expression's value, a map included, is kept or
// replaced whole, and a field the blueprint no longer has is not brought back.
func TestKeepUnresolved(t *testing.T) {
	tmpl := map[string]any{
		"bucketId":       "${bucket.spec.resourceID}",
		"endpoints":      "${credentials.data}",
		"bucketSecret":   map[string]any{"name": "${credentials.metadata.name}"},
		"providerStatus": map[string]any{"projectId": "${tenant.status.projectId}", "selfLink": "${bucket.status.selfLink}"},
	}
	projected := map[string]any{
		"bucketId":       "new",
		"endpoints":      map[string]any{},
		"providerStatus": map[string]any{"projectId": "p-1"},
	}
	prev := map[string]any{
		"bucketId":       "old",
		"endpoints":      map[string]any{"endpoint": "https://old.example"},
		"bucketSecret":   map[string]any{"name": "demo-credentials"},
		"providerStatus": map[string]any{"projectId": "p-0", "selfLink": "https://example/b/demo"},
		"removedField":   "from an earlier blueprint",
	}
	keepUnresolved(projected, prev, tmpl)

	want := map[string]any{
		"bucketId":       "new",
		"endpoints":      map[string]any{},
		"bucketSecret":   map[string]any{"name": "demo-credentials"},
		"providerStatus": map[string]any{"projectId": "p-1", "selfLink": "https://example/b/demo"},
	}
	if !reflect.DeepEqual(projected, want) {
		t.Fatalf("keepUnresolved = %v, want %v", projected, want)
	}
}
