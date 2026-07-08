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

// internal/engine/reader_test.go
package engine

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var cmGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

func cm(name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "default"},
	}}
	if labels != nil {
		u.SetLabels(labels)
	}

	return u
}

func TestClientReader_SatisfiesInterface(t *testing.T) {
	var _ Reader = NewClientReader(fake.NewClientBuilder().Build())
}

func TestClientReader_Get_Found(t *testing.T) {
	cl := fake.NewClientBuilder().WithObjects(cm("cfg", nil)).Build()
	r := NewClientReader(cl)

	got, err := r.Get(context.Background(), cmGVK, "default", "cfg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GetName() != "cfg" || got.GetNamespace() != "default" {
		t.Fatalf("got %s/%s, want default/cfg", got.GetNamespace(), got.GetName())
	}
}

func TestClientReader_Get_NotFound(t *testing.T) {
	cl := fake.NewClientBuilder().Build()
	r := NewClientReader(cl)

	_, err := r.Get(context.Background(), cmGVK, "default", "missing")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want IsNotFound error, got %v", err)
	}
}

func TestClientReader_List_BySelector(t *testing.T) {
	cl := fake.NewClientBuilder().WithObjects(
		cm("a", map[string]string{"app": "web"}),
		cm("b", map[string]string{"app": "db"}),
		cm("c", map[string]string{"app": "web"}),
	).Build()
	r := NewClientReader(cl)

	sel := labels.SelectorFromSet(labels.Set{"app": "web"})
	got, err := r.List(context.Background(), cmGVK, "default", sel)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("selector list returned %d, want 2", len(got))
	}
	for _, o := range got {
		if o.GetLabels()["app"] != "web" {
			t.Fatalf("unexpected object in selector list: %s", o.GetName())
		}
	}
}

func TestClientReader_List_NoSelector(t *testing.T) {
	cl := fake.NewClientBuilder().WithObjects(
		cm("a", nil),
		cm("b", nil),
	).Build()
	r := NewClientReader(cl)

	got, err := r.List(context.Background(), cmGVK, "default", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list returned %d, want 2", len(got))
	}
}
