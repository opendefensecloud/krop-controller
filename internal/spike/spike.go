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

// Package spike is THROWAWAY M0 de-risking code. It probes how far kro v0.9.2's
// graph/runtime engine can be driven with NO live cluster, and pins down the
// exact coupling of graph.NewBuilder to a discovery/OpenAPI endpoint (spec §6.1
// / §16.3). None of this ships; it exists to inform the M0 findings note.
package spike

import (
	"reflect"
	"unsafe"

	"k8s.io/apimachinery/pkg/api/meta"
	memory "k8s.io/client-go/discovery/cached/memory"
	restmapper "k8s.io/client-go/restmapper"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// SampleRGD builds an in-memory kro ResourceGraphDefinition with a SimpleSchema
// and two child resources that exercise both kinds of CEL reference the engine
// must resolve:
//
//   - vpc:    template references a ${schema.spec.*} field (instance -> child).
//   - subnet: template references ${vpc.status.vpcID} (child -> child status),
//     which forces a dependency edge vpc -> subnet and topological ordering.
//
// The child GVKs (ec2.services.k8s.aws/v1alpha1 VPC & Subnet) are ones the kro
// fake resolver/discovery already knows about, so the graph can be built without
// contacting an apiserver.
func SampleRGD() *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(
		"network",
		generator.WithSchema(
			"Network", "v1alpha1",
			// spec (SimpleSchema)
			map[string]interface{}{
				"name": "string",
				"cidr": "string",
			},
			// status: mapped from a child status field via CEL, proving
			// status aggregation is a pure CEL projection.
			map[string]interface{}{
				"vpcID": "${vpc.status.vpcID}",
			},
		),
		generator.WithResource("vpc", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1",
			"kind":       "VPC",
			"metadata": map[string]interface{}{
				// instance -> child reference
				"name": "${schema.spec.name}-vpc",
			},
			"spec": map[string]interface{}{
				"cidrBlocks": []interface{}{"${schema.spec.cidr}"},
			},
		}, []string{"${vpc.status.vpcID != ''}"}, nil),
		generator.WithResource("subnet", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1",
			"kind":       "Subnet",
			"metadata": map[string]interface{}{
				"name": "${schema.spec.name}-subnet",
			},
			"spec": map[string]interface{}{
				// child -> child status reference (the dependency edge)
				"vpcID":     "${vpc.status.vpcID}",
				"cidrBlock": "${schema.spec.cidr}",
			},
		}, nil, nil),
	)
	// generator.WithSchema does not set the group; pin it so the synthesized
	// instance GVK is deterministic (example.com/v1alpha1, kind Network).
	rgd.Spec.Schema.Group = "example.com"
	return rgd
}

// BuildGraphNoCluster builds a *graph.Graph from an RGD WITHOUT any live cluster,
// by injecting kro's own fake SchemaResolver + a fake-discovery-backed RESTMapper
// into a graph.Builder.
//
// KEY FINDING (spec §16.3): graph.Builder's schemaResolver / restMapper are
// UNEXPORTED fields and there is no exported constructor that accepts them --
// the only public constructor, graph.NewBuilder(*rest.Config, *http.Client),
// insists on a rest.Config it uses to build a live discovery/OpenAPI resolver.
// So from an EXTERNAL module the fake resolver can only be wired in via unsafe
// reflection (below). That the injection is possible AT ALL proves the engine
// core is client-free; that it needs `unsafe` from outside the kro module is the
// precise coupling the M0 spike set out to measure.
func BuildGraphNoCluster(rgd *krov1alpha1.ResourceGraphDefinition) (*graph.Graph, error) {
	fakeResolver, fakeDiscovery := testk8s.NewFakeResolver()
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(fakeDiscovery))

	b := &graph.Builder{}
	setUnexportedField(b, "schemaResolver", fakeResolver)
	setUnexportedField(b, "restMapper", meta.RESTMapper(restMapper))

	return b.NewResourceGraphDefinition(rgd, graph.RGDConfig{
		MaxCollectionSize:          1000,
		MaxCollectionDimensionSize: 1000,
	})
}

// setUnexportedField sets an unexported struct field via unsafe reflection.
// Used ONLY to inject the fake resolver/restMapper into graph.Builder for the
// no-cluster spike; production code must not do this (see findings note --
// resolve via NewBuilder-at-endpoint or an upstreamed exported constructor).
func setUnexportedField(obj interface{}, name string, val interface{}) {
	rv := reflect.ValueOf(obj).Elem().FieldByName(name)
	rv = reflect.NewAt(rv.Type(), unsafe.Pointer(rv.UnsafeAddr())).Elem()
	rv.Set(reflect.ValueOf(val))
}
