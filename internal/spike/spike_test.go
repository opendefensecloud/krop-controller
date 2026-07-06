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

package spike

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/crd"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

// Q1a: the load-bearing claim (idea.md §12) is that pkg/runtime imports no
// client-go / controller-runtime / dynamic client. That is precisely true of
// its *direct* imports -- the reconcile-execution surface. It is NOT true of the
// transitive closure: pkg/runtime imports pkg/graph (for the Graph type +
// variable kinds), and pkg/graph *also* houses the Builder, whose imports pull
// in client-go/rest + controller-runtime/apiutil. That coupling is compile-time
// only; nothing on the FromGraph/Node execution path calls a client -- proven by
// TestQ1_DriveRuntimeNoCluster running the whole core with zero cluster access.
//
// So we assert the accurate thing: pkg/runtime's DIRECT imports are client-free.
func TestQ1_RuntimeIsClientFree(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{ join .Imports "\n" }}`,
		"github.com/kubernetes-sigs/kro/pkg/runtime").CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	imports := strings.Fields(string(out))

	forbiddenPrefix := []string{
		"k8s.io/client-go/dynamic",
		"k8s.io/client-go/kubernetes",
		"k8s.io/client-go/rest",
		"k8s.io/client-go/discovery",
		"sigs.k8s.io/controller-runtime",
	}
	for _, imp := range imports {
		for _, f := range forbiddenPrefix {
			if strings.HasPrefix(imp, f) {
				t.Errorf("pkg/runtime directly imports %q -- NOT client-free", imp)
			}
		}
	}
	t.Logf("pkg/runtime direct imports (%d) are client-free: %v", len(imports), imports)

	// Document the transitive nuance so nobody mistakes it for an execution-path
	// coupling: pkg/graph is what drags client-go in, via the Builder.
	gout, err := exec.Command("go", "list", "-f", `{{ join .Imports "\n" }}`,
		"github.com/kubernetes-sigs/kro/pkg/graph").CombinedOutput()
	if err != nil {
		t.Fatalf("go list graph failed: %v\n%s", err, gout)
	}
	if !strings.Contains(string(gout), "k8s.io/client-go/rest") {
		t.Errorf("expected pkg/graph to import client-go/rest (the Builder coupling)")
	}
	t.Logf("transitive client-go enters via pkg/graph (Builder), not the runtime path")
}

// Q1b: build a real *graph.Graph with NO cluster (fake resolver injected) and
// drive the runtime reconcile core end-to-end with FAKE observed data:
//   - GetDesired on the instance-referencing node (${schema.spec.*}) resolves
//     from the instance object alone.
//   - SetObserved on vpc feeds ${vpc.status.vpcID} into subnet.GetDesired,
//     proving cross-resource CEL resolves from SetObserved with no client.
//   - CheckReadiness / IsIgnored run purely on observed state.
func TestQ1_DriveRuntimeNoCluster(t *testing.T) {
	g, err := BuildGraphNoCluster(SampleRGD())
	if err != nil {
		t.Fatalf("BuildGraphNoCluster: %v", err)
	}
	t.Logf("graph built with no cluster; topological order = %v", g.TopologicalOrder)
	if got := g.TopologicalOrder; len(got) != 2 || got[0] != "vpc" || got[1] != "subnet" {
		t.Fatalf("expected dependency order [vpc subnet], got %v", got)
	}

	instance := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "Network",
		"metadata":   map[string]interface{}{"name": "demo", "namespace": "default"},
		"spec":       map[string]interface{}{"name": "acme", "cidr": "10.0.0.0/16"},
	}}

	rt, err := runtime.FromGraph(g, instance, graph.RGDConfig{
		MaxCollectionSize:          1000,
		MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		t.Fatalf("runtime.FromGraph: %v", err)
	}

	nodes := map[string]*runtime.Node{}
	for _, n := range rt.Nodes() {
		nodes[n.Spec.Meta.ID] = n
	}
	vpc, subnet := nodes["vpc"], nodes["subnet"]
	if vpc == nil || subnet == nil {
		t.Fatalf("missing nodes; have %v", nodes)
	}

	// --- vpc: resolves from the instance (schema.spec.*) with no observed dep.
	vpcDesired, err := vpc.GetDesired()
	if err != nil {
		t.Fatalf("vpc.GetDesired: %v", err)
	}
	if got := vpcDesired[0].GetName(); got != "acme-vpc" {
		t.Fatalf("vpc name: want acme-vpc, got %q (schema.spec CEL did not resolve)", got)
	}
	t.Logf("vpc desired name = %q (from ${schema.spec.name})", vpcDesired[0].GetName())

	// --- subnet: BEFORE vpc is observed, its ${vpc.status.vpcID} dep is pending.
	if _, err := subnet.GetDesired(); err == nil {
		t.Fatalf("subnet.GetDesired should be pending until vpc is observed")
	} else {
		t.Logf("subnet.GetDesired correctly pending pre-observation: %v", err)
	}

	// IsIgnored is pure and false here (no includeWhen on subnet).
	if ignored, err := subnet.IsIgnored(); err != nil || ignored {
		t.Fatalf("subnet.IsIgnored: ignored=%v err=%v (want false,nil)", ignored, err)
	}

	// --- feed FAKE observed vpc (as if read back from a cluster we never touched).
	fakeVPC := vpcDesired[0].DeepCopy()
	_ = unstructured.SetNestedField(fakeVPC.Object, "vpc-0xDEADBEEF", "status", "vpcID")
	vpc.SetObserved([]*unstructured.Unstructured{fakeVPC})

	// vpc now passes readyWhen (${vpc.status.vpcID != ''}) from observed alone.
	if err := vpc.CheckReadiness(); err != nil {
		t.Fatalf("vpc.CheckReadiness after observe: %v", err)
	}
	t.Logf("vpc ready from observed status alone (no cluster)")

	// --- subnet.GetDesired now resolves the cross-resource CEL from SetObserved.
	subnetDesired, err := subnet.GetDesired()
	if err != nil {
		t.Fatalf("subnet.GetDesired after vpc observed: %v", err)
	}
	gotVPCID, _, _ := unstructured.NestedString(subnetDesired[0].Object, "spec", "vpcID")
	if gotVPCID != "vpc-0xDEADBEEF" {
		t.Fatalf("cross-resource CEL: subnet.spec.vpcID = %q, want vpc-0xDEADBEEF", gotVPCID)
	}
	t.Logf("cross-resource CEL resolved: subnet.spec.vpcID = %q (from vpc.SetObserved, NO client)", gotVPCID)

	// --- instance status projection (${vpc.status.vpcID}) also resolves.
	instNode := rt.Instance()
	instDesired, err := instNode.GetDesired()
	if err != nil {
		t.Fatalf("instance.GetDesired: %v", err)
	}
	gotStatus, _, _ := unstructured.NestedString(instDesired[0].Object, "status", "vpcID")
	if gotStatus != "vpc-0xDEADBEEF" {
		t.Fatalf("instance status projection = %q, want vpc-0xDEADBEEF", gotStatus)
	}
	t.Logf("instance status.vpcID projected = %q", gotStatus)
}

// Q2: pin down the Builder coupling (spec §16.3 / idea.md §6.1) via reflection,
// documenting the exact facts the findings note cites:
//   - graph.Builder has fields named schemaResolver + restMapper, both UNEXPORTED.
//   - the only exported constructor, graph.NewBuilder, takes (*rest.Config,
//     *http.Client) and internally builds a LIVE discovery/OpenAPI resolver +
//     dynamic REST mapper (it dereferences the config -- NewBuilder(nil,nil)
//     panics, so we do not call it here).
//
// Together these prove: from an external module you cannot inject a kcp-aware
// resolver without either a working rest.Config or unsafe/a fork.
func TestQ2_BuilderCoupling(t *testing.T) {
	bt := reflect.TypeOf(graph.Builder{})
	want := map[string]bool{"schemaResolver": false, "restMapper": false}
	for i := 0; i < bt.NumField(); i++ {
		f := bt.Field(i)
		if _, ok := want[f.Name]; ok {
			unexported := f.PkgPath != "" // non-empty PkgPath == unexported
			want[f.Name] = unexported
			t.Logf("graph.Builder.%s type=%s unexported=%v", f.Name, f.Type, unexported)
		}
	}
	for name, unexported := range want {
		if !unexported {
			t.Errorf("expected graph.Builder.%s to exist AND be unexported", name)
		}
	}

	// NewBuilder signature: func(*rest.Config, *http.Client) (*Builder, error).
	nb := reflect.TypeOf(graph.NewBuilder)
	if nb.NumIn() != 2 || nb.NumOut() != 2 {
		t.Fatalf("unexpected NewBuilder arity: in=%d out=%d", nb.NumIn(), nb.NumOut())
	}
	t.Logf("graph.NewBuilder signature: %v", nb)
	t.Logf("=> no exported constructor accepts schemaResolver/restMapper; the only " +
		"public path (NewBuilder) needs a rest.Config serving discovery/OpenAPI.")
}

// Q3: confirm the pure, client-free helpers the M4 Registrar needs are importable
// and callable without a cluster: simpleschema.ToOpenAPISpec and crd.SynthesizeCRD.
func TestQ3_PureSchemaHelpers(t *testing.T) {
	specSchema, err := simpleschema.ToOpenAPISpec(
		map[string]interface{}{"name": "string", "cidr": "string"},
		nil,
	)
	if err != nil {
		t.Fatalf("simpleschema.ToOpenAPISpec: %v", err)
	}
	if specSchema.Properties["name"].Type != "string" {
		t.Fatalf("ToOpenAPISpec: unexpected schema %+v", specSchema)
	}
	t.Logf("ToOpenAPISpec produced spec with props: %v", keysOf(specSchema.Properties))

	generated := crd.SynthesizeCRD(
		"example.com", "v1alpha1", "Network",
		*specSchema, extv1.JSONSchemaProps{}, false,
		extv1.NamespaceScoped,
		&krov1alpha1.Schema{Kind: "Network", APIVersion: "v1alpha1", Group: "example.com"},
	)
	if generated.Spec.Names.Kind != "Network" || generated.Spec.Group != "example.com" {
		t.Fatalf("SynthesizeCRD: unexpected CRD %+v", generated.Spec.Names)
	}
	t.Logf("SynthesizeCRD produced CRD %q (group=%s, plural=%s)",
		generated.Name, generated.Spec.Group, generated.Spec.Names.Plural)
}

func keysOf(m map[string]extv1.JSONSchemaProps) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
