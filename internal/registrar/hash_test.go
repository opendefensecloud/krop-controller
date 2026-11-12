// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

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
		Resources: []*kropv1alpha1.Resource{{ID: "config"}},
	}
	retargeted := kropv1alpha1.ResourceGraphDefinitionSpec{
		Schema:    &krov1alpha1.Schema{Kind: "A"},
		Resources: []*kropv1alpha1.Resource{{ID: "config", Target: "provider"}},
	}
	if mustHash(t, base) == mustHash(t, retargeted) {
		t.Fatal("a target change must bump the spec hash")
	}
}
