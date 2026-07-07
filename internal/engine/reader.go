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

// internal/engine/reader.go — read side of engine I/O, mirroring Applier.
package engine

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reader reads existing objects a blueprint references via externalRef but does
// not create. It is the read counterpart of Applier: the engine drives all
// read I/O through it, keeping the reconcile loop client-agnostic and testable.
type Reader interface {
	// Get fetches one object by name (and namespace, empty for cluster-scoped).
	Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error)
	// List returns objects of gvk in namespace (empty ⇒ all namespaces) matching selector.
	List(ctx context.Context, gvk schema.GroupVersionKind, namespace string, selector labels.Selector) ([]*unstructured.Unstructured, error)
}

// ClientReader implements Reader over one workspace/cluster-scoped client.Client.
type ClientReader struct{ c client.Client }

// NewClientReader builds a ClientReader bound to one client.
func NewClientReader(c client.Client) *ClientReader { return &ClientReader{c: c} }

func (r *ClientReader) Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := r.c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, u); err != nil {
		return nil, err
	}

	return u, nil
}

func (r *ClientReader) List(ctx context.Context, gvk schema.GroupVersionKind, namespace string, selector labels.Selector) ([]*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if selector != nil {
		opts = append(opts, client.MatchingLabelsSelector{Selector: selector})
	}
	if err := r.c.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		out[i] = &list.Items[i]
	}

	return out, nil
}
