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

// internal/engine/collection_test.go
package engine

import (
	"context"
	"slices"
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const collectionTestCluster = "root-consumer"

// collectionRGD: a host-target forEach collection "addrs" (one ConfigMap per
// spec.names entry, ready only once each item's data.ready is "true") and a
// host-target VPC "consumer" whose spec.cidrBlocks lists the addrs' names. The
// instance status projects the collection size. Mirrors the KCC pattern where a
// ComputeInstance's natIpRef.name must reference each ComputeAddress by the name
// it actually has on the target.
func collectionRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"addrset",
		generator.WithSchema("AddrSet", "v1alpha1",
			map[string]any{"names": "[]string"},
			map[string]any{"addrCount": "${size(addrs)}"},
		),
		generator.WithResourceCollection("addrs", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name":      "${name}-addr",
				"namespace": "default",
			},
			"data": map[string]any{"name": "${name}"},
		}, []krov1alpha1.ForEachDimension{{"name": "${schema.spec.names}"}},
			[]string{"${each.data.ready == 'true'}"}, nil),
		generator.WithResource("consumer", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "VPC",
			"metadata": map[string]any{"name": "addr-consumer"},
			"spec":     map[string]any{"cidrBlocks": "${addrs.map(a, a.metadata.name)}"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

func collectionRuntime(t *testing.T, names ...string) *runtime.Runtime {
	t.Helper()
	g := buildTestGraph(t, collectionRGD())
	items := make([]any, 0, len(names))
	for _, n := range names {
		items = append(items, n)
	}
	inst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "AddrSet",
		"metadata": map[string]any{"name": "demo", "namespace": "default"},
		"spec":     map[string]any{"names": items},
	}}
	rt, err := runtime.FromGraph(g, graph.Config{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000}, runtime.WithInstance(inst))
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}

	return rt
}

// hostApplier models a qualified (provider/host) target: every child lands on
// the target under its ProviderChildName and comes back ready. The rename is
// done in the fake's result rather than via QualifyingApplier so the test pins
// the engine contract (observed identity differs from the template's) however
// the rename is wired. A RecordingApplier on top captures prune bookkeeping.
func hostApplier(sink *[]ChildID) (Applier, *fakeApplier) {
	ssa := &fakeApplier{mutate: func(o *unstructured.Unstructured) {
		o.SetName(ProviderChildName(collectionTestCluster, "demo", o.GetName()))
		_ = unstructured.SetNestedField(o.Object, "true", "data", "ready")
	}}

	return NewRecordingApplier(ssa, sink), ssa
}

func reconcileCollection(t *testing.T, names ...string) (Result, *runtime.Runtime, *fakeApplier, []ChildID) {
	t.Helper()
	rt := collectionRuntime(t, names...)
	var sink []ChildID
	host, ssa := hostApplier(&sink)
	res, err := New().Reconcile(context.Background(), rt, map[Target]Applier{TargetHost: host}, nil,
		map[string]Target{"addrs": TargetHost, "consumer": TargetHost})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	return res, rt, ssa, sink
}

// appliedVPC returns the single VPC the fake SSA applier received, or fails.
func appliedVPC(t *testing.T, ssa *fakeApplier) *unstructured.Unstructured {
	t.Helper()
	var vpcs []*unstructured.Unstructured
	for _, o := range ssa.applied {
		if o.GetKind() == "VPC" {
			vpcs = append(vpcs, o)
		}
	}
	if len(vpcs) != 1 {
		t.Fatalf("applied %d VPCs, want 1 (dependent of the collection never resolved?)", len(vpcs))
	}

	return vpcs[0]
}

// A renamed (qualified) collection item must still be observed: the collection
// becomes ready per item, and a dependent reading the items' metadata.name sees
// the name the object REALLY has on its target, not the template name.
func TestReconcile_RenamedCollectionItemsAreObserved(t *testing.T) {
	res, rt, ssa, sink := reconcileCollection(t, "a")

	want := []any{ProviderChildName(collectionTestCluster, "demo", "a-addr")}
	got, _, _ := unstructured.NestedSlice(appliedVPC(t, ssa).Object, "spec", "cidrBlocks")
	if !slices.Equal(got, want) {
		t.Fatalf("dependent spec.cidrBlocks = %v, want the qualified item name %v", got, want)
	}
	if !res.Ready || !res.Complete || res.Requeue {
		t.Fatalf("want Ready+Complete without requeue, got %+v", res)
	}

	di, err := ProjectStatus(rt)
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if n, _, _ := unstructured.NestedInt64(di.Object, "status", "addrCount"); n != 1 {
		t.Fatalf("status.addrCount = %d, want 1", n)
	}

	// Prune bookkeeping records the qualified identities that actually exist.
	wantIDs := []string{ProviderChildName(collectionTestCluster, "demo", "a-addr"),
		ProviderChildName(collectionTestCluster, "demo", "addr-consumer")}
	gotIDs := make([]string, 0, len(sink))
	for _, id := range sink {
		gotIDs = append(gotIDs, id.Name)
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("recorded children = %v, want %v", gotIDs, wantIDs)
	}
}

// Observed items follow the forEach (desired) order, not lexical name order.
func TestReconcile_RenamedCollectionKeepsDesiredOrder(t *testing.T) {
	_, _, ssa, _ := reconcileCollection(t, "c", "a", "b")

	want := []any{
		ProviderChildName(collectionTestCluster, "demo", "c-addr"),
		ProviderChildName(collectionTestCluster, "demo", "a-addr"),
		ProviderChildName(collectionTestCluster, "demo", "b-addr"),
	}
	got, _, _ := unstructured.NestedSlice(appliedVPC(t, ssa).Object, "spec", "cidrBlocks")
	if !slices.Equal(got, want) {
		t.Fatalf("dependent spec.cidrBlocks = %v, want %v (forEach order)", got, want)
	}
}

// A forEach over an empty list is trivially ready and its dependent resolves to
// an empty list.
func TestReconcile_EmptyCollectionStillResolves(t *testing.T) {
	res, rt, ssa, _ := reconcileCollection(t)

	got, _, _ := unstructured.NestedSlice(appliedVPC(t, ssa).Object, "spec", "cidrBlocks")
	if len(got) != 0 {
		t.Fatalf("dependent spec.cidrBlocks = %v, want empty", got)
	}
	if !res.Ready || !res.Complete {
		t.Fatalf("want Ready+Complete, got %+v", res)
	}
	di, err := ProjectStatus(rt)
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if n, found, _ := unstructured.NestedInt64(di.Object, "status", "addrCount"); !found || n != 0 {
		t.Fatalf("status.addrCount = %d (found=%v), want 0", n, found)
	}
}
