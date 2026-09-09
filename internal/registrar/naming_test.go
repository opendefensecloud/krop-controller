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

package registrar

import (
	"strings"
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
)

func namingSpec(n *kropv1alpha1.Naming) kropv1alpha1.ResourceGraphDefinitionSpec {
	return kropv1alpha1.ResourceGraphDefinitionSpec{
		Resources: []*kropv1alpha1.Resource{
			{Resource: krov1alpha1.Resource{ID: "gdcaProject"}, Target: "host", Naming: n},
		},
	}
}

func TestNamingConstraints_ConvertsPerResource(t *testing.T) {
	got, err := NamingConstraints(namingSpec(&kropv1alpha1.Naming{MaxLength: 30, Prefix: "p"}))
	if err != nil {
		t.Fatalf("NamingConstraints: %v", err)
	}
	c, ok := got["gdcaProject"]
	if !ok {
		t.Fatalf("gdcaProject absent from %v", got)
	}
	if c.MaxLength != 30 || c.Prefix != "p" {
		t.Errorf("constraints = %+v, want {MaxLength:30 Prefix:p}", c)
	}
}

func TestNamingConstraints_AbsentBlockYieldsNoEntry(t *testing.T) {
	got, err := NamingConstraints(namingSpec(nil))
	if err != nil {
		t.Fatalf("NamingConstraints: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no entries for a blueprint that declares no naming, got %v", got)
	}
}

func TestNamingConstraints_RejectsUnsatisfiableConstraints(t *testing.T) {
	// maxLength 15 with a 1-character prefix leaves no room for a readable
	// character once the 12-character content hash and its separators are taken.
	_, err := NamingConstraints(namingSpec(&kropv1alpha1.Naming{MaxLength: 15, Prefix: "p"}))
	if err == nil {
		t.Fatal("want an error for unsatisfiable constraints, got nil")
	}
	// The message must name the offending resource: a blueprint may hold many.
	if !strings.Contains(err.Error(), "gdcaProject") {
		t.Errorf("error must name the resource, got %q", err)
	}
}
