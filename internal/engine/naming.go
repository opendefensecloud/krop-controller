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

// ProviderChildName derives a deterministic, collision-free, DNS-safe name for a
// provider-target child. Many consumers' provider children land in ONE provider
// workspace (idea.md §9.1), so the name is qualified by the consumer's logical
// cluster name and the instance name. Inputs are assumed already DNS-safe
// (kcp cluster names, k8s-validated instance names, blueprint template names).
//
// The name is ALWAYS suffixed with a short content hash of the structured,
// null-joined tuple so distinct (cluster, instance, name) tuples never collide —
// even when the readable hyphen-joined prefix would. Instance names are DNS-1123
// subdomains that may themselves contain hyphens, so the hyphen-joined prefix
// alone is not injective (e.g. "a"+"b-c"+"d" vs "a-b"+"c"+"d" both read "a-b-c-d").
// \x00 cannot appear in a DNS name, so the null-joined tuple is un-collidable.
func ProviderChildName(clusterName, instanceName, originalName string) string {
	sum := sha256.Sum256([]byte(clusterName + "\x00" + instanceName + "\x00" + originalName))
	suffix := hex.EncodeToString(sum[:])[:12]
	base := fmt.Sprintf("%s-%s-%s", clusterName, instanceName, originalName)
	// leave room for "-" + suffix; truncate the readable prefix if needed.
	if len(base)+1+len(suffix) > maxNameLen {
		base = base[:maxNameLen-1-len(suffix)]
		// Truncation can sever the readable prefix mid-segment, leaving a trailing
		// "-" or "." that would yield a "--"/".-" seam before the hash suffix. Both
		// are DNS-valid (internal hyphens/dots are allowed), but trimming keeps the
		// derived name clean and unambiguous.
		base = strings.TrimRight(base, "-.")
	}

	return base + "-" + suffix
}

// LivenessRecordName derives the fixed, collision-free name of the provider-
// workspace liveness record (a ConfigMap) for one instance. Keyed by the
// consumer cluster + instance UID (null-joined so the tuple is un-collidable),
// hashed short so the name is stable and DNS-safe regardless of input length.
func LivenessRecordName(consumerCluster, instanceUID string) string {
	sum := sha256.Sum256([]byte(consumerCluster + "\x00" + instanceUID))

	return "krop-live-" + hex.EncodeToString(sum[:])[:16]
}
