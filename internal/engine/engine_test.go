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

// internal/engine/engine_test.go
package engine

import (
	"context"
	"errors"
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newInstance() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krop.opendefense.cloud/v1alpha1",
		"kind":       "KubernetesCluster",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{"region": "eu"},
	}}
}

func newRuntime(t *testing.T, inst *unstructured.Unstructured) *runtime.Runtime {
	t.Helper()
	g := buildTestGraph(t, sampleRGD())
	rt, err := runtime.FromGraph(g, inst, graph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}

	return rt
}

func TestReconcile_AppliesConsumerChild_StripsRouting(t *testing.T) {
	inst := newInstance()
	rt := newRuntime(t, inst)
	consumer := &fakeApplier{}

	e := New()
	res, err := e.Reconcile(context.Background(), rt, map[Target]Applier{TargetConsumer: consumer, TargetProvider: &fakeApplier{}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 1 {
		t.Fatalf("want 1 applied consumer object, got %d", len(consumer.applied))
	}
	got := consumer.applied[0]
	// CEL resolved from the instance:
	if got.GetName() != "eu-cluster-config" {
		t.Fatalf("child name = %q, want eu-cluster-config", got.GetName())
	}
	if region, _, _ := unstructured.NestedString(got.Object, "data", "region"); region != "eu" {
		t.Fatalf("child data.region = %q, want eu", region)
	}
	// routing annotation stripped before apply:
	if _, ok := got.GetAnnotations()[TargetAnnotation]; ok {
		t.Fatal("routing annotation leaked onto applied object")
	}
	if res.Ready != true {
		t.Fatalf("want Ready=true (ConfigMap has no readyWhen), got %+v", res)
	}
	if !res.Complete {
		t.Fatalf("want Complete=true after a full pass, got %+v", res)
	}
}

func TestReconcile_UnconfiguredTargetErrors(t *testing.T) {
	rt := newRuntime(t, newInstance())
	e := New()
	// No consumer applier configured → the single consumer child cannot route.
	_, err := e.Reconcile(context.Background(), rt, map[Target]Applier{})
	if err == nil {
		t.Fatal("want error when the child's target has no configured applier")
	}
}

func TestReconcile_ProjectsInstanceStatus(t *testing.T) {
	inst := newInstance()
	rt := newRuntime(t, inst)
	consumer := &fakeApplier{}

	e := New()
	if _, err := e.Reconcile(context.Background(), rt, map[Target]Applier{TargetConsumer: consumer, TargetProvider: &fakeApplier{}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The blueprint maps status.configMapName = ${config.metadata.name}.
	desiredInstance, err := ProjectStatus(rt)
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	name, _, _ := unstructured.NestedString(desiredInstance.Object, "status", "configMapName")
	if name != "eu-cluster-config" {
		t.Fatalf("status.configMapName = %q, want eu-cluster-config", name)
	}
}

// brokenCELRGD has a consumer child whose desired-state CEL always fails at
// evaluation with a genuine type-conversion error (int() of a non-numeric string)
// — NOT a pending-dependency error. The graph still builds (the expression type-
// checks: int(string) is a valid overload); it only errors at GetDesired.
func brokenCELRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"broken",
		generator.WithSchema("Broken", "v1alpha1",
			map[string]any{"region": "string"},
			map[string]any{"x": "${config.metadata.name}"},
		),
		generator.WithResource("config", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name":        "cfg",
				"namespace":   "default",
				"annotations": map[string]any{TargetAnnotation: string(TargetConsumer)},
			},
			// int("abc") is a runtime type-conversion error, not data-pending.
			"data": map[string]any{"n": "${string(int(schema.spec.region))}"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

// A genuine CEL evaluation error (not ErrDataPending) must be RETURNED as an
// error so a broken blueprint surfaces instead of hot-looping silently (I1).
func TestReconcile_GenuineGetDesiredError_IsReturned(t *testing.T) {
	g := buildTestGraph(t, brokenCELRGD())
	inst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "Broken",
		"metadata": map[string]any{"name": "demo", "namespace": "default"},
		"spec":     map[string]any{"region": "abc"}, // int("abc") → type conversion error
	}}
	rt, err := runtime.FromGraph(g, inst, graph.RGDConfig{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000})
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}

	res, err := New().Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{},
	})
	if err == nil {
		t.Fatalf("want a hard error for a genuine CEL failure, got nil (res=%+v)", res)
	}
	if errors.Is(err, runtime.ErrDataPending) {
		t.Fatalf("genuine CEL error must NOT be classified as ErrDataPending: %v", err)
	}
	// A hard error returns before Complete is set → nothing prunes.
	if res.Complete {
		t.Fatalf("want Complete=false on a hard error, got %+v", res)
	}
}

func TestReconcile_RoutesToBothTargets(t *testing.T) {
	rt := newRuntime(t, newInstance())
	consumer := &fakeApplier{}
	provider := &fakeApplier{}

	e := New()
	res, err := e.Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer,
		TargetProvider: provider,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 1 || consumer.applied[0].GetName() != "eu-cluster-config" {
		t.Fatalf("consumer applier got %d objs: %+v", len(consumer.applied), consumer.applied)
	}
	if len(provider.applied) != 1 || provider.applied[0].GetName() != "eu-provider-record" {
		t.Fatalf("provider applier got %d objs: %+v", len(provider.applied), provider.applied)
	}
	if !res.Ready {
		t.Fatalf("want Ready, got %+v", res)
	}
	if !res.Complete {
		t.Fatalf("want Complete=true after a full pass over both targets, got %+v", res)
	}
}
