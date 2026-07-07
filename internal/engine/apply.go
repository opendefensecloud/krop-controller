// internal/engine/apply.go
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

package engine

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FieldManager is the server-side-apply field owner for all engine writes.
const FieldManager = "krop-controller"

// Applier applies one desired object into a single target workspace and returns
// the object as observed afterwards (server-side apply result, read back). The
// engine owns all I/O through this interface, keeping the reconcile loop
// client-agnostic and unit-testable.
type Applier interface {
	Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

// SSAApplier applies objects into one workspace via server-side apply using a
// controller-runtime client, then reads the object back so callers observe the
// full server state (including fields other controllers set).
type SSAApplier struct {
	c client.Client
}

// NewSSAApplier builds an SSAApplier bound to one workspace-scoped client.
func NewSSAApplier(c client.Client) *SSAApplier {
	return &SSAApplier{c: c}
}

// Apply server-side-applies obj (force ownership) and returns it as read back.
func (a *SSAApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	desired := obj.DeepCopy()
	if err := a.c.Patch(ctx, desired, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return nil, err
	}
	observed := &unstructured.Unstructured{}
	observed.SetGroupVersionKind(obj.GroupVersionKind())
	if err := a.c.Get(ctx, client.ObjectKeyFromObject(desired), observed); err != nil {
		return nil, err
	}
	return observed, nil
}

// QualifyingApplier wraps an Applier and rewrites metadata.name via a rename
// function before delegating. Used to give provider-target children collision-
// free names (see ProviderChildName) without changing the engine's routing loop.
type QualifyingApplier struct {
	inner  Applier
	rename func(original string) string
}

// NewQualifyingApplier returns a QualifyingApplier over inner.
func NewQualifyingApplier(inner Applier, rename func(original string) string) *QualifyingApplier {
	return &QualifyingApplier{inner: inner, rename: rename}
}

// Apply renames a copy of obj (metadata.name → rename(name)) and delegates.
func (q *QualifyingApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	renamed := obj.DeepCopy()
	renamed.SetName(q.rename(obj.GetName()))
	return q.inner.Apply(ctx, renamed)
}
