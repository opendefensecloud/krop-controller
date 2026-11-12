// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package registrar

import (
	"testing"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

func TestGraphCache_KeyedByWorkspaceNameHash(t *testing.T) {
	c := NewGraphCache()
	g := &graph.Graph{}
	c.Put("ws1", "bp", "h1", g)

	if got, ok := c.Get("ws1", "bp", "h1"); !ok || got != g {
		t.Fatal("expected cache hit for same key")
	}
	if _, ok := c.Get("ws1", "bp", "h2"); ok {
		t.Fatal("different hash must miss (spec changed)")
	}
	if _, ok := c.Get("ws2", "bp", "h1"); ok {
		t.Fatal("different workspace must miss (collision hazard, design §1.6)")
	}
	c.Delete("ws1", "bp")
	if _, ok := c.Get("ws1", "bp", "h1"); ok {
		t.Fatal("deleted entry must miss")
	}
}
