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

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

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
	appliers := map[kropengine.Target]kropengine.Applier{
		kropengine.TargetConsumer: kropengine.NewLabelingApplier(
			kropengine.NewOwnerRefApplier(kropengine.NewSSAApplier(consumerClient), inst), labels),
		kropengine.TargetProvider: kropengine.NewLabelingApplier(
			kropengine.NewQualifyingApplier(kropengine.NewSSAApplier(r.ProviderClient),
				func(orig string) string { return kropengine.ProviderChildName(clusterName, instanceName, orig) }),
			labels),
	}

	res, err := kropengine.New().Reconcile(ctx, rt, appliers)
	if err != nil {
		return res, err
	}

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
