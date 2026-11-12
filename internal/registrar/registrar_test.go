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
	"reflect"
	"strings"
	"testing"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
	"go.opendefense.cloud/krop-controller/internal/testlog"
)

// TestReconcile_FinalizerTeardownOnDelete proves the deletion path: a blueprint
// being deleted (DeletionTimestamp + our finalizer + a recorded ExportedAPI)
// triggers OnDeleted with the export name and releases the finalizer, letting the
// API server (here, the fake client) remove the object.
func TestReconcile_FinalizerTeardownOnDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		kropv1alpha1.AddToScheme,
		apisv1alpha1.AddToScheme,
		apisv1alpha2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("registering scheme: %v", err)
		}
	}

	const (
		exportName = "widgets.example.com"
		arsName    = "vdeadbeef.widgets.example.com"
	)
	now := metav1.Now()
	bp := &kropv1alpha1.ResourceGraphDefinition{
		Name:              "widgets",
		DeletionTimestamp: &now,
		// The fake client only persists a DeletionTimestamp when the object
		// still carries a finalizer, so seed it with ours.
		Finalizers: []string{blueprintFinalizer},
		Status:     kropv1alpha1.BlueprintStatus{ExportedAPI: exportName},
	}
	// Seed the published APIExport (referencing its ARS) and the ARS itself, so the
	// cascade-unpublish path can find and delete them.
	export := &apisv1alpha2.APIExport{Name: exportName}
	export.Spec.Resources = []apisv1alpha2.ResourceSchema{{
		Name:   "widgets",
		Group:  "example.com",
		Schema: arsName,
	}}
	ars := &apisv1alpha1.APIResourceSchema{Name: arsName}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(bp, export, ars).
		WithStatusSubresource(&kropv1alpha1.ResourceGraphDefinition{}).
		Build()

	// A populated cache entry for this blueprint must be purged on delete.
	cache := NewGraphCache()

	var stopped []string
	r := &Registrar{
		Client:    c,
		Workspace: "ws",
		Cache:     cache,
		OnDeleted: func(export string) { stopped = append(stopped, export) },
	}

	lg, sink := testlog.New(0)
	res, err := r.Reconcile(log.IntoContext(context.Background(), lg), reconcile.Request{
		Name: "widgets",
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if res != (reconcile.Result{}) {
		t.Fatalf("delete path should not requeue, got %+v", res)
	}

	// Withdrawing the API is a deletion krop decides on: logged at the default level.
	deleted := sink.Find("deleted APIExport")
	if len(deleted) != 1 || deleted[0].Level != 0 || deleted[0].Values["export"] != exportName ||
		!reflect.DeepEqual(deleted[0].Values["schemas"], []any{arsName}) {
		t.Fatalf("want one default-level deleted APIExport line for %s, got: %s", exportName, sink.Raw())
	}

	if len(stopped) != 1 || stopped[0] != exportName {
		t.Fatalf("OnDeleted expected [%s], got %v", exportName, stopped)
	}

	// The published APIExport and its ARS must be cascade-deleted.
	if err := c.Get(context.Background(), types.NamespacedName{Name: exportName}, &apisv1alpha2.APIExport{}); !apierrors.IsNotFound(err) {
		t.Fatalf("APIExport %q should be deleted, got err=%v", exportName, err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: arsName}, &apisv1alpha1.APIResourceSchema{}); !apierrors.IsNotFound(err) {
		t.Fatalf("APIResourceSchema %q should be deleted, got err=%v", arsName, err)
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

// TestDeletePublishedAPI_AlreadyGoneNotLogged proves an APIExport that is already
// gone (e.g. on the retry after a failed finalizer removal) is not logged as a
// deletion: krop did not delete anything.
func TestDeletePublishedAPI_AlreadyGoneNotLogged(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apisv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("registering scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	lg, sink := testlog.New(0)
	if err := DeletePublishedAPI(log.IntoContext(context.Background(), lg), c, "widgets.example.com"); err != nil {
		t.Fatalf("DeletePublishedAPI: %v", err)
	}
	if n := len(sink.Find("deleted APIExport")); n != 0 {
		t.Fatalf("want no deleted APIExport line for an absent export, got: %s", sink.Raw())
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
		Name:              "widgets",
		DeletionTimestamp: &now,
		Finalizers:        []string{"other.example.com/keep"},
		Status:            kropv1alpha1.BlueprintStatus{ExportedAPI: "widgets.example.com"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bp).Build()

	called := false
	r := &Registrar{Client: c, OnDeleted: func(string) { called = true }}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		Name: "widgets",
	}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if called {
		t.Fatal("OnDeleted must not fire when our finalizer is absent")
	}
}

// TestReconcile_FailureErrorNamesStage proves a publish failure's returned error
// names the failing stage, so controller-runtime's default-level error line says
// where publishing broke, while the status message keeps the plain error. A
// pre-seeded graph cache skips the build; an invalid routing target then fails.
func TestReconcile_FailureErrorNamesStage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kropv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering scheme: %v", err)
	}
	bp := &kropv1alpha1.ResourceGraphDefinition{Name: "widgets"}
	bp.Spec.Resources = []*kropv1alpha1.Resource{{ID: "config", Target: "bogus"}}
	specHash, err := SpecHash(bp.Spec)
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	cache := NewGraphCache()
	cache.Put("ws", "widgets", specHash, &krograph.Graph{})
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(bp).
		WithStatusSubresource(&kropv1alpha1.ResourceGraphDefinition{}).
		Build()
	r := &Registrar{Client: c, Workspace: "ws", Cache: cache}

	_, err = r.Reconcile(context.Background(), reconcile.Request{Name: "widgets"})
	if err == nil || !strings.HasPrefix(err.Error(), "InvalidTarget: ") || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("want error prefixed with the stage InvalidTarget, got %v", err)
	}

	got := &kropv1alpha1.ResourceGraphDefinition{}
	if gerr := c.Get(context.Background(), types.NamespacedName{Name: "widgets"}, got); gerr != nil {
		t.Fatalf("get blueprint: %v", gerr)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if ready == nil || ready.Reason != "InvalidTarget" || strings.HasPrefix(ready.Message, "InvalidTarget") {
		t.Fatalf("status condition should keep reason and plain message, got %+v", ready)
	}
}
