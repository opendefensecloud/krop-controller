// internal/engine/graphsource.go
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
	"fmt"

	"k8s.io/client-go/rest"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// GraphSource builds a compiled kro graph from a blueprint RGD. Split behind an
// interface so the engine loop tests use an in-memory build (no cluster) while
// production builds against a schema-complete workspace endpoint.
type GraphSource interface {
	Build(rgd *krov1alpha1.ResourceGraphDefinition) (*graph.Graph, error)
}

// EndpointGraphSource builds graphs via graph.NewBuilder pointed at a workspace
// that serves discovery/OpenAPI for every child GVK (design §16.3 option A).
type EndpointGraphSource struct {
	builder *graph.Builder
}

// NewEndpointGraphSource constructs the builder from a workspace-scoped config.
// The config must serve discovery/OpenAPI — NewBuilder dereferences it eagerly.
// kro's graph.NewBuilder (via apiutil.NewDynamicRESTMapper) requires a non-nil
// http.Client, so we derive one from the config's transport (TLS, auth, proxy).
func NewEndpointGraphSource(cfg *rest.Config) (*EndpointGraphSource, error) {
	if cfg == nil {
		return nil, fmt.Errorf("EndpointGraphSource: nil rest.Config")
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("EndpointGraphSource: http client: %w", err)
	}
	b, err := graph.NewBuilder(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("EndpointGraphSource: %w", err)
	}
	return &EndpointGraphSource{builder: b}, nil
}

// Build compiles the RGD into a graph (per-blueprint; amortized over instances).
func (s *EndpointGraphSource) Build(rgd *krov1alpha1.ResourceGraphDefinition) (*graph.Graph, error) {
	return s.builder.NewResourceGraphDefinition(rgd, graph.RGDConfig{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
}
