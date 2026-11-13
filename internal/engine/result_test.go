// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// errApplier fails every apply with err.
type errApplier struct{ err error }

func (a errApplier) Apply(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return nil, a.err
}

// errReader fails every read with err.
type errReader struct{ err error }

func (r errReader) Get(context.Context, schema.GroupVersionKind, string, string) (*unstructured.Unstructured, error) {
	return nil, r.err
}

func (r errReader) List(context.Context, schema.GroupVersionKind, string, labels.Selector) ([]*unstructured.Unstructured, error) {
	return nil, r.err
}

// withVPCID sets status.vpcID, which the vpc nodes' readyWhen and includeWhen read.
func withVPCID(id string) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) { _ = unstructured.SetNestedField(o.Object, id, "status", "vpcID") }
}

// #49: the Result counts the graph's nodes and the ready ones, for the instance's
// progress. A pass that ends early leaves the nodes after it uncounted.
func TestReconcile_CountsNodes(t *testing.T) {
	for _, tc := range []struct {
		name              string
		run               func(t *testing.T) (Result, error)
		nodes, readyNodes int
	}{
		{"every node ready", func(t *testing.T) (Result, error) {
			return New().Reconcile(context.Background(), newRuntime(t, newInstance()),
				map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, sampleRouting())
		}, 2, 2},
		{"pending on a not-ready dependency", func(t *testing.T) (Result, error) {
			return New().Reconcile(context.Background(), crossTargetRuntime(t),
				map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, crossTargetRouting())
		}, 2, 0},
		{"dependency ready", func(t *testing.T) (Result, error) {
			return New().Reconcile(context.Background(), crossTargetRuntime(t),
				map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{mutate: withVPCID("vpc-1")}}, nil, crossTargetRouting())
		}, 2, 2},
		{"node excluded by includeWhen", func(t *testing.T) (Result, error) {
			rt := runtimeFor(t, includeWhenRGD(), "IncWhen", map[string]any{"region": "eu"})

			return New().Reconcile(context.Background(), rt,
				map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{mutate: withVPCID("none")}}, nil, crossTargetRouting())
		}, 1, 1},
		{"external ref read", func(t *testing.T) (Result, error) {
			reader := &fakeReader{objs: map[string]*unstructured.Unstructured{"default/prod-vpc": vpcWithID("vpc-1")}}

			return New().Reconcile(context.Background(), externalRefRuntime(t),
				map[Target]Applier{TargetConsumer: &fakeApplier{}}, map[Target]Reader{TargetConsumer: reader}, nil)
		}, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.run(t)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if res.Nodes != tc.nodes || res.ReadyNodes != tc.readyNodes {
				t.Fatalf("nodes %d/%d, want %d/%d (%+v)", res.ReadyNodes, res.Nodes, tc.readyNodes, tc.nodes, res)
			}
		})
	}
}

// #49: a failed pass names the step it failed at, which decides what the
// consumer is told and whether a retry can clear it.
func TestReconcile_FailedStage(t *testing.T) {
	denied := apierrors.NewBadRequest("denied")
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) (Result, error)
		want Stage
	}{
		{"expression fails", func(t *testing.T) (Result, error) {
			rt := runtimeFor(t, brokenCELRGD(), "Broken", map[string]any{"region": "abc"})

			return New().Reconcile(context.Background(), rt,
				map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, nil)
		}, StageEvaluate},
		{"no applier for the target", func(t *testing.T) (Result, error) {
			return New().Reconcile(context.Background(), newRuntime(t, newInstance()), map[Target]Applier{}, nil, sampleRouting())
		}, StageRoute},
		{"apply refused", func(t *testing.T) (Result, error) {
			return New().Reconcile(context.Background(), newRuntime(t, newInstance()),
				map[Target]Applier{TargetConsumer: errApplier{denied}, TargetProvider: errApplier{denied}}, nil, sampleRouting())
		}, StageApply},
		{"external read refused", func(t *testing.T) (Result, error) {
			forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "vpcs"}, "prod-vpc", errors.New("claim"))

			return New().Reconcile(context.Background(), externalRefRuntime(t),
				map[Target]Applier{TargetConsumer: &fakeApplier{}}, map[Target]Reader{TargetConsumer: errReader{forbidden}}, nil)
		}, StageRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.run(t)
			if err == nil {
				t.Fatalf("want an error, got nil (%+v)", res)
			}
			if res.Outcome != OutcomeFailed || res.Stage != tc.want {
				t.Fatalf("want Failed at stage %q, got outcome %q stage %q", tc.want, res.Outcome, res.Stage)
			}
		})
	}
}

// #49: the Ready message of the not-ready object reaches the Result, the node's
// own on NotReady and the dependency's when a node pends on it.
func TestReconcile_ReadyMessage(t *testing.T) {
	t.Run("not ready node", func(t *testing.T) {
		rt := runtimeFor(t, vpcOnlyRGD(), "VPCOnly", map[string]any{"region": "eu"})
		res, err := New().Reconcile(context.Background(), rt,
			map[Target]Applier{TargetProvider: &fakeApplier{mutate: withReady("False", "Creating", "creating the vpc")}}, nil,
			map[string]Target{"vpc": TargetProvider})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if res.Outcome != OutcomeNotReady || res.ReadyMessage != "creating the vpc" {
			t.Fatalf("want NotReady with the vpc's message, got %+v", res)
		}
	})
	t.Run("pending on a not-ready dependency", func(t *testing.T) {
		res, err := New().Reconcile(context.Background(), crossTargetRuntime(t),
			map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{mutate: withReady("False", "UpdateFailed", "quota exceeded")}},
			nil, crossTargetRouting())
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if res.Outcome != OutcomePending || res.Dependency != "vpc" || res.ReadyMessage != "quota exceeded" {
			t.Fatalf("want Pending on vpc with its message, got %+v", res)
		}
	})
}

func TestReadyMessage(t *testing.T) {
	obj := func(mutate func(*unstructured.Unstructured)) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		if mutate != nil {
			mutate(u)
		}

		return u
	}
	for _, tc := range []struct {
		desc string
		objs []*unstructured.Unstructured
		want string
	}{
		{"not ready", []*unstructured.Unstructured{obj(withReady("False", "UpdateFailed", "quota exceeded"))}, "quota exceeded"},
		{"ready", []*unstructured.Unstructured{obj(withReady("True", "UpToDate", "all good"))}, ""},
		{"no conditions", []*unstructured.Unstructured{obj(nil)}, ""},
		{"first not-ready item of a collection",
			[]*unstructured.Unstructured{obj(withReady("True", "UpToDate", "fine")), obj(withReady("False", "Creating", "second"))}, "second"},
	} {
		if got := readyMessage(tc.objs); got != tc.want {
			t.Errorf("%s: readyMessage = %q, want %q", tc.desc, got, tc.want)
		}
	}
}
