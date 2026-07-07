// internal/engine/engine.go
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
	"errors"
	"fmt"

	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Engine drives kro's runtime for a single instance: it resolves, routes,
// applies and observes each node, then aggregates instance status.
type Engine struct{}

// New returns a stateless Engine.
func New() *Engine { return &Engine{} }

// Result summarizes one reconcile pass.
type Result struct {
	// Ready is true when every included node passed CheckReadiness.
	Ready bool
	// Requeue is true when a node is not yet ready or a dependency is pending.
	Requeue bool
	// Complete is true when the loop processed all non-ignored nodes without
	// early-returning on a pending dependency — safe to prune. It stays false
	// on the GetDesired pending-dependency early return, because a pending pass
	// applies only a prefix of the desired set and pruning then would delete
	// still-desired children that were simply not re-applied this pass.
	Complete bool
}

// Reconcile drives the runtime node-by-node in topological order. For each
// included node it resolves the desired object(s), routes each by its target
// annotation (stripping it), applies via the matching Applier, reads back, and
// feeds the observed state to the runtime so later nodes' CEL resolves. It never
// rolls back: partial application is reported via Result and a requeue.
func (e *Engine) Reconcile(ctx context.Context, rt *runtime.Runtime, appliers map[Target]Applier) (Result, error) {
	res := Result{Ready: true}

	for _, node := range rt.Nodes() {
		ignored, err := node.IsIgnored()
		if err != nil {
			return res, fmt.Errorf("node %s: includeWhen: %w", node.Spec.Meta.ID, err)
		}
		if ignored {
			continue
		}

		desired, err := node.GetDesired()
		if err != nil {
			// kro distinguishes a pending dependency (CEL couldn't resolve because
			// a referenced status/field isn't observed yet, wrapped with the
			// exported runtime.ErrDataPending sentinel) from a genuine expression
			// bug (type error, bad overload, division by zero). Only the former is
			// normal convergence flow; the latter must surface as a hard error so a
			// broken blueprint doesn't hot-loop every requeue with nothing reported.
			if errors.Is(err, runtime.ErrDataPending) {
				// A dependency is not yet observed/ready → don't apply partial;
				// converge on a later requeue. Complete stays false so the caller
				// does not prune the still-desired children this pass skipped.
				res.Ready = false
				res.Requeue = true

				//nolint:nilerr // unresolved dependency is normal convergence: signal NotReady+Requeue, not a hard error.
				return res, nil
			}

			// Genuine CEL/type error: return it so the failure is surfaced (logged,
			// counted) instead of silently requeueing forever. A hard error returns
			// before res.Complete is set — intentional; nothing should prune.
			return res, fmt.Errorf("node %s: %w", node.Spec.Meta.ID, err)
		}

		observed := make([]*unstructured.Unstructured, 0, len(desired))
		for _, obj := range desired {
			target, err := TargetOf(obj)
			if err != nil {
				return res, fmt.Errorf("node %s: %w", node.Spec.Meta.ID, err)
			}
			StripRouting(obj)
			applier, ok := appliers[target]
			if !ok {
				return res, fmt.Errorf("node %s: no applier configured for target %q", node.Spec.Meta.ID, target)
			}
			obs, err := applier.Apply(ctx, obj)
			if err != nil {
				return res, fmt.Errorf("node %s: apply to %s: %w", node.Spec.Meta.ID, target, err)
			}
			observed = append(observed, obs)
		}
		node.SetObserved(observed)

		if err := node.CheckReadiness(); err != nil {
			// Not an error: the child exists but isn't ready yet.
			res.Ready = false
			res.Requeue = true
		}
	}

	// Every non-ignored node was resolved and applied without a pending-dependency
	// early return: the applied set is the complete desired set, so prune is safe.
	res.Complete = true

	return res, nil
}
