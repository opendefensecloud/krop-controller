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
	"time"

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	graph "github.com/kubernetes-sigs/kro/pkg/graph"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// blueprintFinalizer guards a blueprint so that its per-export instance-serving
// manager is torn down before the object is removed from the API server.
const blueprintFinalizer = "krop.opendefense.cloud/registrar"

// resyncInterval periodically re-drives a successfully published blueprint so
// Reconcile → OnPublished → Ensure re-triggers. Ensure is idempotent while a
// manager runs and restarts one that crashed (supervisor.forget cleared it), so
// the periodic requeue is the production self-heal re-trigger.
const resyncInterval = 5 * time.Minute

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
	// an instance manager is running for this blueprint's export. changed reports
	// whether the served specHash actually differs from what was last published
	// (true for a new blueprint or a spec edit, false for an unchanged resync), so
	// the wiring can restart the manager only when the compiled graph changed.
	// routing is the resource-id → target map extracted from the blueprint spec
	// (empty targets default to consumer downstream). May be nil.
	OnPublished func(exportName string, instanceGVK schema.GroupVersionKind, g *graph.Graph, routing map[string]kropengine.Target, changed bool)
	// OnDeleted is called during finalizer-based teardown of a deleted blueprint so
	// the supervisor can stop the export's instance manager. May be nil.
	OnDeleted func(exportName string)
}

// Reconcile publishes one blueprint: build (or reuse) its graph, mint the ARS,
// derive permissionClaims, upsert the APIExport, notify the supervisor, and write
// status back.
func (r *Registrar) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	bp := &kropv1alpha1.ResourceGraphDefinition{}
	if err := r.Client.Get(ctx, req.NamespacedName, bp); err != nil {
		// NotFound means the blueprint is fully gone: the finalizer pass below
		// already tore the manager down, so there is nothing left to do here.
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: the object still exists (with a DeletionTimestamp) while our
	// finalizer is present. Tear the instance manager down, then release the
	// finalizer so the API server can remove the object.
	if !bp.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(bp, blueprintFinalizer) {
			// The blueprint IS the offering: withdrawing it withdraws the published
			// API. Cascade-delete the APIExport and its APIResourceSchema(s) from the
			// provider workspace before releasing the finalizer (consumers still bound
			// will lose the type — expected on blueprint withdrawal). Do this BEFORE
			// dropping the finalizer so a failed delete requeues with the object (and
			// our finalizer) still present. Deleting a blueprint with live instances
			// is a provider decision: the instances are the consumers' to delete.
			if bp.Status.ExportedAPI != "" {
				if err := DeletePublishedAPI(ctx, r.Client, bp.Status.ExportedAPI); err != nil {
					return reconcile.Result{}, err
				}
			}
			// Purge the compiled-graph cache for this blueprint so its entry does not
			// leak for the process lifetime.
			if r.Cache != nil {
				r.Cache.Delete(r.Workspace, bp.Name)
			}
			// Stop the per-export manager and purge the served-graph registry (the
			// wired OnDeleted does both).
			if r.OnDeleted != nil && bp.Status.ExportedAPI != "" {
				r.OnDeleted(bp.Status.ExportedAPI)
			}
			controllerutil.RemoveFinalizer(bp, blueprintFinalizer)
			if err := r.Client.Update(ctx, bp); err != nil {
				return reconcile.Result{}, fmt.Errorf("removing blueprint finalizer: %w", err)
			}
		}

		return reconcile.Result{}, nil
	}

	// Normal reconcile: ensure the teardown finalizer is present before we
	// publish anything, so a delete that races an in-flight publish still runs
	// the manager teardown.
	if controllerutil.AddFinalizer(bp, blueprintFinalizer) {
		if err := r.Client.Update(ctx, bp); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding blueprint finalizer: %w", err)
		}
	}

	specHash, err := SpecHash(bp.Spec)
	if err != nil {
		return r.fail(ctx, bp, "HashFailed", err)
	}
	// changed is true when the compiled graph we are about to serve differs from
	// what was last published: a NEW blueprint has an empty ObservedSpecHash
	// (changed → first start), a spec EDIT mints a new specHash (changed → restart),
	// and an unchanged 5m resync leaves them equal (not changed → keep serving).
	changed := bp.Status.ObservedSpecHash != specHash
	// Strip the per-resource routing targets out of the wrapper spec: the graph
	// builder gets clean kro types, and routingRaw (resource-id → target string)
	// is threaded to the served blueprint below.
	kroSpec, routingRaw := bp.Spec.ToKro()
	g, ok := r.Cache.Get(r.Workspace, bp.Name, specHash)
	if !ok {
		rgd := &krov1alpha1.ResourceGraphDefinition{Spec: kroSpec}
		rgd.Name = bp.Name
		built, err := r.Source.Build(rgd)
		if err != nil {
			return r.fail(ctx, bp, "BuildFailed", fmt.Errorf("building graph: %w", err))
		}
		g = built
		r.Cache.Put(r.Workspace, bp.Name, specHash, g)
	}

	// Validate + convert routingRaw to typed engine targets. The CRD enum already
	// constrains the field, but validate here too so a hand-edited or migrated
	// blueprint with a bogus target fails the reconcile with a clear message rather
	// than silently mis-routing.
	routing := make(map[string]kropengine.Target, len(routingRaw))
	for id, raw := range routingRaw {
		t, terr := kropengine.ParseTarget(raw)
		if terr != nil {
			return r.fail(ctx, bp, "InvalidTarget", fmt.Errorf("resource %q: %w", id, terr))
		}
		routing[id] = t
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
	// A foreign (non-core) claim with no identityHash would not authorize: the
	// owning APIExport isn't bound in the provider workspace yet. Fail the publish
	// rather than emit a silently-broken claim — the resync retries once the
	// provider binds the foreign export.
	if err := validateClaims(claims); err != nil {
		return r.fail(ctx, bp, "ClaimIdentityUnresolved", err)
	}

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
		r.OnPublished(exportName, instanceGVK, g, routing, changed)
	}

	// Re-Get the published APIExport to observe status.identityHash, which kcp
	// assigns asynchronously after the apply. It may still be empty on this pass
	// (the object was just applied) — that's fine, the 5m resync re-reads it. This
	// is best-effort: a failed re-Get must not fail the reconcile, since the resync
	// retries. Whatever we read (including "") is written to blueprint status.
	var identityHash string
	published := &apisv1alpha2.APIExport{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: exportName}, published); err != nil {
		logf.FromContext(ctx).V(1).Info("re-Get of published APIExport for identityHash failed; resync will retry",
			"apiexport", exportName, "err", err.Error())
	} else {
		identityHash = published.Status.IdentityHash
	}

	bp.Status.ExportedAPI = exportName
	bp.Status.IdentityHash = identityHash
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
	// Periodically re-drive so Reconcile → OnPublished → Ensure re-triggers: the
	// production self-heal path. Ensure is a no-op while the manager runs and
	// restarts it if it crashed (supervisor.forget cleared the entry). SetStatusCondition
	// and a no-diff Status().Update don't bump resourceVersion and the APIExport SSA
	// is a server-side no-op for identical content, so the requeue is the only
	// steady-state churn.
	return reconcile.Result{RequeueAfter: resyncInterval}, nil
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
