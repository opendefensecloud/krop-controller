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
	"sort"

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

// claimVerbs is the CRUD verb set the engine needs on claimed consumer-target types.
var claimVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete"}

// DeriveClaims builds one permissionClaim per foreign consumer-target GroupResource,
// resolving identityHash from the provided map (empty string for core types).
// Deterministic order (sorted) so publications are stable.
func DeriveClaims(foreign []schema.GroupResource, identity map[schema.GroupResource]string) []apisv1alpha2.PermissionClaim {
	sorted := append([]schema.GroupResource(nil), foreign...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Group != sorted[j].Group {
			return sorted[i].Group < sorted[j].Group
		}

		return sorted[i].Resource < sorted[j].Resource
	})
	claims := make([]apisv1alpha2.PermissionClaim, 0, len(sorted))
	for _, gr := range sorted {
		claims = append(claims, apisv1alpha2.PermissionClaim{
			GroupResource: apisv1alpha2.GroupResource{Group: gr.Group, Resource: gr.Resource},
			Verbs:         claimVerbs,
			IdentityHash:  identity[gr],
		})
	}

	return claims
}

// ForeignConsumerGRs enumerates the GroupResources of consumer-target nodes that
// are NOT the instance's own type (those need permissionClaims to be written into
// the consumer workspace through the vw). Reads the routing target off each node's
// template exactly as the engine does.
func ForeignConsumerGRs(g *graph.Graph, instanceGR schema.GroupResource) []schema.GroupResource {
	seen := map[schema.GroupResource]bool{}
	var out []schema.GroupResource
	for _, node := range g.Nodes {
		target, err := kropengine.TargetOf(node.Template)
		if err != nil || target != kropengine.TargetConsumer {
			continue
		}
		gr := node.Meta.GVR.GroupResource()
		if gr == instanceGR || seen[gr] {
			continue
		}
		seen[gr] = true
		out = append(out, gr)
	}

	return out
}
