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
