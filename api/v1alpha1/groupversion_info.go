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

// +kubebuilder:object:generate=true
// +groupName=krop.opendefense.cloud
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group/version for the krop blueprint API.
var GroupVersion = schema.GroupVersion{Group: "krop.opendefense.cloud", Version: "v1alpha1"}

// SchemeBuilder registers the blueprint types. It uses apimachinery's
// runtime.SchemeBuilder (rather than controller-runtime's deprecated
// scheme.Builder) so this api package keeps minimal dependencies.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the blueprint types to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &ResourceGraphDefinition{}, &ResourceGraphDefinitionList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)

	return nil
}
