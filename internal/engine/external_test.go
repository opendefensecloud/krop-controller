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

// internal/engine/external_test.go
package engine

import (
	"context"
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeReader is the read-side test double: Get returns a preloaded object keyed by
// "namespace/name" (NotFound otherwise) and counts calls; List echoes a fixed set.
type fakeReader struct {
	objs  map[string]*unstructured.Unstructured
	items []*unstructured.Unstructured
	gets  int
	lists int
}

func (r *fakeReader) Get(_ context.Context, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	r.gets++
	if o, ok := r.objs[ns+"/"+name]; ok {
		return o, nil
	}

	return nil, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, name)
}

func (r *fakeReader) List(_ context.Context, _ schema.GroupVersionKind, _ string, _ labels.Selector) ([]*unstructured.Unstructured, error) {
	r.lists++

	return r.items, nil
}

// externalRefRGD: an externalRef "vpc" (read-only) whose status.vpcID feeds a
// consumer-target subnet reading ${vpc.status.vpcID}. VPC/Subnet are in the fake
// resolver's schema. Both nodes default to the consumer target (empty routing).
func externalRefRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"extref",
		generator.WithSchema("ExtRef", "v1alpha1",
			map[string]any{"region": "string"},
			map[string]any{"vpcID": "${vpc.status.vpcID}"},
		),
		generator.WithExternalRef("vpc", &krov1alpha1.ExternalRef{
			APIVersion: "ec2.services.k8s.aws/v1alpha1",
			Kind:       "VPC",
			Metadata:   krov1alpha1.ExternalRefMetadata{Name: "prod-vpc", Namespace: "default"},
		}, nil, nil),
		generator.WithResource("subnet", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "Subnet",
			"metadata": map[string]any{"name": "${schema.spec.region}-subnet", "namespace": "default"},
			"spec":     map[string]any{"vpcID": "${vpc.status.vpcID}", "cidrBlock": "10.0.1.0/24"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

func externalRefRuntime(t *testing.T) *runtime.Runtime {
	t.Helper()
	g := buildTestGraph(t, externalRefRGD())
	inst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": "ExtRef",
		"metadata": map[string]any{"name": "demo", "namespace": "default"},
		"spec":     map[string]any{"region": "eu"},
	}}
	rt, err := runtime.FromGraph(g, inst, graph.RGDConfig{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000})
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}

	return rt
}

// vpcWithID returns a VPC object carrying status.vpcID, as the external Reader
// would fetch it from the cluster.
func vpcWithID(id string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "VPC",
		"metadata": map[string]any{"name": "prod-vpc", "namespace": "default"},
		"status":   map[string]any{"vpcID": id},
	}}
}

// An external ref that exists is READ (never applied) and its observed value feeds
// the downstream consumer child's CEL.
func TestReconcile_ExternalRef_ObservedFeedsDownstream(t *testing.T) {
	rt := externalRefRuntime(t)
	consumer := &fakeApplier{}
	reader := &fakeReader{objs: map[string]*unstructured.Unstructured{"default/prod-vpc": vpcWithID("vpc-abc123")}}

	res, err := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetConsumer: consumer},
		map[Target]Reader{TargetConsumer: reader}, nil)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reader.gets != 1 {
		t.Fatalf("external Get called %d times, want 1", reader.gets)
	}
	// The consumer subnet is the ONLY applied object — the external VPC is never applied.
	if len(consumer.applied) != 1 {
		t.Fatalf("consumer applied %d objects, want 1 (external VPC must not be applied)", len(consumer.applied))
	}
	if got, _, _ := unstructured.NestedString(consumer.applied[0].Object, "spec", "vpcID"); got != "vpc-abc123" {
		t.Fatalf("subnet.spec.vpcID = %q, want vpc-abc123 (external value not funneled)", got)
	}
	di, err := ProjectStatus(rt)
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if got, _, _ := unstructured.NestedString(di.Object, "status", "vpcID"); got != "vpc-abc123" {
		t.Fatalf("instance status.vpcID = %q, want vpc-abc123", got)
	}
	if !res.Ready || !res.Complete {
		t.Fatalf("want Ready+Complete, got %+v", res)
	}
}

// A not-yet-present external ref pends the whole pass: Ready=false, Requeue=true,
// Complete=false (so prune stays disabled), and the applier is NEVER invoked.
func TestReconcile_ExternalRef_NotFoundPendsWithoutApplying(t *testing.T) {
	rt := externalRefRuntime(t)
	consumer := &fakeApplier{}
	reader := &fakeReader{} // empty → Get returns NotFound

	res, err := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetConsumer: consumer},
		map[Target]Reader{TargetConsumer: reader}, nil)
	if err != nil {
		t.Fatalf("Reconcile: %v (a missing external ref is normal convergence, not an error)", err)
	}
	if res.Ready || !res.Requeue {
		t.Fatalf("want Ready=false Requeue=true, got %+v", res)
	}
	if res.Complete {
		t.Fatalf("want Complete=false while the external ref is pending (prune must stay off), got %+v", res)
	}
	if len(consumer.applied) != 0 {
		t.Fatalf("applier invoked %d times on a pending external ref, want 0", len(consumer.applied))
	}
}

// An external ref is never recorded as a materialized child: with the consumer
// applier wrapped in a RecordingApplier, only the subnet's ChildID is recorded —
// the external VPC never is (it is read, not applied/labeled/owned).
func TestReconcile_ExternalRef_NeverRecorded(t *testing.T) {
	rt := externalRefRuntime(t)
	var sink []ChildID
	consumer := NewRecordingApplier(&fakeApplier{}, &sink)
	reader := &fakeReader{objs: map[string]*unstructured.Unstructured{"default/prod-vpc": vpcWithID("vpc-abc123")}}

	if _, err := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetConsumer: consumer},
		map[Target]Reader{TargetConsumer: reader}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(sink) != 1 {
		t.Fatalf("recorded %d ChildIDs, want 1 (only the subnet; the external VPC must not be recorded): %+v", len(sink), sink)
	}
	if sink[0].GVK.Kind != "Subnet" {
		t.Fatalf("recorded child GVK = %s, want Subnet", sink[0].GVK.Kind)
	}
}
