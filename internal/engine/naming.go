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
)

// maxNameLen is the Kubernetes metadata.name length ceiling.
const maxNameLen = 253

// ProviderChildName derives a deterministic, collision-free, DNS-safe name for a
// provider-target child. Many consumers' provider children land in ONE provider
// workspace (idea.md §9.1), so the name is qualified by the consumer's logical
// cluster name and the instance name. Inputs are assumed already DNS-safe
// (kcp cluster names, k8s-validated instance names, blueprint template names);
// over-long results fall back to a truncated prefix + content hash.
func ProviderChildName(clusterName, instanceName, originalName string) string {
	base := fmt.Sprintf("%s-%s-%s", clusterName, instanceName, originalName)
	if len(base) <= maxNameLen {
		return base
	}
	sum := sha256.Sum256([]byte(base))
	suffix := hex.EncodeToString(sum[:])[:16]
	// leave room for "-" + 16 hex chars
	prefix := base[:maxNameLen-1-len(suffix)]
	return prefix + "-" + suffix
}
