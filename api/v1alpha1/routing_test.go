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

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestToKro_RoutingMap(t *testing.T) {
	spec := ResourceGraphDefinitionSpec{
		Schema: &krov1alpha1.Schema{Kind: "KubernetesCluster"},
		Resources: []*Resource{
			{Resource: krov1alpha1.Resource{ID: "config"}}, // empty target
			{Resource: krov1alpha1.Resource{ID: "agentRequest"}, Target: "provider"},
			{Resource: krov1alpha1.Resource{ID: "vm"}, Target: "host"},
		},
	}

	_, routing := spec.ToKro()

	// Present targets land in the map keyed by resource id.
	if got := routing["agentRequest"]; got != "provider" {
		t.Errorf("routing[agentRequest] = %q, want provider", got)
	}
	if got := routing["vm"]; got != "host" {
		t.Errorf("routing[vm] = %q, want host", got)
	}
	// An empty target is absent (defaults to consumer downstream).
	if _, ok := routing["config"]; ok {
		t.Errorf("routing[config] present, want absent (empty target)")
	}
	if len(routing) != 2 {
		t.Errorf("routing has %d entries, want 2", len(routing))
	}
}

func TestToKro_PreservesKroFields(t *testing.T) {
	tmpl := runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)}
	extRef := &krov1alpha1.ExternalRef{APIVersion: "v1", Kind: "Secret"}
	spec := ResourceGraphDefinitionSpec{
		Schema: &krov1alpha1.Schema{Kind: "KubernetesCluster"},
		Resources: []*Resource{
			{Resource: krov1alpha1.Resource{ID: "config", Template: tmpl}, Target: "provider"},
			{Resource: krov1alpha1.Resource{ID: "existing", ExternalRef: extRef}},
		},
	}

	kro, _ := spec.ToKro()

	// The schema pointer is carried through verbatim.
	if kro.Schema == nil || kro.Schema.Kind != "KubernetesCluster" {
		t.Fatalf("kro.Schema = %+v", kro.Schema)
	}
	if len(kro.Resources) != 2 {
		t.Fatalf("kro.Resources has %d entries, want 2", len(kro.Resources))
	}
	// The routing target is NOT carried onto the kro resource (clean kro type).
	if kro.Resources[0].ID != "config" || string(kro.Resources[0].Template.Raw) != string(tmpl.Raw) {
		t.Errorf("resource[0] = %+v, want id=config with template preserved", kro.Resources[0])
	}
	if kro.Resources[1].ID != "existing" || kro.Resources[1].ExternalRef == nil ||
		kro.Resources[1].ExternalRef.Kind != "Secret" {
		t.Errorf("resource[1] = %+v, want id=existing with externalRef preserved", kro.Resources[1])
	}
}

func TestToKro_SkipsNilResources(t *testing.T) {
	spec := ResourceGraphDefinitionSpec{
		Resources: []*Resource{
			nil,
			{Resource: krov1alpha1.Resource{ID: "config"}, Target: "provider"},
			nil,
		},
	}

	kro, routing := spec.ToKro()

	if len(kro.Resources) != 1 || kro.Resources[0].ID != "config" {
		t.Fatalf("kro.Resources = %+v, want single config entry", kro.Resources)
	}
	if len(routing) != 1 || routing["config"] != "provider" {
		t.Errorf("routing = %+v, want {config: provider}", routing)
	}
}
