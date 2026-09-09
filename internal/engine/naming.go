// internal/engine/naming.go
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

package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// maxNameLen is the Kubernetes metadata.name length ceiling.
const maxNameLen = 253

// hashLen is the length of the content-hash suffix every derived name carries.
// It is load-bearing for injectivity and is never truncated.
const hashLen = 12

// NameConstraints narrows a derived child name to what the target API server
// actually accepts. The ZERO VALUE reproduces the plain Kubernetes form, so a
// blueprint that says nothing about naming keeps the names it has always had.
//
// Both fields exist because a target may enforce rules stricter than Kubernetes'
// own (issue #30): Google Cloud caps names at 30 characters, and RFC1123 LABEL
// types require an alphabetic first character — which a kcp logical cluster name
// (e.g. "231cyw14qhtl611l") routinely violates.
type NameConstraints struct {
	// MaxLength caps the TOTAL derived name. Zero means maxNameLen.
	MaxLength int
	// Prefix, when set, is prepended as "<prefix>-" so the name starts with an
	// alphabetic character regardless of what the cluster name starts with.
	Prefix string
}

// Validate reports whether the constraints leave room for a name that is still
// collision-free AND readable: the prefix, the 12-character content hash and the
// separators are all mandatory, so at least one character of the readable middle
// must survive. Constraints that fail here would drive ChildName into its bare
// prefix+hash fallback, which is legal but tells an operator nothing — so the
// Registrar rejects them at publish time instead of shipping unreadable children.
//
// A zero MaxLength (the Kubernetes ceiling) is always valid.
func (c NameConstraints) Validate() error {
	if c.MaxLength <= 0 {
		return nil
	}
	// hash + the "-" joining it + one readable character.
	required := hashLen + 2
	if c.Prefix != "" {
		required += len(c.Prefix) + 1 // the prefix and its "-"
	}
	if c.MaxLength < required {
		return fmt.Errorf("maxLength %d is too small: prefix %q plus the %d-character content hash "+
			"needs at least %d", c.MaxLength, c.Prefix, hashLen, required)
	}

	return nil
}

// ChildName derives a deterministic, collision-free name for a child of a
// QUALIFIED target (provider or host), honoring c. Many consumers' children land
// in ONE provider workspace or host cluster (idea.md §9.1), so the name is
// qualified by the consumer's logical cluster name and the instance name. Inputs
// are assumed already DNS-safe (kcp cluster names, k8s-validated instance names,
// blueprint template names).
//
// The name is ALWAYS suffixed with a short content hash of the structured,
// null-joined tuple so distinct (cluster, instance, name) tuples never collide —
// even when the readable hyphen-joined prefix would. Instance names are DNS-1123
// subdomains that may themselves contain hyphens, so the hyphen-joined prefix
// alone is not injective (e.g. "a"+"b-c"+"d" vs "a-b"+"c"+"d" both read "a-b-c-d").
// \x00 cannot appear in a DNS name, so the null-joined tuple is un-collidable.
//
// Under a tight MaxLength the READABLE MIDDLE is the only part truncation may eat:
// the hash suffix carries injectivity and the prefix carries the letter start, so
// both survive intact. That keeps names collision-free at any ceiling — at the
// cost of readability, which the GC labels on every child preserve anyway.
func ChildName(clusterName, instanceName, originalName string, c NameConstraints) string {
	sum := sha256.Sum256([]byte(clusterName + "\x00" + instanceName + "\x00" + originalName))
	suffix := hex.EncodeToString(sum[:])[:hashLen]

	limit := c.MaxLength
	if limit <= 0 {
		limit = maxNameLen
	}
	var head string
	if c.Prefix != "" {
		head = c.Prefix + "-"
	}

	// Budget for the readable middle: what is left after the prefix, the hash and
	// the "-" joining them.
	budget := limit - len(head) - len(suffix) - 1
	if budget <= 0 {
		// No room for any readable part. Still deterministic and injective, just
		// unreadable. The Registrar rejects such constraints at publish time, so
		// this is the belt-and-braces runtime path.
		return head + suffix
	}

	base := fmt.Sprintf("%s-%s-%s", clusterName, instanceName, originalName)
	if len(base) > budget {
		base = base[:budget]
		// Truncation can sever the readable prefix mid-segment, leaving a trailing
		// "-" or "." that would yield a "--"/".-" seam before the hash suffix. Both
		// are DNS-valid (internal hyphens/dots are allowed), but trimming keeps the
		// derived name clean and unambiguous.
		base = strings.TrimRight(base, "-.")
	}

	return head + base + "-" + suffix
}

// ProviderChildName is ChildName with no constraints: the unconstrained
// Kubernetes form used wherever a blueprint declares no naming requirements.
func ProviderChildName(clusterName, instanceName, originalName string) string {
	return ChildName(clusterName, instanceName, originalName, NameConstraints{})
}

// LivenessRecordName derives the fixed, collision-free name of the provider-
// workspace liveness record (a ConfigMap) for one instance. Keyed by the
// consumer cluster + instance UID (null-joined so the tuple is un-collidable),
// hashed short so the name is stable and DNS-safe regardless of input length.
func LivenessRecordName(consumerCluster, instanceUID string) string {
	sum := sha256.Sum256([]byte(consumerCluster + "\x00" + instanceUID))

	return "krop-live-" + hex.EncodeToString(sum[:])[:16]
}
