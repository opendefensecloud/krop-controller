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
	// Naming constrains the name krop derives for this resource's object(s) on a
	// qualified target (provider or host). Omit it for the unconstrained
	// Kubernetes form. Ignored for consumer-target resources, whose objects keep
	// their template name.
	// +optional
	Naming *Naming `json:"naming,omitempty"`
}

// Naming constrains the derived name of a qualified-target child so it satisfies
// an API server stricter than Kubernetes itself. krop derives child names as
// "<cluster>-<instance>-<template name>-<hash>", which can exceed a target's
// length ceiling or start with a digit (kcp logical cluster names often do) —
// both fatal for APIs that enforce their own rules.
//
// The content hash always survives, so constrained names stay collision-free.
type Naming struct {
	// MaxLength caps the total derived name. Omit for the Kubernetes ceiling (253).
	// The floor of 16 is what a 1-character prefix, a readable character and the
	// content hash need; anything less leaves no room to stay collision-free.
	// +kubebuilder:validation:Minimum=16
	// +kubebuilder:validation:Maximum=253
	// +optional
	MaxLength int `json:"maxLength,omitempty"`
	// Prefix is prepended as "<prefix>-" so the derived name starts with an
	// alphabetic character, as RFC1123 label types require.
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=20
	// +optional
	Prefix string `json:"prefix,omitempty"`
}

// NamingMap returns a map of resource id -> naming constraints for the resources
// that declare a naming block. Resources without one are ABSENT rather than
// zero-valued, so "no naming block" and "explicitly unconstrained" stay
// indistinguishable downstream — both mean the Kubernetes form.
//
// It is a separate accessor rather than a third ToKro return so ToKro's signature
// (and every caller of it) stays untouched.
func (s ResourceGraphDefinitionSpec) NamingMap() map[string]Naming {
	naming := map[string]Naming{}
	for _, r := range s.Resources {
		if r == nil || r.Naming == nil {
			continue
		}
		naming[r.ID] = *r.Naming
	}

	return naming
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
