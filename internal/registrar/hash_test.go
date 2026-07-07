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
)

func mustHash(t *testing.T, spec krov1alpha1.ResourceGraphDefinitionSpec) string {
	t.Helper()
	h, err := SpecHash(spec)
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}

	return h
}

func TestSpecHash_StableAndSensitive(t *testing.T) {
	a := krov1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "A"}}
	b := krov1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "A"}}
	c := krov1alpha1.ResourceGraphDefinitionSpec{Schema: &krov1alpha1.Schema{Kind: "B"}}
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
