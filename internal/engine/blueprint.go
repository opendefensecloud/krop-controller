// internal/engine/blueprint.go
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

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"sigs.k8s.io/yaml"

	_ "embed"
)

//go:embed embedded/blueprint-kubernetescluster.yaml
var exampleBlueprintYAML []byte

// LoadExampleBlueprint parses the M1 example blueprint (a kro RGD) that the
// controller compiles at startup. Replaced by the Registrar in M4.
func LoadExampleBlueprint() (*krov1alpha1.ResourceGraphDefinition, error) {
	var rgd krov1alpha1.ResourceGraphDefinition
	if err := yaml.Unmarshal(exampleBlueprintYAML, &rgd); err != nil {
		return nil, fmt.Errorf("unmarshalling example blueprint: %w", err)
	}

	return &rgd, nil
}
