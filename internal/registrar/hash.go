// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

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
