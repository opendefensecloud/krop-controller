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
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// Resource wraps a kro resource with a krop routing target. All kro fields
// (id/template/externalRef/readyWhen/includeWhen/forEach) are inlined verbatim.
type Resource struct {
	krov1alpha1.Resource `json:",inline"`
	// Target routes this resource's object(s): consumer (default) | provider | host.
	// +kubebuilder:validation:Enum=consumer;provider;host
	// +optional
	Target string `json:"target,omitempty"`
}

// ResourceGraphDefinitionSpec is kro's spec (Schema + Resources) with each
// resource carrying a routing Target. ToKro strips the targets back out.
type ResourceGraphDefinitionSpec struct {
	// +kubebuilder:validation:Required
	Schema *krov1alpha1.Schema `json:"schema,omitempty"`
	// +optional
	Resources []*Resource `json:"resources,omitempty"`
}

// ToKro returns the underlying kro spec (clean types for the graph builder) plus
// a routing map of resource id → target. Resources with an empty target are
// omitted from the map (they default to consumer downstream).
func (s ResourceGraphDefinitionSpec) ToKro() (krov1alpha1.ResourceGraphDefinitionSpec, map[string]string) {
	routing := map[string]string{}
	res := make([]*krov1alpha1.Resource, 0, len(s.Resources))
	for _, r := range s.Resources {
		if r == nil {
			continue
		}
		kr := r.Resource
		res = append(res, &kr)
		if r.Target != "" {
			routing[r.ID] = r.Target
		}
	}

	return krov1alpha1.ResourceGraphDefinitionSpec{Schema: s.Schema, Resources: res}, routing
}
