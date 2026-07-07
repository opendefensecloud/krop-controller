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

// internal/engine/apply_test.go
package engine

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeApplier records applied objects and echoes them back as "observed",
// optionally injecting extra status fields to simulate a controller populating
// the object after apply. It is the test double for the engine loop tests.
type fakeApplier struct {
	applied []*unstructured.Unstructured
	// mutate, if set, is called on a deep copy of the applied object to produce
	// the observed object (e.g. to set a status field a readyWhen depends on).
	mutate func(*unstructured.Unstructured)
}

func (f *fakeApplier) Apply(_ context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.applied = append(f.applied, o.DeepCopy())
	observed := o.DeepCopy()
	if f.mutate != nil {
		f.mutate(observed)
	}

	return observed, nil
}

func TestFakeApplier_SatisfiesInterface(t *testing.T) {
	var _ Applier = &fakeApplier{}
	obs, err := (&fakeApplier{}).Apply(context.Background(), obj(nil))
	if err != nil || obs == nil {
		t.Fatalf("apply returned %v,%v", obs, err)
	}
}

func TestSSAApplier_AppliesAndReadsBack(t *testing.T) {
	cl := fake.NewClientBuilder().Build()
	a := NewSSAApplier(cl)

	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "cfg", "namespace": "default"},
		"data":     map[string]any{"region": "eu"},
	}}

	observed, err := a.Apply(context.Background(), cm)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	region, _, _ := unstructured.NestedString(observed.Object, "data", "region")
	if region != "eu" {
		t.Fatalf("read-back data.region = %q, want eu", region)
	}
}

func TestQualifyingApplier_RenamesBeforeDelegating(t *testing.T) {
	inner := &fakeApplier{}
	q := NewQualifyingApplier(inner, func(orig string) string { return "pfx-" + orig })

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "record", "namespace": "default"},
	}}
	if _, err := q.Apply(context.Background(), obj); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(inner.applied) != 1 {
		t.Fatalf("inner not called once: %d", len(inner.applied))
	}
	if got := inner.applied[0].GetName(); got != "pfx-record" {
		t.Fatalf("inner received name %q, want pfx-record", got)
	}
	// original object must not be mutated
	if obj.GetName() != "record" {
		t.Fatalf("caller's object was mutated: name=%q", obj.GetName())
	}
}

func TestLabelingApplier_MergesLabels_NoMutateCaller(t *testing.T) {
	inner := &fakeApplier{}
	a := NewLabelingApplier(inner, map[string]string{"k": "v"})
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "x", "labels": map[string]any{"keep": "me"}},
	}}
	if _, err := a.Apply(context.Background(), obj); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := inner.applied[0].GetLabels()
	if got["k"] != "v" || got["keep"] != "me" {
		t.Fatalf("labels not merged: %v", got)
	}
	if _, ok := obj.GetLabels()["k"]; ok {
		t.Fatal("caller object mutated")
	}
}

func TestOwnerRefApplier_StampsInstanceOwner(t *testing.T) {
	inner := &fakeApplier{}
	owner := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "KubernetesCluster",
		"metadata": map[string]any{"name": "demo", "uid": "uid-9"},
	}}
	a := NewOwnerRefApplier(inner, owner)
	child := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "x"},
	}}
	if _, err := a.Apply(context.Background(), child); err != nil {
		t.Fatalf("apply: %v", err)
	}
	refs := inner.applied[0].GetOwnerReferences()
	if len(refs) != 1 || refs[0].UID != "uid-9" || refs[0].Kind != "KubernetesCluster" || refs[0].Name != "demo" {
		t.Fatalf("owner ref wrong: %+v", refs)
	}
	if len(child.GetOwnerReferences()) != 0 {
		t.Fatal("caller object mutated")
	}
}
