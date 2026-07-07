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
	"fmt"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
)

// SpecHash returns a short deterministic content hash of a blueprint spec, used
// for the ARS name suffix and for change-detection (skip rebuild when unchanged).
// It hashes our wrapper spec (Schema + Resources WITH their routing targets) so a
// target-only edit still bumps the hash and triggers a republish. It can fail
// because the spec embeds runtime.RawExtension, whose MarshalJSON is fallible;
// callers must surface the error rather than hash a partial body.
func SpecHash(spec kropv1alpha1.ResourceGraphDefinitionSpec) (string, error) {
	b, err := json.Marshal(spec) // stable: json.Marshal sorts map keys
	if err != nil {
		return "", fmt.Errorf("marshaling blueprint spec for hashing: %w", err)
	}
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])[:12], nil
}
