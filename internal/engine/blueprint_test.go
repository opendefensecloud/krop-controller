// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

// internal/engine/blueprint_test.go
package engine

import "testing"

func TestLoadExampleBlueprint(t *testing.T) {
	rgd, err := LoadExampleBlueprint()
	if err != nil {
		t.Fatalf("LoadExampleBlueprint: %v", err)
	}
	if rgd.Spec.Schema.Kind != "KubernetesCluster" {
		t.Fatalf("schema kind = %q, want KubernetesCluster", rgd.Spec.Schema.Kind)
	}
	if len(rgd.Spec.Resources) != 2 {
		t.Fatalf("want 2 resources, got %d", len(rgd.Spec.Resources))
	}
	ids := map[string]bool{}
	for _, r := range rgd.Spec.Resources {
		ids[r.ID] = true
	}
	if !ids["config"] || !ids["agentRequest"] {
		t.Fatalf("want resources config+agentRequest, got %v", ids)
	}
}
