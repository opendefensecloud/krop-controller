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

	"k8s.io/apimachinery/pkg/api/meta"
	memory "k8s.io/client-go/discovery/cached/memory"
	restmapper "k8s.io/client-go/restmapper"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// sampleRGD is the M1 blueprint: schema{region}, one consumer-target child that
// is a core ConfigMap carrying the region. ConfigMap is in the kro fake
// resolver's core scheme, so the graph builds with no cluster.
func sampleRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"kubernetescluster",
		generator.WithSchema(
			"KubernetesCluster", "v1alpha1",
			map[string]interface{}{"region": "string"},
			map[string]interface{}{"configMapName": "${config.metadata.name}"},
		),
		generator.WithResource("config", map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "${schema.spec.region}-cluster-config",
				"namespace": "default",
				"annotations": map[string]interface{}{
					TargetAnnotation: string(TargetConsumer),
				},
			},
			"data": map[string]interface{}{"region": "${schema.spec.region}"},
		}, nil, nil),
	)
	rgd.Spec.Schema.Group = "krop.opendefense.cloud"
	return rgd
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

func setUnexportedField(obj interface{}, name string, val interface{}) {
	rv := reflect.ValueOf(obj).Elem().FieldByName(name)
	rv = reflect.NewAt(rv.Type(), unsafe.Pointer(rv.UnsafeAddr())).Elem()
	rv.Set(reflect.ValueOf(val))
}

func TestBuildTestGraph_Builds(t *testing.T) {
	g := buildTestGraph(t, sampleRGD())
	if len(g.TopologicalOrder) != 1 || g.TopologicalOrder[0] != "config" {
		t.Fatalf("topological order = %v, want [config]", g.TopologicalOrder)
	}
}
