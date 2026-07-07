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
