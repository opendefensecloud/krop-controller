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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krograph "github.com/kubernetes-sigs/kro/pkg/graph"
	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"

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

	rt, err := kroruntime.FromGraph(r.Graph, inst, krograph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		return kropengine.Result{}, fmt.Errorf("runtime: %w", err)
	}

	instanceName := inst.GetName()
	appliers := map[kropengine.Target]kropengine.Applier{
		kropengine.TargetConsumer: kropengine.NewSSAApplier(consumerClient),
		kropengine.TargetProvider: kropengine.NewQualifyingApplier(
			kropengine.NewSSAApplier(r.ProviderClient),
			func(orig string) string { return kropengine.ProviderChildName(clusterName, instanceName, orig) },
		),
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
