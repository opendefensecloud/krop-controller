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

// Package registrar publishes provider blueprints as bindable kcp APIExports.
package registrar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// SpecHash returns a short deterministic content hash of a blueprint spec, used
// for the ARS name suffix and for change-detection (skip rebuild when unchanged).
func SpecHash(spec krov1alpha1.ResourceGraphDefinitionSpec) string {
	b, _ := json.Marshal(spec) // stable: json.Marshal sorts map keys
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}
