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

package registrar

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
)

// TestReconcile_FinalizerTeardownOnDelete proves the deletion path: a blueprint
// being deleted (DeletionTimestamp + our finalizer + a recorded ExportedAPI)
// triggers OnDeleted with the export name and releases the finalizer, letting the
// API server (here, the fake client) remove the object.
func TestReconcile_FinalizerTeardownOnDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kropv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering scheme: %v", err)
	}

	now := metav1.Now()
	bp := &kropv1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "widgets",
			DeletionTimestamp: &now,
			// The fake client only persists a DeletionTimestamp when the object
			// still carries a finalizer, so seed it with ours.
			Finalizers: []string{blueprintFinalizer},
		},
		Status: kropv1alpha1.BlueprintStatus{ExportedAPI: "widgets.example.com"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(bp).
		WithStatusSubresource(&kropv1alpha1.ResourceGraphDefinition{}).
		Build()

	var stopped []string
	r := &Registrar{
		Client:    c,
		OnDeleted: func(export string) { stopped = append(stopped, export) },
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "widgets"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if res != (reconcile.Result{}) {
		t.Fatalf("delete path should not requeue, got %+v", res)
	}

	if len(stopped) != 1 || stopped[0] != "widgets.example.com" {
		t.Fatalf("OnDeleted expected [widgets.example.com], got %v", stopped)
	}

	// Removing the last finalizer while a DeletionTimestamp is set makes the fake
	// client garbage-collect the object.
	got := &kropv1alpha1.ResourceGraphDefinition{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "widgets"}, got)
	if err == nil {
		if controllerutil.ContainsFinalizer(got, blueprintFinalizer) {
			t.Fatalf("finalizer %q was not removed", blueprintFinalizer)
		}
		return
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected Get error: %v", err)
	}
}


// TestReconcile_DeleteWithoutFinalizerIsNoop proves a blueprint being deleted that
// no longer carries our finalizer is a no-op: OnDeleted is not called again.
func TestReconcile_DeleteWithoutFinalizerIsNoop(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kropv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering scheme: %v", err)
	}

	now := metav1.Now()
	// Keep a placeholder finalizer so the fake client persists the deleting object,
	// but not ours — Reconcile must treat it as already torn down.
	bp := &kropv1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "widgets",
			DeletionTimestamp: &now,
			Finalizers:        []string{"other.example.com/keep"},
		},
		Status: kropv1alpha1.BlueprintStatus{ExportedAPI: "widgets.example.com"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bp).Build()

	called := false
	r := &Registrar{Client: c, OnDeleted: func(string) { called = true }}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "widgets"},
	}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if called {
		t.Fatal("OnDeleted must not fire when our finalizer is absent")
	}
}
