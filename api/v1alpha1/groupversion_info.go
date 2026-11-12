// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

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
