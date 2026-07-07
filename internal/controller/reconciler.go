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

// Package controller holds the krop instance reconcile glue shared by the
// controller entrypoint and the envtest e2e: one dual-target reconcile path.
package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// defaultRecordNamespace is the provider-workspace namespace holding liveness
// records when Reconciler.RecordNamespace is unset.
const defaultRecordNamespace = "default"

// configMapGVK is the GVK of the liveness record (a plain ConfigMap).
var configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

// Reconciler drives one instance through the engine, applying consumer-target
// children via the per-request consumer client and provider-target children via
// a fixed provider-workspace client with collision-free names.
type Reconciler struct {
	// Graph is the compiled blueprint graph (built once at startup).
	Graph *krograph.Graph
	// ProviderClient writes provider-target children into the provider workspace.
	ProviderClient client.Client
	// InstanceGVK is the generated instance kind this reconciler serves.
	InstanceGVK schema.GroupVersionKind
	// BlueprintName is the blueprint/export identifier stamped as the
	// `blueprint` GC label on every materialized child.
	BlueprintName string
	// RecordNamespace is the provider-workspace namespace where the per-instance
	// liveness record (a ConfigMap) is written. Defaults to "default" when empty.
	RecordNamespace string
}

// recordNamespace returns the configured liveness-record namespace, or the
// "default" fallback.
func (r *Reconciler) recordNamespace() string {
	if r.RecordNamespace != "" {
		return r.RecordNamespace
	}

	return defaultRecordNamespace
}

// Reconcile fetches the instance via consumerClient, drives the engine with both
// appliers, and writes back projected status. clusterName is the consumer's
// logical cluster (used to qualify provider-child names). A missing instance is
// not an error.
func (r *Reconciler) Reconcile(ctx context.Context, consumerClient client.Client, clusterName string, key client.ObjectKey) (kropengine.Result, error) {
	inst := &unstructured.Unstructured{}
	inst.SetGroupVersionKind(r.InstanceGVK)
	if err := consumerClient.Get(ctx, key, inst); err != nil {
		return kropengine.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: run cross-workspace GC, then drop the finalizer.
	if inst.GetDeletionTimestamp() != nil {
		if slices.Contains(inst.GetFinalizers(), kropengine.Finalizer) {
			if err := r.deleteChildren(ctx, consumerClient, string(inst.GetUID())); err != nil {
				return kropengine.Result{}, err
			}
			// A normally-deleted instance must leave no liveness record behind,
			// else the sweeper would eventually re-run child GC against an already
			// GC'd instance (harmless, but noisy) or race the finalizer.
			if err := r.deleteLivenessRecord(ctx, clusterName, string(inst.GetUID())); err != nil {
				return kropengine.Result{}, err
			}
			inst.SetFinalizers(removeString(inst.GetFinalizers(), kropengine.Finalizer))
			if err := consumerClient.Update(ctx, inst); err != nil {
				return kropengine.Result{}, err
			}
		}

		return kropengine.Result{}, nil
	}

	// Ensure the finalizer BEFORE applying any children (grounding rule).
	if !slices.Contains(inst.GetFinalizers(), kropengine.Finalizer) {
		inst.SetFinalizers(append(inst.GetFinalizers(), kropengine.Finalizer))
		if err := consumerClient.Update(ctx, inst); err != nil {
			return kropengine.Result{}, err
		}
		// The Update re-triggers reconcile with the finalizer present; apply then.
		return kropengine.Result{}, nil
	}

	rt, err := kroruntime.FromGraph(r.Graph, inst, krograph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		return kropengine.Result{}, fmt.Errorf("runtime: %w", err)
	}

	instanceName := inst.GetName()
	labels := kropengine.GCLabels(string(inst.GetUID()), clusterName, r.BlueprintName)

	// Per-target sinks record the final identity of every applied child so a
	// complete pass can prune labeled children no longer in the desired set.
	// RecordingApplier is the INNERMOST decorator (wrapping SSA) so it observes
	// the object AFTER QualifyingApplier's rename and LabelingApplier's labels.
	var appliedConsumer, appliedProvider []kropengine.ChildID
	appliers := map[kropengine.Target]kropengine.Applier{
		kropengine.TargetConsumer: kropengine.NewLabelingApplier(
			kropengine.NewOwnerRefApplier(
				kropengine.NewRecordingApplier(kropengine.NewSSAApplier(consumerClient), &appliedConsumer), inst), labels),
		kropengine.TargetProvider: kropengine.NewLabelingApplier(
			kropengine.NewQualifyingApplier(
				kropengine.NewRecordingApplier(kropengine.NewSSAApplier(r.ProviderClient), &appliedProvider),
				func(orig string) string { return kropengine.ProviderChildName(clusterName, instanceName, orig) }),
			labels),
	}

	res, err := kropengine.New().Reconcile(ctx, rt, appliers)
	if err != nil {
		return res, err
	}

	// Refresh the provider-side liveness record on EVERY non-deleting pass that
	// reached the apply loop — INCLUDING an incomplete (pending cross-target
	// dependency) pass. This must be independent of res.Complete: the engine
	// applies provider-target children BEFORE it early-returns on a pending
	// consumer dependency (engine.Reconcile's GetDesired-pending path), so the
	// provider AgentRequest is created immediately while the consumer child pends
	// on ${agentRequest.status.token} for possibly minutes. Gating the record on
	// res.Complete would leave that provider child with NO liveness record during
	// the whole pending window; if the consumer unbinds the APIExport then, the
	// orphan sweeper would have nothing to act on → permanent orphan (idea.md §11).
	// The providerChildGVKs come from the graph, so they are known regardless of
	// completeness. Only prune (below) stays gated on Complete. The record is a
	// single Get+Create/Update upsert (see writeLivenessRecord).
	if err := r.writeLivenessRecord(ctx, clusterName, string(inst.GetUID())); err != nil {
		return res, err
	}

	// Prune ONLY after a complete pass: an incomplete (pending-dependency) pass
	// applies just a prefix of the desired set, so pruning then would delete
	// still-desired children it simply had not re-applied yet. The deletion path
	// (above) already GCs everything, so skip prune when the instance is deleting.
	if res.Complete && inst.GetDeletionTimestamp() == nil {
		if err := r.pruneChildren(ctx, consumerClient, string(inst.GetUID()), map[kropengine.Target][]kropengine.ChildID{
			kropengine.TargetConsumer: appliedConsumer,
			kropengine.TargetProvider: appliedProvider,
		}); err != nil {
			return res, err
		}
	}

	// Request a periodic requeue on every non-deleting apply pass: this ~30s
	// requeue (the caller maps res.Requeue → RequeueAfter(requeueInterval)) is the
	// LIVENESS HEARTBEAT that keeps the record's lastReconciled fresh. StaleAfter
	// on the sweeper must stay >> this interval (see the Sweeper doc comment). An
	// incomplete pass already sets Requeue for convergence; setting it here also
	// covers the fully-ready complete pass so the heartbeat never stops.
	res.Requeue = true

	if desired, perr := kropengine.ProjectStatus(rt); perr == nil {
		if status, found, _ := unstructured.NestedMap(desired.Object, "status"); found {
			_ = unstructured.SetNestedMap(inst.Object, status, "status")
			_ = consumerClient.Status().Update(ctx, inst)
		}
	}

	return res, nil
}

// deleteChildren deletes all children of the instance (by the instance-uid
// label) in both target workspaces, enumerating each target's child GVKs from
// the compiled graph. Consumer children are also owner-ref backstopped.
func (r *Reconciler) deleteChildren(ctx context.Context, consumerClient client.Client, instanceUID string) error {
	sel := client.MatchingLabels{kropengine.LabelInstanceUID: instanceUID}
	for target, cl := range map[kropengine.Target]client.Client{
		kropengine.TargetConsumer: consumerClient,
		kropengine.TargetProvider: r.ProviderClient,
	} {
		for _, gvk := range r.childGVKs(target) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(gvk)
			if err := cl.List(ctx, list, sel); err != nil {
				return fmt.Errorf("listing %s children: %w", gvk.Kind, err)
			}
			for i := range list.Items {
				if err := cl.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
					return fmt.Errorf("deleting %s %s: %w", gvk.Kind, list.Items[i].GetName(), err)
				}
			}
		}
	}

	return nil
}

// pruneChildren deletes instance-labeled children that are NOT in the just-
// applied set, for each target and each of that target's child GVKs. This
// reclaims forEach-shrunk items, includeWhen-excluded children, and (after a
// graph change) children of dropped blueprint nodes. It must run ONLY after a
// complete apply pass (see the res.Complete guard at the call site), else a
// pending pass's partial applied set would delete still-desired children.
func (r *Reconciler) pruneChildren(ctx context.Context, consumerClient client.Client, instanceUID string, applied map[kropengine.Target][]kropengine.ChildID) error {
	sel := client.MatchingLabels{kropengine.LabelInstanceUID: instanceUID}
	for target, cl := range map[kropengine.Target]client.Client{
		kropengine.TargetConsumer: consumerClient,
		kropengine.TargetProvider: r.ProviderClient,
	} {
		keep := make(map[kropengine.ChildID]bool, len(applied[target]))
		for _, id := range applied[target] {
			keep[id] = true
		}
		for _, gvk := range r.childGVKs(target) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(gvk)
			if err := cl.List(ctx, list, sel); err != nil {
				return fmt.Errorf("listing %s children for prune: %w", gvk.Kind, err)
			}
			for i := range list.Items {
				item := &list.Items[i]
				id := kropengine.ChildID{GVK: gvk, Namespace: item.GetNamespace(), Name: item.GetName()}
				if keep[id] {
					continue
				}
				if err := cl.Delete(ctx, item); client.IgnoreNotFound(err) != nil {
					return fmt.Errorf("pruning %s %s: %w", gvk.Kind, item.GetName(), err)
				}
			}
		}
	}

	return nil
}

// childGVKs returns the distinct child GVKs of the given target from the graph.
func (r *Reconciler) childGVKs(target kropengine.Target) []schema.GroupVersionKind {
	seen := map[schema.GroupVersionKind]bool{}
	var out []schema.GroupVersionKind
	for _, node := range r.Graph.Nodes {
		t, err := kropengine.TargetOf(node.Template)
		if err != nil || t != target {
			continue
		}
		gvk := node.Template.GroupVersionKind()
		if !seen[gvk] {
			seen[gvk] = true
			out = append(out, gvk)
		}
	}

	return out
}

// writeLivenessRecord upserts the per-instance liveness ConfigMap in the
// provider workspace. Its labels let the sweeper find it (and the child GVKs to
// delete) without observing the consumer instance; its lastReconciled timestamp
// bounds staleness. See the Sweeper doc comment for the full mechanism.
func (r *Reconciler) writeLivenessRecord(ctx context.Context, clusterName, instanceUID string) error {
	labels := map[string]string{
		kropengine.LabelInstanceUID:     instanceUID,
		kropengine.LabelConsumerCluster: clusterName,
		kropengine.LabelBlueprint:       r.BlueprintName,
		kropengine.LabelLiveness:        "true",
	}
	data := map[string]string{
		"lastReconciled":    time.Now().UTC().Format(time.RFC3339),
		"providerChildGVKs": r.providerChildGVKString(),
	}

	key := client.ObjectKey{Namespace: r.recordNamespace(), Name: kropengine.LivenessRecordName(clusterName, instanceUID)}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(configMapGVK)
	err := r.ProviderClient.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(configMapGVK)
		cm.SetNamespace(key.Namespace)
		cm.SetName(key.Name)
		cm.SetLabels(labels)
		if serr := unstructured.SetNestedStringMap(cm.Object, data, "data"); serr != nil {
			return fmt.Errorf("building liveness record: %w", serr)
		}
		if cerr := r.ProviderClient.Create(ctx, cm); cerr != nil {
			return fmt.Errorf("creating liveness record %s: %w", key.Name, cerr)
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("getting liveness record %s: %w", key.Name, err)
	}

	existing.SetLabels(labels)
	if serr := unstructured.SetNestedStringMap(existing.Object, data, "data"); serr != nil {
		return fmt.Errorf("updating liveness record: %w", serr)
	}
	if uerr := r.ProviderClient.Update(ctx, existing); uerr != nil {
		return fmt.Errorf("updating liveness record %s: %w", key.Name, uerr)
	}

	return nil
}

// deleteLivenessRecord removes the per-instance liveness ConfigMap. A missing
// record is not an error (the reconciler may never have written one).
func (r *Reconciler) deleteLivenessRecord(ctx context.Context, clusterName, instanceUID string) error {
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(configMapGVK)
	cm.SetNamespace(r.recordNamespace())
	cm.SetName(kropengine.LivenessRecordName(clusterName, instanceUID))
	if err := r.ProviderClient.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting liveness record %s: %w", cm.GetName(), err)
	}

	return nil
}

// providerChildGVKString serializes the provider-target child GVKs as a
// comma-joined list of "group/version/Kind" tokens, so the sweeper knows what to
// delete for an orphaned instance without re-deriving the graph. The core group
// serializes with an empty first segment (e.g. "/v1/ConfigMap").
func (r *Reconciler) providerChildGVKString() string {
	gvks := r.childGVKs(kropengine.TargetProvider)
	tokens := make([]string, 0, len(gvks))
	for _, gvk := range gvks {
		tokens = append(tokens, gvk.Group+"/"+gvk.Version+"/"+gvk.Kind)
	}

	return strings.Join(tokens, ",")
}

// removeString returns s without any occurrence of v.
func removeString(s []string, v string) []string {
	out := s[:0:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}

	return out
}
