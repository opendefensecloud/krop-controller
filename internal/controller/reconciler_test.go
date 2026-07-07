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
	"slices"
	"testing"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

var testGVK = schema.GroupVersionKind{Group: "krop.opendefense.cloud", Version: "v1alpha1", Kind: "KubernetesCluster"}

func controllerHasFinalizer(u *unstructured.Unstructured, f string) bool {
	return slices.Contains(u.GetFinalizers(), f)
}

func mkInstance(name string, deleting bool, finalizers ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(testGVK)
	u.SetName(name)
	u.SetNamespace("default")
	u.SetUID("uid-1")
	if len(finalizers) > 0 {
		u.SetFinalizers(finalizers)
	}
	if deleting {
		now := metav1.Now()
		u.SetDeletionTimestamp(&now)
	}

	return u
}

func TestReconciler_InstanceNotFound_NoError(t *testing.T) {
	consumer := fake.NewClientBuilder().Build()
	r := &Reconciler{Graph: nil, ProviderClient: consumer, InstanceGVK: testGVK}

	// No instance exists → IgnoreNotFound → no error, empty result.
	_, err := r.Reconcile(context.Background(), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "missing"})
	if err != nil {
		t.Fatalf("expected nil error on not-found, got %v", err)
	}
}

func TestReconcile_AddsFinalizerBeforeApply(t *testing.T) {
	inst := mkInstance("demo", false) // no finalizer yet
	consumer := fake.NewClientBuilder().WithObjects(inst).Build()
	r := &Reconciler{Graph: nil, ProviderClient: consumer, InstanceGVK: testGVK, BlueprintName: "bp"}

	// nil Graph is safe: the finalizer-add path returns before FromGraph.
	_, err := r.Reconcile(context.Background(), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "demo"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(testGVK)
	if err := consumer.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "demo"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !controllerHasFinalizer(got, kropengine.Finalizer) {
		t.Fatalf("finalizer not added: %v", got.GetFinalizers())
	}
}

// mkLabeledCM returns a ConfigMap carrying the instance-uid GC label.
func mkLabeledCM(name, uid string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	u.SetName(name)
	u.SetNamespace("default")
	u.SetLabels(map[string]string{kropengine.LabelInstanceUID: uid})

	return u
}

// graphWithConfigMapNode builds a minimal graph whose single node is a
// consumer-target ConfigMap, so childGVKs(TargetConsumer) returns {v1 ConfigMap}.
func graphWithConfigMapNode() *krograph.Graph {
	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	// Consumer is the default target (no routing annotation), matching TargetOf.
	return &krograph.Graph{Nodes: map[string]*krograph.Node{
		"config": {Template: tmpl},
	}}
}

func TestPruneChildren_DeletesStaleKeepsApplied(t *testing.T) {
	const uid = "uid-1"
	desired := mkLabeledCM("desired-child", uid)
	stale := mkLabeledCM("stale-child", uid)
	consumer := fake.NewClientBuilder().WithObjects(desired, stale).Build()

	r := &Reconciler{
		Graph:          graphWithConfigMapNode(),
		ProviderClient: consumer, // provider target has no child GVKs here → no-op
		InstanceGVK:    testGVK,
		BlueprintName:  "bp",
	}

	// Applied set contains only the desired child; stale-child is absent → prune.
	applied := map[kropengine.Target][]kropengine.ChildID{
		kropengine.TargetConsumer: {{
			GVK:       schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Namespace: "default", Name: "desired-child",
		}},
	}
	if err := r.pruneChildren(context.Background(), consumer, uid, applied); err != nil {
		t.Fatalf("pruneChildren: %v", err)
	}

	// stale-child must be gone.
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	err := consumer.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "stale-child"}, got)
	if err == nil {
		t.Fatal("stale-child was not pruned")
	}
	// desired-child must remain.
	got = &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := consumer.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "desired-child"}, got); err != nil {
		t.Fatalf("desired-child was wrongly pruned: %v", err)
	}
}

func TestLivenessRecord_WriteThenDelete(t *testing.T) {
	provider := fake.NewClientBuilder().Build()
	r := &Reconciler{
		Graph:          graphWithConfigMapNode(), // consumer-target only → empty provider GVK list
		ProviderClient: provider,
		InstanceGVK:    testGVK,
		BlueprintName:  "bp",
	}

	// Write creates the record with the expected labels + data.
	if err := r.writeLivenessRecord(context.Background(), "cluster1", "uid-1"); err != nil {
		t.Fatalf("writeLivenessRecord: %v", err)
	}
	name := kropengine.LivenessRecordName("cluster1", "uid-1")
	rec := &unstructured.Unstructured{}
	rec.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := provider.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, rec); err != nil {
		t.Fatalf("record not created: %v", err)
	}
	if rec.GetLabels()[kropengine.LabelLiveness] != "true" {
		t.Errorf("missing liveness label: %v", rec.GetLabels())
	}
	if rec.GetLabels()[kropengine.LabelInstanceUID] != "uid-1" {
		t.Errorf("missing instance-uid label: %v", rec.GetLabels())
	}
	if last, _, _ := unstructured.NestedString(rec.Object, "data", "lastReconciled"); last == "" {
		t.Error("lastReconciled not set")
	}

	// A second write updates (upserts), not errors.
	if err := r.writeLivenessRecord(context.Background(), "cluster1", "uid-1"); err != nil {
		t.Fatalf("second writeLivenessRecord: %v", err)
	}

	// Delete removes it; a second delete is a no-op (not-found ignored).
	if err := r.deleteLivenessRecord(context.Background(), "cluster1", "uid-1"); err != nil {
		t.Fatalf("deleteLivenessRecord: %v", err)
	}
	if err := provider.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, rec); err == nil {
		t.Fatal("record not deleted")
	}
	if err := r.deleteLivenessRecord(context.Background(), "cluster1", "uid-1"); err != nil {
		t.Fatalf("deleteLivenessRecord on missing record should be nil: %v", err)
	}
}

func TestReconcile_DeletionRemovesFinalizer_EmptyGraph(t *testing.T) {
	inst := mkInstance("demo", true, kropengine.Finalizer)
	consumer := fake.NewClientBuilder().WithObjects(inst).Build()
	r := &Reconciler{
		Graph:          &krograph.Graph{Nodes: map[string]*krograph.Node{}},
		ProviderClient: consumer,
		InstanceGVK:    testGVK,
		BlueprintName:  "bp",
	}
	_, err := r.Reconcile(context.Background(), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "demo"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(testGVK)
	err = consumer.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "demo"}, got)
	// With no finalizers left, the fake client GCs the deleting object → NotFound,
	// OR it exists with the finalizer gone. Accept either.
	if err == nil && controllerHasFinalizer(got, kropengine.Finalizer) {
		t.Fatalf("finalizer not removed: %v", got.GetFinalizers())
	}
}
