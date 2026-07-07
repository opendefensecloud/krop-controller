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
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
)

func mustHash(t *testing.T, spec kropv1alpha1.ResourceGraphDefinitionSpec) string {
	t.Helper()
	h, err := SpecHash(spec)
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}

	return h
}

func TestSpecHash_StableAndSensitive(t *testing.T) {
	a := kropv1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "A"}}
	b := kropv1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "A"}}
	c := kropv1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "B"}}
	if mustHash(t, a) != mustHash(t, b) {
		t.Fatal("equal specs must hash equal")
	}
	if mustHash(t, a) == mustHash(t, c) {
		t.Fatal("different specs must hash differently")
	}
	if len(mustHash(t, a)) == 0 {
		t.Fatal("empty hash")
	}
}

// A target-only edit must change the hash so the registrar republishes: the
// wrapper spec includes each resource's routing target in the hashed body.
func TestSpecHash_SensitiveToTarget(t *testing.T) {
	base := kropv1alpha1.ResourceGraphDefinitionSpec{
		Schema:    &krov1alpha1.Schema{Kind: "A"},
		Resources: []*kropv1alpha1.Resource{{Resource: krov1alpha1.Resource{ID: "config"}}},
	}
	retargeted := kropv1alpha1.ResourceGraphDefinitionSpec{
		Schema:    &krov1alpha1.Schema{Kind: "A"},
		Resources: []*kropv1alpha1.Resource{{Resource: krov1alpha1.Resource{ID: "config"}, Target: "provider"}},
	}
	if mustHash(t, base) == mustHash(t, retargeted) {
		t.Fatal("a target change must bump the spec hash")
	}
}
