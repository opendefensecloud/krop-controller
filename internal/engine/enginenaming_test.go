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

// internal/engine/enginenaming_test.go
package engine

import (
	"context"
	"testing"
)

// The engine owns the rename because it is the only layer that knows a desired
// object's RESOURCE ID — appliers receive a bare object. Per-resource naming
// (issue #30) is keyed by that id, exactly as routing already is.
func TestReconcile_NamerReceivesResourceID(t *testing.T) {
	rt := newRuntime(t, newInstance())
	consumer := &fakeApplier{}
	provider := &fakeApplier{}

	seen := map[string]string{} // nodeID -> original name handed to the namer
	e := New()
	e.Namer = func(nodeID, originalName string, _ Target) string {
		seen[nodeID] = originalName

		return "renamed-" + nodeID
	}

	if _, err := e.Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer,
		TargetProvider: provider,
	}, nil, sampleRouting()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, ok := seen["providerRecord"]; !ok {
		t.Fatalf("namer never saw the providerRecord node, saw %v", seen)
	}
	if len(provider.applied) != 1 {
		t.Fatalf("provider applier got %d objects, want 1", len(provider.applied))
	}
	if got := provider.applied[0].GetName(); got != "renamed-providerRecord" {
		t.Errorf("provider child name = %q, want renamed-providerRecord", got)
	}
}

// A nil Namer must leave every name untouched, so blueprints that declare no
// naming keep the behavior (and the names) they have today.
func TestReconcile_NilNamerLeavesNamesUnchanged(t *testing.T) {
	rt := newRuntime(t, newInstance())
	consumer := &fakeApplier{}
	provider := &fakeApplier{}

	if _, err := New().Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer,
		TargetProvider: provider,
	}, nil, sampleRouting()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(provider.applied) != 1 {
		t.Fatalf("provider applier got %d objects, want 1", len(provider.applied))
	}
	if got := provider.applied[0].GetName(); got != "eu-provider-record" {
		t.Errorf("name changed with a nil Namer: %q", got)
	}
}

// The namer is told which target a child is bound for, so the caller can leave
// consumer children alone (they keep their template name) while qualifying the
// provider and host planes.
func TestReconcile_NamerSeesTarget(t *testing.T) {
	rt := newRuntime(t, newInstance())
	consumer := &fakeApplier{}
	provider := &fakeApplier{}

	e := New()
	e.Namer = func(_, originalName string, target Target) string {
		if target == TargetConsumer {
			return originalName
		}

		return "q-" + originalName
	}

	if _, err := e.Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer,
		TargetProvider: provider,
	}, nil, sampleRouting()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := consumer.applied[0].GetName(); got != "eu-cluster-config" {
		t.Errorf("consumer child was renamed: %q", got)
	}
	if got := provider.applied[0].GetName(); got != "q-eu-provider-record" {
		t.Errorf("provider child name = %q, want q-eu-provider-record", got)
	}
}
