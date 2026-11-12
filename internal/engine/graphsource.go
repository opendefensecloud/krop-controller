// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"k8s.io/client-go/rest"
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
	return s.builder.NewResourceGraphDefinition(rgd, graph.Config{
		MaxCollectionSize: 1000, MaxCollectionDimensionSize: 1000,
	})
}
