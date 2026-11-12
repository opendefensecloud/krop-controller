// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

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
