// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package registrar

import (
	"sync"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// GraphCache holds compiled blueprint graphs keyed by (workspace, name, specHash).
// The workspace dimension avoids the blueprint-name collision hazard (design §1.6).
// Graph build is expensive (discovery/OpenAPI), so this amortizes it per blueprint.
type GraphCache struct {
	mu sync.RWMutex
	m  map[string]map[string]*graph.Graph // (ws|name) -> specHash -> graph
}

// NewGraphCache returns an empty cache.
func NewGraphCache() *GraphCache { return &GraphCache{m: map[string]map[string]*graph.Graph{}} }

func bpKey(workspace, name string) string { return workspace + "|" + name }

// Get returns the cached graph for (workspace, name, specHash) if present.
func (c *GraphCache) Get(workspace, name, specHash string) (*graph.Graph, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	byHash, ok := c.m[bpKey(workspace, name)]
	if !ok {
		return nil, false
	}
	g, ok := byHash[specHash]

	return g, ok
}

// Put stores the graph, replacing any prior hash for this blueprint.
func (c *GraphCache) Put(workspace, name, specHash string, g *graph.Graph) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[bpKey(workspace, name)] = map[string]*graph.Graph{specHash: g}
}

// Delete drops all cached graphs for a blueprint.
func (c *GraphCache) Delete(workspace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, bpKey(workspace, name))
}
