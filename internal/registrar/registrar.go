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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	graph "github.com/kubernetes-sigs/kro/pkg/graph"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// Registrar reconciles blueprints into published APIExports and notifies the
// supervisor to (re)start the instance-serving manager for the export.
type Registrar struct {
	// Client is the provider-workspace client.
	Client client.Client
	// Workspace is the provider workspace name, used as the graph cache key.
	Workspace string
	// Cache holds compiled graphs keyed by (workspace, name, specHash).
	Cache *GraphCache
	// Source builds a compiled graph from a kro RGD against the provider workspace.
	Source *kropengine.EndpointGraphSource
	// OnPublished is called after a successful publish so the supervisor can ensure
	// an instance manager is running for this blueprint's export. May be nil.
	OnPublished func(exportName string, instanceGVK schema.GroupVersionKind, g *graph.Graph)
}

// Reconcile publishes one blueprint: build (or reuse) its graph, mint the ARS,
// derive permissionClaims, upsert the APIExport, notify the supervisor, and write
// status back.
func (r *Registrar) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	bp := &kropv1alpha1.ResourceGraphDefinition{}
	if err := r.Client.Get(ctx, req.NamespacedName, bp); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	specHash := SpecHash(bp.Spec)
	g, ok := r.Cache.Get(r.Workspace, bp.Name, specHash)
	if !ok {
		rgd := &krov1alpha1.ResourceGraphDefinition{Spec: bp.Spec}
		rgd.Name = bp.Name
		built, err := r.Source.Build(rgd)
		if err != nil {
			return r.fail(ctx, bp, "BuildFailed", fmt.Errorf("building graph: %w", err))
		}
		g = built
		r.Cache.Put(r.Workspace, bp.Name, specHash, g)
	}

	instanceGR := g.Instance.Meta.GVR.GroupResource()
	ars, err := BuildARS(g, specHash)
	if err != nil {
		return r.fail(ctx, bp, "BuildFailed", err)
	}
	if err := applyARS(ctx, r.Client, ars); err != nil {
		return r.fail(ctx, bp, "PublishFailed", err)
	}

	identity, err := identityByGroupResource(ctx, r.Client)
	if err != nil {
		return r.fail(ctx, bp, "PublishFailed", err)
	}
	claims := DeriveClaims(ForeignConsumerGRs(g, instanceGR), identity)

	exportName := ars.Spec.Names.Plural + "." + ars.Spec.Group
	if err := UpsertAPIExport(ctx, r.Client, exportName, ars, claims); err != nil {
		return r.fail(ctx, bp, "PublishFailed", err)
	}

	instanceGVK := schema.GroupVersionKind{
		Group:   ars.Spec.Group,
		Version: g.CRD.Spec.Versions[0].Name,
		Kind:    ars.Spec.Names.Kind,
	}
	if r.OnPublished != nil {
		r.OnPublished(exportName, instanceGVK, g)
	}

	bp.Status.ExportedAPI = exportName
	bp.Status.ObservedSpecHash = specHash
	meta.SetStatusCondition(&bp.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "Published",
		Message: fmt.Sprintf("published APIExport %q (specHash %s)", exportName, specHash),
	})
	if err := r.Client.Status().Update(ctx, bp); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating blueprint status: %w", err)
	}
	return reconcile.Result{}, nil
}

// fail records a Ready=False condition (best-effort) and returns the error so the
// reconcile is requeued.
func (r *Registrar) fail(ctx context.Context, bp *kropv1alpha1.ResourceGraphDefinition, reason string, err error) (reconcile.Result, error) {
	meta.SetStatusCondition(&bp.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: err.Error(),
	})
	_ = r.Client.Status().Update(ctx, bp) // best-effort; the returned error requeues.
	return reconcile.Result{}, err
}
