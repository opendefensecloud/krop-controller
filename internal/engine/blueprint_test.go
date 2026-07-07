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
	if len(rgd.Spec.Resources) != 1 || rgd.Spec.Resources[0].ID != "config" {
		t.Fatalf("want a single resource id=config, got %+v", rgd.Spec.Resources)
	}
}
