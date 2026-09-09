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
	"fmt"

	kropv1alpha1 "go.opendefense.cloud/krop-controller/api/v1alpha1"
	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// NamingConstraints converts the blueprint's per-resource naming blocks into the
// engine's typed constraints, validating each one.
//
// The CRD already range-checks maxLength and pattern-checks prefix, but it cannot
// express the relationship BETWEEN them — a long prefix can exhaust a legal
// maxLength — so that check lives here and fails the publish loudly rather than
// silently shipping children named after nothing but their hash.
//
// Resources with no naming block are absent from the result, which the engine
// reads as the unconstrained Kubernetes form.
func NamingConstraints(spec kropv1alpha1.ResourceGraphDefinitionSpec) (map[string]kropengine.NameConstraints, error) {
	raw := spec.NamingMap()
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]kropengine.NameConstraints, len(raw))
	for id, n := range raw {
		c := kropengine.NameConstraints{MaxLength: n.MaxLength, Prefix: n.Prefix}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("resource %q: %w", id, err)
		}
		out[id] = c
	}

	return out, nil
}
