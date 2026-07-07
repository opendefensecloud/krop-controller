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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"

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
