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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
	"go.opendefense.cloud/krop-controller/internal/testlog"
)

func TestInstanceLogConstructor_NilRequest(t *testing.T) {
	lg, sink := testlog.New(0)
	InstanceLogConstructor(lg, "bucket", "buckets.storage.x", "Bucket")(nil).Info("x")

	lines := sink.Find("x")
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	v := lines[0].Values
	if v["blueprint"] != "bucket" || v["export"] != "buckets.storage.x" || v["kind"] != "Bucket" {
		t.Fatalf("blueprint fields missing on a nil request: %+v", v)
	}
	if _, ok := v["name"]; ok {
		t.Fatalf("nil request must not carry a name: %+v", v)
	}
}

func TestInstanceLogConstructor_FieldsFromRequest(t *testing.T) {
	for _, tc := range []struct{ desc, namespace string }{
		{"namespaced", "ns"},
		{"cluster-scoped", ""},
	} {
		lg, sink := testlog.New(0)
		req := &mcreconcile.Request{
			Namespace: tc.namespace, Name: "b1",
			ClusterName: "c1",
		}
		InstanceLogConstructor(lg, "bucket", "buckets.storage.x", "Bucket")(req).Info("x")

		v := sink.Find("x")[0].Values
		ns, hasNS := v["namespace"]
		if v["cluster"] != "c1" || v["name"] != "b1" || !hasNS || ns != tc.namespace {
			t.Fatalf("%s: request fields wrong: %+v", tc.desc, v)
		}
	}
}

// passEndReconciler is the conditions-test reconciler with routing overridable.
func passEndReconciler(t *testing.T, routing map[string]kropengine.Target) (*Reconciler, client.Client) {
	t.Helper()
	inst := mkInstance("logs", false, kropengine.Finalizer)
	_ = unstructured.SetNestedField(inst.Object, "eu", "spec", "region")
	consumer := fake.NewClientBuilder().WithObjects(inst).WithStatusSubresource(inst).Build()

	return &Reconciler{
		Graph:          buildConditionsGraph(t),
		ProviderClient: fake.NewClientBuilder().Build(),
		InstanceGVK:    testGVK,
		BlueprintName:  "bp",
		Routing:        routing,
	}, consumer
}

func TestReconcile_PassEndLine_Complete(t *testing.T) {
	routing := map[string]kropengine.Target{"config": kropengine.TargetConsumer, "providerRecord": kropengine.TargetProvider}

	for _, tc := range []struct {
		verbosity, want int
	}{{1, 1}, {0, 0}} {
		r, consumer := passEndReconciler(t, routing)
		lg, sink := testlog.New(tc.verbosity)
		if _, err := r.Reconcile(log.IntoContext(context.Background(), lg), consumer, "cluster1",
			client.ObjectKey{Namespace: "default", Name: "logs"}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		lines := sink.Find("pass ended")
		if len(lines) != tc.want {
			t.Fatalf("verbosity %d: want %d pass ended lines, got %d: %s", tc.verbosity, tc.want, len(lines), sink.Raw())
		}
		if tc.want == 0 {
			continue
		}
		v := lines[0].Values
		if lines[0].Level != 1 || v["outcome"] != "Complete" || v["ready"] != true || v["instanceUID"] != "uid-1" {
			t.Fatalf("pass ended line wrong: level=%d %+v", lines[0].Level, v)
		}
	}
}

func TestReconcile_PassEndLine_OnError(t *testing.T) {
	// No host client: routing config to host leaves it without an applier → engine error.
	r, consumer := passEndReconciler(t, map[string]kropengine.Target{
		"config": kropengine.TargetHost, "providerRecord": kropengine.TargetProvider,
	})
	lg, sink := testlog.New(1)
	_, err := r.Reconcile(log.IntoContext(context.Background(), lg), consumer, "cluster1",
		client.ObjectKey{Namespace: "default", Name: "logs"})
	if err == nil {
		t.Fatal("want an error for a target without an applier")
	}
	lines := sink.Find("pass ended")
	if len(lines) != 1 {
		t.Fatalf("want 1 pass ended line on error, got %d: %s", len(lines), sink.Raw())
	}
	v := lines[0].Values
	if v["outcome"] != "Failed" || v["node"] != "config" || v["reason"] == "" {
		t.Fatalf("pass ended line on error wrong: %+v", v)
	}
}
