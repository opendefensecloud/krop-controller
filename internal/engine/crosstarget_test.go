// internal/engine/crosstarget_test.go
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

// crossTargetRGD: provider-target vpc (VPC.status.vpcID is a real schema field in
// the fake resolver) and consumer-target subnet reading ${vpc.status.vpcID}.
func crossTargetRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"xtarget",
		generator.WithSchema("XTarget", "v1alpha1",
			map[string]any{"region": "string"},
			map[string]any{"vpcID": "${vpc.status.vpcID}"},
		),
		generator.WithResource("vpc", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "VPC",
			"metadata": map[string]any{
				"name": "${schema.spec.region}-vpc",
			},
			"spec": map[string]any{"cidrBlocks": []any{"10.0.0.0/16"}},
		}, []string{"${vpc.status.vpcID != ''}"}, nil),
		generator.WithResource("subnet", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "Subnet",
			"metadata": map[string]any{
				"name": "${schema.spec.region}-subnet",
			},
			"spec": map[string]any{"vpcID": "${vpc.status.vpcID}", "cidrBlock": "10.0.1.0/24"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

// crossTargetRouting routes the cross-target RGD: vpc → provider, subnet → consumer.
func crossTargetRouting() map[string]Target {
	return map[string]Target{"vpc": TargetProvider, "subnet": TargetConsumer}
}

func crossTargetRuntime(t *testing.T) *runtime.Runtime {
	t.Helper()
	g := buildTestGraph(t, crossTargetRGD())
	inst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "XTarget",
		"metadata": map[string]any{"name": "demo", "namespace": "default"},
		"spec":     map[string]any{"region": "eu"},
	}}
	rt, err := runtime.FromGraph(g, inst, graph.RGDConfig{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000})
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}

	return rt
}

// Provider status present → consumer resolves it, instance status projects.
func TestReconcile_CrossTargetCEL_ResolvesFromProviderStatus(t *testing.T) {
	rt := crossTargetRuntime(t)
	consumer := &fakeApplier{}
	provider := &fakeApplier{mutate: func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, "vpc-abc123", "status", "vpcID")
	}}

	res, err := New().Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer, TargetProvider: provider,
	}, nil, crossTargetRouting())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 1 {
		t.Fatalf("consumer applied %d, want 1", len(consumer.applied))
	}
	if got, _, _ := unstructured.NestedString(consumer.applied[0].Object, "spec", "vpcID"); got != "vpc-abc123" {
		t.Fatalf("cross-target CEL: subnet.spec.vpcID = %q, want vpc-abc123", got)
	}
	di, err := ProjectStatus(rt)
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if got, _, _ := unstructured.NestedString(di.Object, "status", "vpcID"); got != "vpc-abc123" {
		t.Fatalf("instance status.vpcID = %q, want vpc-abc123", got)
	}
	if !res.Ready {
		t.Fatalf("want Ready, got %+v", res)
	}
}

// Provider status absent → consumer child NOT created (no partial write), requeue.
func TestReconcile_CrossTargetPendsUntilProviderReady(t *testing.T) {
	rt := crossTargetRuntime(t)
	consumer := &fakeApplier{}
	provider := &fakeApplier{} // no status injected

	res, err := New().Reconcile(context.Background(), rt, map[Target]Applier{
		TargetConsumer: consumer, TargetProvider: provider,
	}, nil, crossTargetRouting())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(consumer.applied) != 0 {
		t.Fatalf("partial write: consumer applied %d, want 0 (should pend)", len(consumer.applied))
	}
	if res.Ready || !res.Requeue {
		t.Fatalf("want Ready=false Requeue=true, got %+v", res)
	}
	if res.Complete {
		t.Fatalf("want Complete=false on a pending pass (prune must not run), got %+v", res)
	}
	// The provider child WAS applied (it has no cross-target dep of its own).
	if len(provider.applied) != 1 {
		t.Fatalf("provider applied %d, want 1", len(provider.applied))
	}
}
