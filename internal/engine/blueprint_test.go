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
