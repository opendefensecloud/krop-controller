// Copyright 2026 opendefense contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"reflect"
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
				"name":        "${schema.spec.region}-cluster-config",
				"namespace":   "default",
				"annotations": map[string]any{kropengine.TargetAnnotation: string(kropengine.TargetConsumer)},
			},
			"data": map[string]any{"region": "${schema.spec.region}"},
		}, nil, nil),
		generator.WithResource("providerRecord", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name":        "${schema.spec.region}-provider-record",
				"namespace":   "default",
				"annotations": map[string]any{kropengine.TargetAnnotation: string(kropengine.TargetProvider)},
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
	fakeResolver, fakeDiscovery := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDiscovery))
	b := &krograph.Builder{}
	setBuilderField(b, "schemaResolver", fakeResolver)
	setBuilderField(b, "restMapper", meta.RESTMapper(rm))
	g, err := b.NewResourceGraphDefinition(conditionsRGD(), krograph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		t.Fatalf("buildConditionsGraph: %v", err)
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

// TestApplyResultConditions_NotReady covers the progressing/not-ready branch and
// round-trips through the unstructured read/write helpers.
func TestApplyResultConditions_NotReady(t *testing.T) {
	inst := &unstructured.Unstructured{Object: map[string]any{}}

	var conds []metav1.Condition
	applyResultConditions(&conds, kropengine.Result{Ready: false, Requeue: true}, 3)
	if err := writeConditions(inst, conds); err != nil {
		t.Fatalf("writeConditions: %v", err)
	}

	rt := readConditions(inst)
	ready := meta.FindStatusCondition(rt, conditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonProgressing {
		t.Fatalf("Ready = %+v, want False/Progressing", ready)
	}
	if ready.ObservedGeneration != 3 {
		t.Fatalf("Ready.ObservedGeneration = %d, want 3", ready.ObservedGeneration)
	}
	prog := meta.FindStatusCondition(rt, conditionProgressing)
	if prog == nil || prog.Status != metav1.ConditionTrue {
		t.Fatalf("Progressing = %+v, want True", prog)
	}
}

// TestApplyResultConditions_PreservesTransitionTime verifies that a second pass
// with an unchanged Ready status keeps the original lastTransitionTime (the
// read-before-project ordering in Reconcile depends on this).
func TestApplyResultConditions_PreservesTransitionTime(t *testing.T) {
	var conds []metav1.Condition
	applyResultConditions(&conds, kropengine.Result{Ready: true}, 1)
	first := meta.FindStatusCondition(conds, conditionReady).LastTransitionTime

	time.Sleep(10 * time.Millisecond)
	applyResultConditions(&conds, kropengine.Result{Ready: true}, 1)
	second := meta.FindStatusCondition(conds, conditionReady).LastTransitionTime

	if !first.Equal(&second) {
		t.Fatalf("lastTransitionTime changed on an unchanged status: %v -> %v", first, second)
	}
}
