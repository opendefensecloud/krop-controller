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

package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

const agentRequestGVKToken = "fulfil.krop.opendefense.cloud/v1alpha1/AgentRequest"

var agentRequestGVK = schema.GroupVersionKind{
	Group: "fulfil.krop.opendefense.cloud", Version: "v1alpha1", Kind: "AgentRequest",
}

// mkRecord builds a liveness-record ConfigMap recording an AgentRequest child.
func mkRecord(name, uid, lastReconciled string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(configMapGVK)
	u.SetNamespace("default")
	u.SetName(name)
	u.SetLabels(map[string]string{
		kropengine.LabelLiveness:    "true",
		kropengine.LabelInstanceUID: uid,
	})
	_ = unstructured.SetNestedStringMap(u.Object, map[string]string{
		"lastReconciled":    lastReconciled,
		"providerChildGVKs": agentRequestGVKToken,
	}, "data")

	return u
}

// mkAgentRequest builds a labeled provider-target child (an AgentRequest).
func mkAgentRequest(name, uid string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(agentRequestGVK)
	u.SetNamespace("default")
	u.SetName(name)
	u.SetLabels(map[string]string{kropengine.LabelInstanceUID: uid})

	return u
}

// exists reports whether the named object of the given GVK is present.
func exists(t *testing.T, cl client.Client, gvk schema.GroupVersionKind, name string) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, u)

	return err == nil
}

func TestSweeper_DeletesOrphanedChildrenAndRecord(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	// Stale: lastReconciled 10 min ago; its AgentRequest must be swept.
	staleRec := mkRecord("krop-live-stale", "uid-1",
		now.Add(-10*time.Minute).Format(time.RFC3339))
	staleChild := mkAgentRequest("child-1", "uid-1")

	// Fresh: lastReconciled now; its AgentRequest must survive.
	freshRec := mkRecord("krop-live-fresh", "uid-2",
		now.Format(time.RFC3339))
	freshChild := mkAgentRequest("child-2", "uid-2")

	cl := fake.NewClientBuilder().
		WithObjects(staleRec, staleChild, freshRec, freshChild).
		Build()

	s := &Sweeper{
		ProviderClient: cl,
		StaleAfter:     5 * time.Minute,
		Clock:          func() time.Time { return now },
	}
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if exists(t, cl, configMapGVK, "krop-live-stale") {
		t.Error("stale liveness record was not deleted")
	}
	if exists(t, cl, agentRequestGVK, "child-1") {
		t.Error("stale orphaned child was not deleted")
	}
	if !exists(t, cl, configMapGVK, "krop-live-fresh") {
		t.Error("fresh liveness record was wrongly deleted")
	}
	if !exists(t, cl, agentRequestGVK, "child-2") {
		t.Error("fresh child was wrongly deleted")
	}
}

func TestSweeper_SkipsFreshAndUnparseable(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	// A record whose timestamp is garbage must be skipped, not deleted.
	garbageRec := mkRecord("krop-live-garbage", "uid-3", "not-a-timestamp")
	garbageChild := mkAgentRequest("child-3", "uid-3")

	// A record with no timestamp at all must also be skipped.
	emptyRec := mkRecord("krop-live-empty", "uid-4", "")

	cl := fake.NewClientBuilder().
		WithObjects(garbageRec, garbageChild, emptyRec).
		Build()

	s := &Sweeper{
		ProviderClient: cl,
		StaleAfter:     5 * time.Minute,
		Clock:          func() time.Time { return now },
	}
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if !exists(t, cl, configMapGVK, "krop-live-garbage") {
		t.Error("record with garbage timestamp was wrongly deleted")
	}
	if !exists(t, cl, agentRequestGVK, "child-3") {
		t.Error("child of unparseable record was wrongly deleted")
	}
	if !exists(t, cl, configMapGVK, "krop-live-empty") {
		t.Error("record with empty timestamp was wrongly deleted")
	}
}
