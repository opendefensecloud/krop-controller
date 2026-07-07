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

// internal/engine/route_test.go
package engine

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func obj(annos map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "x"},
	}}
	if annos != nil {
		u.SetAnnotations(annos)
	}
	return u
}

func TestTargetOf_DefaultsToConsumer(t *testing.T) {
	got, err := TargetOf(obj(nil))
	if err != nil || got != TargetConsumer {
		t.Fatalf("want consumer,nil; got %q,%v", got, err)
	}
}

func TestTargetOf_ReadsProvider(t *testing.T) {
	got, err := TargetOf(obj(map[string]string{TargetAnnotation: "provider"}))
	if err != nil || got != TargetProvider {
		t.Fatalf("want provider,nil; got %q,%v", got, err)
	}
}

func TestTargetOf_RejectsUnknown(t *testing.T) {
	if _, err := TargetOf(obj(map[string]string{TargetAnnotation: "bogus"})); err == nil {
		t.Fatal("want error for unknown target value")
	}
}

func TestStripRouting_RemovesAnnotationAndEmptyMap(t *testing.T) {
	u := obj(map[string]string{TargetAnnotation: "provider"})
	StripRouting(u)
	if _, ok := u.GetAnnotations()[TargetAnnotation]; ok {
		t.Fatal("routing annotation not stripped")
	}
	// the annotations map should be gone entirely when it was the only key
	if _, found, _ := unstructured.NestedMap(u.Object, "metadata", "annotations"); found {
		t.Fatal("empty annotations map should be removed")
	}
}

func TestStripRouting_PreservesOtherAnnotations(t *testing.T) {
	u := obj(map[string]string{TargetAnnotation: "consumer", "keep.me/x": "y"})
	StripRouting(u)
	if u.GetAnnotations()["keep.me/x"] != "y" {
		t.Fatal("unrelated annotation was dropped")
	}
}
