// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"slices"
	"testing"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
	"go.opendefense.cloud/krop-controller/internal/testlog"
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
	// Consumer is the default target (nil Routing → TargetForNode returns consumer).
	return &krograph.Graph{Nodes: map[string]*krograph.Node{
		"config": {Template: tmpl},
	}}
}

func TestPruneChildren_DeletesStaleKeepsApplied(t *testing.T) {
	const uid = "uid-1"
	desired := mkLabeledCM("desired-child", uid)
	stale := mkLabeledCM("stale-child", uid)
	stale2 := mkLabeledCM("stale-child-2", uid)
	consumer := fake.NewClientBuilder().WithObjects(desired, stale, stale2).Build()

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
	lg, sink := testlog.New(0)
	if err := r.pruneChildren(log.IntoContext(context.Background(), lg), consumer, uid, applied); err != nil {
		t.Fatalf("pruneChildren: %v", err)
	}

	// krop-initiated deletes are logged at the default level as one line per target
	// (so log sampling cannot drop them in bursts); kept children are not listed.
	pruned := sink.Find("pruned children")
	if len(pruned) != 1 {
		t.Fatalf("want 1 pruned children line, got %d: %s", len(pruned), sink.Raw())
	}
	if v := pruned[0].Values; pruned[0].Level != 0 || v["target"] != "consumer" ||
		!slices.Equal(childrenOf(pruned[0]), []string{"/v1/ConfigMap default/stale-child", "/v1/ConfigMap default/stale-child-2"}) {
		t.Fatalf("pruned children line wrong: level=%d %+v", pruned[0].Level, v)
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

// A child that is already terminating (e.g. held by its own finalizer) stays in
// the List and is deleted again on every complete pass; only the first deletion is
// news, so repeats must not be logged at the default level.
func TestPruneChildren_TerminatingChildNotLogged(t *testing.T) {
	const uid = "uid-1"
	terminating := mkLabeledCM("terminating-child", uid)
	terminating.SetFinalizers([]string{"example.com/hold"})
	now := metav1.Now()
	terminating.SetDeletionTimestamp(&now)
	consumer := fake.NewClientBuilder().WithObjects(terminating).Build()
	r := &Reconciler{Graph: graphWithConfigMapNode(), ProviderClient: fake.NewClientBuilder().Build(), InstanceGVK: testGVK, BlueprintName: "bp"}

	lg, sink := testlog.New(0)
	if err := r.pruneChildren(log.IntoContext(context.Background(), lg), consumer, uid, nil); err != nil {
		t.Fatalf("pruneChildren: %v", err)
	}
	if n := len(sink.Find("pruned children")); n != 0 {
		t.Fatalf("want no pruned children line for an already-terminating child, got %d: %s", n, sink.Raw())
	}
}

// A child that vanished between List and Delete was not deleted by krop, so it
// must not be logged as a deletion.
func TestPruneChildren_NotFoundNotLogged(t *testing.T) {
	const uid = "uid-2"
	consumer := fake.NewClientBuilder().WithObjects(mkLabeledCM("gone-child", uid)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
			},
		}).Build()
	r := &Reconciler{Graph: graphWithConfigMapNode(), ProviderClient: fake.NewClientBuilder().Build(), InstanceGVK: testGVK, BlueprintName: "bp"}

	lg, sink := testlog.New(0)
	if err := r.pruneChildren(log.IntoContext(context.Background(), lg), consumer, uid, nil); err != nil {
		t.Fatalf("pruneChildren: %v", err)
	}
	if n := len(sink.Find("pruned children")); n != 0 {
		t.Fatalf("want no pruned children line when Delete returns NotFound, got %d: %s", n, sink.Raw())
	}
}

func TestDeleteChildren_LogsDeletions(t *testing.T) {
	const uid = "uid-1"
	consumer := fake.NewClientBuilder().WithObjects(mkLabeledCM("child", uid)).Build()
	r := &Reconciler{
		Graph:          graphWithConfigMapNode(),
		ProviderClient: fake.NewClientBuilder().Build(),
		InstanceGVK:    testGVK,
		BlueprintName:  "bp",
	}

	lg, sink := testlog.New(0)
	if err := r.deleteChildren(log.IntoContext(context.Background(), lg), consumer, uid); err != nil {
		t.Fatalf("deleteChildren: %v", err)
	}
	deleted := sink.Find("deleted children")
	if len(deleted) != 1 {
		t.Fatalf("want 1 deleted children line, got %d: %s", len(deleted), sink.Raw())
	}
	if v := deleted[0].Values; deleted[0].Level != 0 || v["target"] != "consumer" ||
		!slices.Equal(childrenOf(deleted[0]), []string{"/v1/ConfigMap default/child"}) {
		t.Fatalf("deleted children line wrong: level=%d %+v", deleted[0].Level, v)
	}
}

// A failed delete aborts the pass, but the children deleted before it are still
// logged: they are gone, and the retry will not list them again.
func TestDeleteChildren_LogsDeletionsBeforeError(t *testing.T) {
	const uid = "uid-1"
	consumer := fake.NewClientBuilder().WithObjects(mkLabeledCM("a-child", uid), mkLabeledCM("b-child", uid)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == "b-child" {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), nil)
				}

				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &Reconciler{Graph: graphWithConfigMapNode(), ProviderClient: fake.NewClientBuilder().Build(), InstanceGVK: testGVK, BlueprintName: "bp"}

	lg, sink := testlog.New(0)
	if err := r.deleteChildren(log.IntoContext(context.Background(), lg), consumer, uid); err == nil {
		t.Fatal("deleteChildren: want the forbidden delete to fail the pass")
	}
	deleted := sink.Find("deleted children")
	if len(deleted) != 1 || !slices.Equal(childrenOf(deleted[0]), []string{"/v1/ConfigMap default/a-child"}) {
		t.Fatalf("want one deleted children line listing a-child, got: %s", sink.Raw())
	}
}

// childrenOf returns a captured line's children field as strings.
func childrenOf(l testlog.Line) []string {
	raw, _ := l.Values["children"].([]any)
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		s, _ := c.(string)
		out = append(out, s)
	}

	return out
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
