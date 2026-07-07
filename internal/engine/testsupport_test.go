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

// internal/engine/testsupport_test.go
package engine

import (
	"reflect"
	"testing"
	"unsafe"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
	"k8s.io/apimachinery/pkg/api/meta"
	memory "k8s.io/client-go/discovery/cached/memory"
	restmapper "k8s.io/client-go/restmapper"
)

// sampleRGD is the M1 blueprint: schema{region} with a consumer-target ConfigMap
// carrying the region and a provider-target ConfigMap "record". Routing is now
// carried by the build-time routing map (see sampleRouting), not the templates.
// ConfigMap is in the kro fake resolver's core scheme, so the graph builds with no
// cluster.
func sampleRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"kubernetescluster",
		generator.WithSchema(
			"KubernetesCluster", "v1alpha1",
			map[string]any{"region": "string"},
			map[string]any{"configMapName": "${config.metadata.name}"},
		),
		generator.WithResource("config", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.spec.region}-cluster-config",
				"namespace": "default",
			},
			"data": map[string]any{"region": "${schema.spec.region}"},
		}, nil, nil),
		generator.WithResource("providerRecord", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.spec.region}-provider-record",
				"namespace": "default",
			},
			"data": map[string]any{"region": "${schema.spec.region}"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"

	return rgd
}

// sampleRouting is the routing map for sampleRGD: config → consumer (the default),
// providerRecord → provider. Keyed by resource id, exactly as the engine resolves.
func sampleRouting() map[string]Target {
	return map[string]Target{"config": TargetConsumer, "providerRecord": TargetProvider}
}

// buildTestGraph builds a *graph.Graph with NO cluster by injecting kro's fake
// resolver into a graph.Builder via unsafe (test-only; see Task notes).
func buildTestGraph(t *testing.T, rgd *krov1alpha1.ResourceGraphDefinition) *graph.Graph {
	t.Helper()
	fakeResolver, fakeDiscovery := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDiscovery))
	b := &graph.Builder{}
	setUnexportedField(b, "schemaResolver", fakeResolver)
	setUnexportedField(b, "restMapper", meta.RESTMapper(rm))
	g, err := b.NewResourceGraphDefinition(rgd, graph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
	if err != nil {
		t.Fatalf("buildTestGraph: %v", err)
	}

	return g
}

func setUnexportedField(obj any, name string, val any) {
	rv := reflect.ValueOf(obj).Elem().FieldByName(name)
	rv = reflect.NewAt(rv.Type(), unsafe.Pointer(rv.UnsafeAddr())).Elem()
	rv.Set(reflect.ValueOf(val))
}

func TestBuildTestGraph_Builds(t *testing.T) {
	g := buildTestGraph(t, sampleRGD())
	if len(g.TopologicalOrder) != 2 {
		t.Fatalf("topological order = %v, want 2 nodes", g.TopologicalOrder)
	}
}
