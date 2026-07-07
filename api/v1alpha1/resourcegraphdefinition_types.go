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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BlueprintStatus is the observed state the Registrar writes back.
type BlueprintStatus struct {
	// ExportedAPI is the metadata.name of the published APIExport.
	// +optional
	ExportedAPI string `json:"exportedAPI,omitempty"`
	// IdentityHash is the published APIExport's identity hash.
	// +optional
	IdentityHash string `json:"identityHash,omitempty"`
	// ObservedSpecHash is the spec hash the current publication reflects.
	// +optional
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`
	// Conditions (Ready, etc.).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ResourceGraphDefinition is a provider-authored blueprint. Its spec wraps kro's
// ResourceGraphDefinition spec (Schema + Resources), adding a per-resource routing
// `target` field; ToKro strips the targets back into kro's type for the builder.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=rgd
type ResourceGraphDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceGraphDefinitionSpec `json:"spec,omitempty"`
	Status BlueprintStatus             `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ResourceGraphDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceGraphDefinition `json:"items"`
}
