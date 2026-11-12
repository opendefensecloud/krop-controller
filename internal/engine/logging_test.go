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
	"strings"
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"go.opendefense.cloud/krop-controller/internal/testlog"
)

// logCtx returns a context carrying a capturing logger at verbosity v.
func logCtx(v int) (context.Context, *testlog.Sink) {
	lg, sink := testlog.New(v)

	return log.IntoContext(context.Background(), lg), sink
}

// runtimeFor builds a runtime for rgd with an instance of kind carrying spec.
func runtimeFor(t *testing.T, rgd *krov1alpha1.ResourceGraphDefinition, kind string, spec map[string]any) *runtime.Runtime {
	t.Helper()
	g := buildTestGraph(t, rgd)
	inst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "krop.opendefense.cloud/v1alpha1", "kind": kind,
		"metadata": map[string]any{"name": "demo", "namespace": "default"},
		"spec":     spec,
	}}
	rt, err := runtime.FromGraph(g, graph.Config{MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000}, runtime.WithInstance(inst))
	if err != nil {
		t.Fatalf("FromGraph: %v", err)
	}

	return rt
}

// vpcOnlyRGD is a single provider VPC that is ready only once status.vpcID is set.
func vpcOnlyRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"vpconly",
		generator.WithSchema("VPCOnly", "v1alpha1",
			map[string]any{"region": "string"},
			map[string]any{"vpcName": "${vpc.metadata.name}"},
		),
		generator.WithResource("vpc", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "VPC",
			"metadata": map[string]any{"name": "${schema.spec.region}-vpc"},
			"spec":     map[string]any{"cidrBlocks": []any{"10.0.0.0/16"}},
		}, []string{"${vpc.status.vpcID != ''}"}, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

// includeWhenRGD is crossTargetRGD with the subnet gated on the vpc's status via
// includeWhen, so a not-ready vpc makes kro's IsIgnored report data pending.
func includeWhenRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"incwhen",
		generator.WithSchema("IncWhen", "v1alpha1",
			map[string]any{"region": "string"},
			map[string]any{"vpcName": "${vpc.metadata.name}"},
		),
		generator.WithResource("vpc", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "VPC",
			"metadata": map[string]any{"name": "${schema.spec.region}-vpc"},
			"spec":     map[string]any{"cidrBlocks": []any{"10.0.0.0/16"}},
		}, []string{"${vpc.status.vpcID != ''}"}, nil),
		// The includeWhen text differs from vpc's readyWhen on purpose: kro's runtime
		// shares one evaluation state per expression string across kinds, and a
		// shared readyWhen state lacks the cross-node reference includeWhen needs.
		generator.WithResource("subnet", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "Subnet",
			"metadata": map[string]any{"name": "${schema.spec.region}-subnet"},
			"spec":     map[string]any{"vpcID": "fixed", "cidrBlock": "10.0.1.0/24"},
		}, nil, []string{"${vpc.status.vpcID != 'none'}"}),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

func TestReconcile_Outcome_Complete(t *testing.T) {
	res, err := New().Reconcile(context.Background(), newRuntime(t, newInstance()),
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, sampleRouting())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Outcome != OutcomeComplete || res.Node != "" || res.Reason != "" {
		t.Fatalf("want Complete with no node/reason, got %+v", res)
	}
}

func TestReconcile_Outcome_PendingNamesNode(t *testing.T) {
	res, err := New().Reconcile(context.Background(), crossTargetRuntime(t),
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, crossTargetRouting())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Outcome != OutcomePending || res.Node != "subnet" {
		t.Fatalf("want Pending on subnet, got %+v", res)
	}
	if !strings.Contains(res.Reason, "vpc") || !strings.Contains(res.Reason, "not ready") {
		t.Fatalf("reason should name the not-ready dependency, got %q", res.Reason)
	}
}

func TestReconcile_Outcome_ExternalNotFound(t *testing.T) {
	res, err := New().Reconcile(context.Background(), externalRefRuntime(t),
		map[Target]Applier{TargetConsumer: &fakeApplier{}},
		map[Target]Reader{TargetConsumer: &fakeReader{}}, nil)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Outcome != OutcomeExternalNotFound || res.Node != "vpc" || res.Reason == "" {
		t.Fatalf("want ExternalNotFound on vpc with a reason, got %+v", res)
	}
}

func TestReconcile_Outcome_NotReadyFirstNode(t *testing.T) {
	rt := runtimeFor(t, vpcOnlyRGD(), "VPCOnly", map[string]any{"region": "eu"})
	res, err := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetProvider: &fakeApplier{}}, nil, map[string]Target{"vpc": TargetProvider})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Outcome != OutcomeNotReady || res.Node != "vpc" || !res.Complete {
		t.Fatalf("want NotReady on vpc with Complete=true, got %+v", res)
	}
	if !strings.Contains(res.Reason, "readyWhen") {
		t.Fatalf("reason should carry the readiness message, got %q", res.Reason)
	}
}

func TestReconcile_Outcome_FailedOnGenuineError(t *testing.T) {
	rt := runtimeFor(t, brokenCELRGD(), "Broken", map[string]any{"region": "abc"})
	res, err := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, nil)
	if err == nil {
		t.Fatalf("want an error, got nil (res=%+v)", res)
	}
	if res.Outcome != OutcomeFailed || res.Node != "config" || res.Reason == "" || res.Ready {
		t.Fatalf("want Failed on config with a reason and not ready, got %+v", res)
	}
}

func TestReconcile_IncludeWhenPending_OutcomePendingStillErrors(t *testing.T) {
	rt := runtimeFor(t, includeWhenRGD(), "IncWhen", map[string]any{"region": "eu"})
	res, err := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, crossTargetRouting())
	if err == nil {
		t.Fatalf("includeWhen data pending must still return an error (behaviour unchanged), got nil (res=%+v)", res)
	}
	if !errors.Is(err, runtime.ErrDataPending) {
		t.Fatalf("want ErrDataPending in the chain, got %v", err)
	}
	if res.Outcome != OutcomePending || res.Node != "subnet" || res.Ready {
		t.Fatalf("want Pending on subnet and not ready, got %+v", res)
	}
}

func TestReconcile_V2Lines_PerNode(t *testing.T) {
	ctx, sink := logCtx(2)
	if _, err := New().Reconcile(ctx, newRuntime(t, newInstance()),
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, sampleRouting()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	lines := sink.Find("node applied")
	if len(lines) != 2 {
		t.Fatalf("want 2 node applied lines, got %d: %s", len(lines), sink.Raw())
	}
	byNode := map[string]testlog.Line{}
	for _, l := range lines {
		if l.Level != 2 {
			t.Fatalf("node line at level %d, want 2", l.Level)
		}
		byNode[l.Values["node"].(string)] = l
	}
	cfg, ok := byNode["config"]
	if !ok {
		t.Fatalf("no line for node config: %s", sink.Raw())
	}
	if cfg.Values["target"] != "consumer" || cfg.Values["gvk"] != "/v1/ConfigMap" ||
		cfg.Values["child"] != "eu-cluster-config" || cfg.Values["ready"] != true {
		t.Fatalf("config line fields wrong: %+v", cfg.Values)
	}
	if byNode["providerRecord"].Values["target"] != "provider" {
		t.Fatalf("providerRecord line target wrong: %+v", byNode["providerRecord"].Values)
	}

	quiet, quietSink := logCtx(1)
	if _, err := New().Reconcile(quiet, newRuntime(t, newInstance()),
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, sampleRouting()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if n := len(quietSink.Find("node applied")); n != 0 {
		t.Fatalf("node lines must be V(2) only, got %d at V(1)", n)
	}
}

func TestReconcile_V2Lines_ExternalRead(t *testing.T) {
	ctx, sink := logCtx(2)
	reader := &fakeReader{objs: map[string]*unstructured.Unstructured{"default/prod-vpc": vpcWithID("vpc-abc123")}}
	if _, err := New().Reconcile(ctx, externalRefRuntime(t),
		map[Target]Applier{TargetConsumer: &fakeApplier{}},
		map[Target]Reader{TargetConsumer: reader}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	lines := sink.Find("node read")
	if len(lines) != 1 {
		t.Fatalf("want 1 node read line, got %d: %s", len(lines), sink.Raw())
	}
	v := lines[0].Values
	if v["node"] != "vpc" || v["target"] != "consumer" || v["child"] != "prod-vpc" {
		t.Fatalf("node read fields wrong: %+v", v)
	}
}

func TestReconcile_V2Line_EmptyCollection(t *testing.T) {
	ctx, sink := logCtx(2)
	var recorded []ChildID
	host, _ := hostApplier(&recorded)
	if _, err := New().Reconcile(ctx, collectionRuntime(t), map[Target]Applier{TargetHost: host}, nil,
		map[string]Target{"addrs": TargetHost, "consumer": TargetHost}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var addrs []testlog.Line
	for _, l := range sink.Find("node applied") {
		if l.Values["node"] == "addrs" {
			addrs = append(addrs, l)
		}
	}
	if len(addrs) != 1 {
		t.Fatalf("want 1 node applied line for addrs, got %d: %s", len(addrs), sink.Raw())
	}
	children, ok := addrs[0].Values["children"].([]any)
	if !ok || len(children) != 0 {
		t.Fatalf("want an empty children list, got %#v", addrs[0].Values["children"])
	}
}

func TestReconcile_V2Lines_NeverLogObjectContent(t *testing.T) {
	ctx, sink := logCtx(2)
	consumer := &fakeApplier{mutate: func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(o.Object, "SENTINEL-SECRET-42", "status", "secret")
	}}
	if _, err := New().Reconcile(ctx, newRuntime(t, newInstance()),
		map[Target]Applier{TargetConsumer: consumer, TargetProvider: &fakeApplier{}}, nil, sampleRouting()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sink.Raw() == "" {
		t.Fatal("expected V(2) output to inspect")
	}
	if strings.Contains(sink.Raw(), "SENTINEL-SECRET-42") {
		t.Fatalf("object content leaked into logs: %s", sink.Raw())
	}
}

func TestGVKString(t *testing.T) {
	if got := GVKString(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}); got != "/v1/ConfigMap" {
		t.Fatalf("core GVK = %q, want /v1/ConfigMap", got)
	}
	gvk := schema.GroupVersionKind{Group: "storage.cnrm.cloud.google.com", Version: "v1beta1", Kind: "StorageBucket"}
	if got := GVKString(gvk); got != "storage.cnrm.cloud.google.com/v1beta1/StorageBucket" {
		t.Fatalf("GVK = %q", got)
	}
}

// withReady returns an applier mutation that sets a Ready condition (and, when
// given, a message that must never reach the logs).
func withReady(status, reason, message string) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedSlice(o.Object, []any{
			map[string]any{"type": "Synced", "status": "False", "reason": "ReconcileError"},
			map[string]any{"type": "Ready", "status": status, "reason": reason, "message": message},
		}, "status", "conditions")
	}
}

func TestReadyReason(t *testing.T) {
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
		{"not ready", []*unstructured.Unstructured{obj(withReady("False", "UpdateFailed", "x"))}, "UpdateFailed"},
		{"unknown", []*unstructured.Unstructured{obj(withReady("Unknown", "Creating", ""))}, "Creating"},
		{"ready", []*unstructured.Unstructured{obj(withReady("True", "UpToDate", ""))}, ""},
		{"no conditions", []*unstructured.Unstructured{obj(nil)}, ""},
		{"only another condition type", []*unstructured.Unstructured{obj(func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": "Synced", "status": "False", "reason": "ReconcileError"}}, "status", "conditions")
		})}, ""},
		{"free text is dropped", []*unstructured.Unstructured{obj(withReady("False", "bucket name already taken", ""))}, ""},
		{"code with punctuation is dropped", []*unstructured.Unstructured{obj(withReady("False", "token=abc", ""))}, ""},
		{"overlong code is dropped", []*unstructured.Unstructured{obj(withReady("False", strings.Repeat("A", 65), ""))}, ""},
		{"first not-ready item of a collection",
			[]*unstructured.Unstructured{obj(withReady("True", "UpToDate", "")), obj(withReady("False", "DependencyNotReady", ""))}, "DependencyNotReady"},
	} {
		if got := readyReason(tc.objs); got != tc.want {
			t.Errorf("%s: readyReason = %q, want %q", tc.desc, got, tc.want)
		}
	}
}

// #72: a pass pending on a not-ready dependency names the dependency and its
// Ready condition's reason, never the condition's free-text message.
func TestReconcile_Pending_NamesDependencyReadyReason(t *testing.T) {
	ctx, sink := logCtx(2)
	provider := &fakeApplier{mutate: withReady("False", "UpdateFailed", "SENTINEL-MESSAGE")}
	res, err := New().Reconcile(ctx, crossTargetRuntime(t),
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: provider}, nil, crossTargetRouting())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Outcome != OutcomePending || res.Node != "subnet" || res.Dependency != "vpc" || res.ReadyReason != "UpdateFailed" {
		t.Fatalf("want Pending on subnet waiting for vpc (UpdateFailed), got %+v", res)
	}
	vpc := sink.Find("node applied")
	if len(vpc) != 1 || vpc[0].Values["node"] != "vpc" || vpc[0].Values["readyReason"] != "UpdateFailed" {
		t.Fatalf("want the vpc node line to carry readyReason, got: %s", sink.Raw())
	}
	if strings.Contains(sink.Raw(), "SENTINEL-MESSAGE") {
		t.Fatalf("condition message leaked into logs: %s", sink.Raw())
	}
}

// A dependency without a Ready condition is still named; the reason stays empty.
func TestReconcile_Pending_DependencyWithoutCondition(t *testing.T) {
	res, err := New().Reconcile(context.Background(), crossTargetRuntime(t),
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{}}, nil, crossTargetRouting())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Dependency != "vpc" || res.ReadyReason != "" {
		t.Fatalf("want dependency vpc without a reason, got %+v", res)
	}
}

func TestReconcile_NotReady_ReadyReasonOfNode(t *testing.T) {
	rt := runtimeFor(t, vpcOnlyRGD(), "VPCOnly", map[string]any{"region": "eu"})
	res, err := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetProvider: &fakeApplier{mutate: withReady("False", "Creating", "")}}, nil,
		map[string]Target{"vpc": TargetProvider})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Outcome != OutcomeNotReady || res.Node != "vpc" || res.Dependency != "" || res.ReadyReason != "Creating" {
		t.Fatalf("want NotReady on vpc with readyReason Creating, got %+v", res)
	}
}

func TestReconcile_IncludeWhenPending_NamesDependency(t *testing.T) {
	rt := runtimeFor(t, includeWhenRGD(), "IncWhen", map[string]any{"region": "eu"})
	res, _ := New().Reconcile(context.Background(), rt,
		map[Target]Applier{TargetConsumer: &fakeApplier{}, TargetProvider: &fakeApplier{mutate: withReady("False", "UpdateFailed", "")}},
		nil, crossTargetRouting())
	if res.Outcome != OutcomePending || res.Dependency != "vpc" || res.ReadyReason != "UpdateFailed" {
		t.Fatalf("want Pending waiting for vpc (UpdateFailed), got %+v", res)
	}
}

// With two not-ready dependencies kro names one of them (in map order); the
// recorded dependency must be the one its reason names.
func TestReconcile_Pending_DependencyMatchesReason(t *testing.T) {
	rgd := generator.NewResourceGraphDefinition("twodeps",
		generator.WithSchema("TwoDeps", "v1alpha1", map[string]any{"region": "string"}, nil),
		generator.WithResource("vpc", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "VPC",
			"metadata": map[string]any{"name": "${schema.spec.region}-vpc"},
			"spec":     map[string]any{"cidrBlocks": []any{"10.0.0.0/16"}},
		}, []string{"${vpc.status.vpcID != ''}"}, nil),
		generator.WithResource("gateway", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "VPC",
			"metadata": map[string]any{"name": "${schema.spec.region}-gw"},
			"spec":     map[string]any{"cidrBlocks": []any{"10.1.0.0/16"}},
		}, []string{"${gateway.status.vpcID != 'none'}"}, nil),
		generator.WithResource("subnet", map[string]any{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1", "kind": "Subnet",
			"metadata": map[string]any{"name": "${vpc.metadata.name}-${gateway.metadata.name}"},
			"spec":     map[string]any{"vpcID": "fixed", "cidrBlock": "10.0.1.0/24"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	for range 20 {
		rt := runtimeFor(t, rgd, "TwoDeps", map[string]any{"region": "eu"})
		res, err := New().Reconcile(context.Background(), rt, map[Target]Applier{TargetConsumer: &fakeApplier{}}, nil, nil)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if res.Node != "subnet" || res.Dependency == "" ||
			!strings.Contains(res.Reason, fmt.Sprintf("dependent node %q", res.Dependency)) {
			t.Fatalf("dependency %q does not match reason %q", res.Dependency, res.Reason)
		}
	}
}

// A data-pending error that names no dependency (e.g. an includeWhen expression on
// a missing field of a ready dependency) must not be attributed to some other
// not-ready dependency: the reason would then contradict the dependency field.
func TestWaitingOn_OnlyTheDependencyKroNames(t *testing.T) {
	notReady := map[string]string{"bucket": "UpdateFailed"}
	for _, tc := range []struct {
		desc, wantDep string
		err           error
	}{
		{"names bucket", "bucket", fmt.Errorf("node %q: dependent node %q not ready: x (%w)", "sa", "bucket", runtime.ErrDataPending)},
		{"includeWhen names bucket", "bucket", fmt.Errorf("includeWhen dependency %q not ready: x (%w)", "bucket", runtime.ErrDataPending)},
		{"names no dependency", "", fmt.Errorf("includeWhen %q: no such key: email (%w)", "${vpc.status.email != ''}", runtime.ErrDataPending)},
		{"names a ready dependency", "", fmt.Errorf("node %q: dependent node %q not ready: x (%w)", "sa", "vpc", runtime.ErrDataPending)},
	} {
		var r Result
		r.waitingOn([]string{"vpc", "bucket"}, notReady, tc.err)
		if r.Dependency != tc.wantDep {
			t.Errorf("%s: dependency = %q, want %q", tc.desc, r.Dependency, tc.wantDep)
		}
	}
}
