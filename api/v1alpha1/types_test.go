// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"sigs.k8s.io/yaml"
)

func TestResourceGraphDefinition_ParsesKroSpec(t *testing.T) {
	raw := []byte(`
apiVersion: krop.opendefense.cloud/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: demo
spec:
  schema:
    apiVersion: v1alpha1
    kind: KubernetesCluster
    group: krop.opendefense.cloud
    spec:
      region: string
  resources:
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: x
`)
	var rgd ResourceGraphDefinition
	if err := yaml.Unmarshal(raw, &rgd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rgd.Spec.Schema.Kind != "KubernetesCluster" {
		t.Fatalf("schema kind = %q", rgd.Spec.Schema.Kind)
	}
	if len(rgd.Spec.Resources) != 1 || rgd.Spec.Resources[0].ID != "config" {
		t.Fatalf("resources = %+v", rgd.Spec.Resources)
	}
}
