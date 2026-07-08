// internal/engine/workspace_test.go
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
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestStampConsumerCluster(t *testing.T) {
	// nil annotations map: the stamp initializes it.
	u := &unstructured.Unstructured{Object: map[string]any{}}
	StampConsumerCluster(u, "cluster-xyz")
	if got := u.GetAnnotations()[AnnotationConsumerCluster]; got != "cluster-xyz" {
		t.Fatalf("annotation = %q, want cluster-xyz", got)
	}
	// existing annotations are preserved.
	u.SetAnnotations(map[string]string{"keep": "me"})
	StampConsumerCluster(u, "cluster-2")
	ann := u.GetAnnotations()
	if ann["keep"] != "me" || ann[AnnotationConsumerCluster] != "cluster-2" {
		t.Fatalf("annotations = %v, want keep=me + cluster=cluster-2", ann)
	}
}

// workspaceRGD names a consumer ConfigMap by prefixing the consumer-cluster
// annotation — the collision-avoidance pattern blueprints use for host/provider
// children. ConfigMap is in the fake resolver's core scheme, so it builds cluster-free.
func workspaceRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"wsprefix",
		generator.WithSchema("WsPrefix", "v1alpha1",
			map[string]any{"name": "string"},
			map[string]any{"childName": "${cfg.metadata.name}"},
		),
		generator.WithResource("cfg", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				// prepend the consumer workspace (logical-cluster name) for collision-free naming.
				"name":      `${schema.metadata.annotations["krop.opendefense.cloud/consumer-cluster"]}-${schema.spec.name}`,
				"namespace": "default",
			},
			"data": map[string]any{"name": "${schema.spec.name}"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

// The consumer-cluster annotation stamped on the instance resolves through kro CEL
// (schema.metadata.annotations[...]) so a child name can be prefixed with the unique
// consumer workspace identity.
func TestReconcile_ConsumerClusterAnnotation_ResolvesInCEL(t *testing.T) {
	g := buildTestGraph(t, workspaceRGD())
	inst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "WsPrefix",
		"metadata": map[string]any{"name": "demo", "namespace": "default"},
		"spec":     map[string]any{"name": "db"},
	}}
	// The reconciler stamps this before FromGraph; mirror that here.
	StampConsumerCluster(inst, "kvdk8299mah3yj1p")

	rt, err := runtime.FromGraph(g, inst, graph.RGDConfig{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000})
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}

	consumer := &fakeApplier{}
	res, err := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetConsumer: consumer}, nil, nil)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 1 {
		t.Fatalf("applied %d objects, want 1", len(consumer.applied))
	}
	if got := consumer.applied[0].GetName(); got != "kvdk8299mah3yj1p-db" {
		t.Fatalf("child name = %q, want kvdk8299mah3yj1p-db (consumer-cluster annotation not resolved in CEL)", got)
	}
	if !res.Ready || !res.Complete {
		t.Fatalf("want Ready+Complete, got %+v", res)
	}
}
