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
	"maps"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// ChildID identifies an applied child for prune bookkeeping: the final GVK,
// namespace and name of an object after all decorators (rename + labels) ran.
type ChildID struct {
	GVK             schema.GroupVersionKind
	Namespace, Name string
}

// RecordingApplier wraps an inner Applier and records the identity of each object
// it applies into a caller-owned sink, so the reconciler can compute the just-
// applied set for prune. It must sit as the INNERMOST decorator (wrapping SSA) so
// the object it sees already carries its final, renamed name and merged labels.
type RecordingApplier struct {
	inner   Applier
	applied *[]ChildID
}

// NewRecordingApplier returns a RecordingApplier that appends each applied
// object's ChildID to sink before delegating to inner.
func NewRecordingApplier(inner Applier, sink *[]ChildID) *RecordingApplier {
	return &RecordingApplier{inner: inner, applied: sink}
}

// Apply records obj's final identity, then delegates to inner and returns its
// result. The name is already final at this point (QualifyingApplier renames
// before delegating to its inner), so recording before delegating is correct.
func (r *RecordingApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	*r.applied = append(*r.applied, ChildID{
		GVK:       obj.GroupVersionKind(),
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	})

	return r.inner.Apply(ctx, obj)
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
//
// The write uses the non-deprecated Client.Apply (controller-runtime v0.24)
// adapted for unstructured via client.ApplyConfigurationFromUnstructured, which
// wraps an *unstructured.Unstructured as a runtime.ApplyConfiguration. desired is
// built natively from blueprint templates (not converted from a typed API object),
// so the wrapper's "explicit zero value" caveat does not apply.
//
// The separate Get read-back is LOAD-BEARING and MUST stay: cross-target CEL (M3)
// applies a provider child (spec only) then observes its STATUS here to feed a
// downstream consumer child's ${child.status.x}. The Apply result alone does not
// reflect fields set on the status subresource by other controllers.
func (a *SSAApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	desired := obj.DeepCopy()
	if err := a.c.Apply(ctx, client.ApplyConfigurationFromUnstructured(desired),
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

// LabelingApplier stamps a fixed label set on each child before delegating, so
// children can be enumerated + deleted by label on instance delete (idea.md §11).
type LabelingApplier struct {
	inner  Applier
	labels map[string]string
}

// NewLabelingApplier returns a LabelingApplier over inner.
func NewLabelingApplier(inner Applier, labels map[string]string) *LabelingApplier {
	return &LabelingApplier{inner: inner, labels: labels}
}

// Apply merges the labels onto a copy of obj (preserving existing labels) and delegates.
func (l *LabelingApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	out := obj.DeepCopy()
	merged := out.GetLabels()
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, l.labels)
	out.SetLabels(merged)

	return l.inner.Apply(ctx, out)
}

// OwnerRefApplier stamps an ownerReference to the instance on each child before
// delegating. Used for consumer-target children (same workspace as the instance)
// as a GC backstop: kcp's per-workspace collector reclaims them if the finalizer
// path is ever bypassed (e.g. force-delete). Cross-workspace provider children
// cannot use this (owner refs are workspace-local).
type OwnerRefApplier struct {
	inner Applier
	owner *unstructured.Unstructured
}

// NewOwnerRefApplier returns an OwnerRefApplier owned by instance.
func NewOwnerRefApplier(inner Applier, instance *unstructured.Unstructured) *OwnerRefApplier {
	return &OwnerRefApplier{inner: inner, owner: instance}
}

// Apply sets the instance owner reference on a copy of obj and delegates.
func (o *OwnerRefApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	out := obj.DeepCopy()
	ref := metav1.OwnerReference{
		APIVersion: o.owner.GetAPIVersion(),
		Kind:       o.owner.GetKind(),
		Name:       o.owner.GetName(),
		UID:        o.owner.GetUID(),
	}
	out.SetOwnerReferences([]metav1.OwnerReference{ref})

	return o.inner.Apply(ctx, out)
}
