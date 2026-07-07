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
	"reflect"
	"testing"

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kropengine "go.opendefense.cloud/krop-controller/internal/engine"
)

func TestDeriveClaims_CoreAndForeign(t *testing.T) {
	foreign := []schema.GroupResource{
		{Group: "", Resource: "configmaps"},                     // core → empty identity
		{Group: "access.opendefense.cloud", Resource: "scopes"}, // foreign → identity from map
	}
	identity := map[schema.GroupResource]string{
		{Group: "access.opendefense.cloud", Resource: "scopes"}: "abc123hash",
	}
	claims := DeriveClaims(foreign, claimVerbs, identity)
	if len(claims) != 2 {
		t.Fatalf("want 2 claims, got %d", len(claims))
	}
	byRes := map[string]string{}
	for _, c := range claims {
		byRes[c.Resource] = c.IdentityHash
		if len(c.Verbs) == 0 {
			t.Fatalf("claim %s has no verbs", c.Resource)
		}
	}
	if byRes["configmaps"] != "" {
		t.Fatalf("core configmaps identityHash must be empty, got %q", byRes["configmaps"])
	}
	if byRes["scopes"] != "abc123hash" {
		t.Fatalf("scopes identityHash = %q, want abc123hash", byRes["scopes"])
	}
}

func TestValidateClaims(t *testing.T) {
	tests := []struct {
		name    string
		claims  []apisv1alpha2.PermissionClaim
		wantErr bool
	}{
		{
			name: "core type with empty identity is OK",
			claims: []apisv1alpha2.PermissionClaim{
				{GroupResource: apisv1alpha2.GroupResource{Group: "", Resource: "configmaps"}, IdentityHash: ""},
			},
			wantErr: false,
		},
		{
			name: "foreign type with resolved identity is OK",
			claims: []apisv1alpha2.PermissionClaim{
				{GroupResource: apisv1alpha2.GroupResource{Group: "access.opendefense.cloud", Resource: "scopes"}, IdentityHash: "abc123"},
			},
			wantErr: false,
		},
		{
			name: "foreign type with empty identity is rejected",
			claims: []apisv1alpha2.PermissionClaim{
				{GroupResource: apisv1alpha2.GroupResource{Group: "access.opendefense.cloud", Resource: "scopes"}, IdentityHash: ""},
			},
			wantErr: true,
		},
		{
			name: "mixed: one unresolved foreign among valid is rejected",
			claims: []apisv1alpha2.PermissionClaim{
				{GroupResource: apisv1alpha2.GroupResource{Group: "", Resource: "configmaps"}, IdentityHash: ""},
				{GroupResource: apisv1alpha2.GroupResource{Group: "access.opendefense.cloud", Resource: "scopes"}, IdentityHash: ""},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClaims(tc.claims)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateClaims() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// node builds a graph node of the given id/type/GVR for the claims tests.
func node(id string, typ graph.NodeType, group, resource string) *graph.Node {
	return &graph.Node{Meta: graph.NodeMeta{
		ID:   id,
		Type: typ,
		GVR:  schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource},
	}}
}

// claimFor returns the claim for the given resource, or false if absent.
func claimFor(claims []apisv1alpha2.PermissionClaim, resource string) (apisv1alpha2.PermissionClaim, bool) {
	for _, c := range claims {
		if c.Resource == resource {
			return c, true
		}
	}

	return apisv1alpha2.PermissionClaim{}, false
}

func TestForeignConsumerGRs_SplitsWritableAndExternal(t *testing.T) {
	instanceGR := schema.GroupResource{Group: "krop.opendefense.cloud", Resource: "widgets"}
	g := &graph.Graph{Nodes: map[string]*graph.Node{
		"instance": node("instance", graph.NodeTypeInstance, "krop.opendefense.cloud", "widgets"),
		"child":    node("child", graph.NodeTypeResource, "example.io", "children"),
		"extRef":   node("extRef", graph.NodeTypeExternal, "example.io", "vpcs"),
		"extColl":  node("extColl", graph.NodeTypeExternalCollection, "example.io", "subnets"),
		// provider-target external ref: must not produce a claim.
		"provExt": node("provExt", graph.NodeTypeExternal, "example.io", "gateways"),
		// host-target external ref: must not produce a claim.
		"hostExt": node("hostExt", graph.NodeTypeExternal, "example.io", "hosts"),
	}}
	routing := map[string]kropengine.Target{
		"provExt": kropengine.TargetProvider,
		"hostExt": kropengine.TargetHost,
	}

	writable, external := ForeignConsumerGRs(g, instanceGR, routing)

	wantWritable := []schema.GroupResource{{Group: "example.io", Resource: "children"}}
	if !reflect.DeepEqual(writable, wantWritable) {
		t.Fatalf("writable = %v, want %v", writable, wantWritable)
	}
	wantExternal := map[schema.GroupResource]bool{
		{Group: "example.io", Resource: "vpcs"}:    true,
		{Group: "example.io", Resource: "subnets"}: true,
	}
	if len(external) != len(wantExternal) {
		t.Fatalf("external = %v, want %v entries", external, len(wantExternal))
	}
	for _, gr := range external {
		if !wantExternal[gr] {
			t.Fatalf("unexpected external GR %v", gr)
		}
	}
}

func TestDeriveClaims_ExternalReadOnly_WritableCRUD(t *testing.T) {
	instanceGR := schema.GroupResource{Group: "krop.opendefense.cloud", Resource: "widgets"}
	g := &graph.Graph{Nodes: map[string]*graph.Node{
		"child":  node("child", graph.NodeTypeResource, "example.io", "children"),
		"extRef": node("extRef", graph.NodeTypeExternal, "example.io", "vpcs"),
	}}
	identity := map[schema.GroupResource]string{
		{Group: "example.io", Resource: "children"}: "childhash",
		{Group: "example.io", Resource: "vpcs"}:     "vpchash",
	}

	writable, external := ForeignConsumerGRs(g, instanceGR, nil)
	claims := append(
		DeriveClaims(writable, claimVerbs, identity),
		DeriveClaims(external, readOnlyVerbs, identity)...,
	)

	childClaim, ok := claimFor(claims, "children")
	if !ok {
		t.Fatalf("no claim for children")
	}
	if !reflect.DeepEqual(childClaim.Verbs, claimVerbs) {
		t.Fatalf("children verbs = %v, want %v", childClaim.Verbs, claimVerbs)
	}
	vpcClaim, ok := claimFor(claims, "vpcs")
	if !ok {
		t.Fatalf("no claim for vpcs")
	}
	if !reflect.DeepEqual(vpcClaim.Verbs, readOnlyVerbs) {
		t.Fatalf("vpcs verbs = %v, want %v", vpcClaim.Verbs, readOnlyVerbs)
	}
}

func TestDeriveClaims_CoreExternalAllowed(t *testing.T) {
	// A core (empty-group) external ref keeps an empty identityHash and passes
	// validateClaims — core types legitimately carry no identityHash.
	instanceGR := schema.GroupResource{Group: "krop.opendefense.cloud", Resource: "widgets"}
	g := &graph.Graph{Nodes: map[string]*graph.Node{
		"cm": node("cm", graph.NodeTypeExternal, "", "configmaps"),
	}}

	_, external := ForeignConsumerGRs(g, instanceGR, nil)
	claims := DeriveClaims(external, readOnlyVerbs, map[schema.GroupResource]string{})
	if len(claims) != 1 {
		t.Fatalf("want 1 claim, got %d", len(claims))
	}
	if claims[0].IdentityHash != "" {
		t.Fatalf("core external identityHash = %q, want empty", claims[0].IdentityHash)
	}
	if err := validateClaims(claims); err != nil {
		t.Fatalf("validateClaims rejected core external: %v", err)
	}
}

func TestDeriveClaims_ForeignExternalUnresolvedIdentityRejected(t *testing.T) {
	// A foreign external ref whose owning APIExport is not bound resolves to an
	// empty identityHash and must be rejected by validateClaims.
	instanceGR := schema.GroupResource{Group: "krop.opendefense.cloud", Resource: "widgets"}
	g := &graph.Graph{Nodes: map[string]*graph.Node{
		"vpc": node("vpc", graph.NodeTypeExternal, "example.io", "vpcs"),
	}}

	_, external := ForeignConsumerGRs(g, instanceGR, nil)
	claims := DeriveClaims(external, readOnlyVerbs, map[schema.GroupResource]string{})
	if err := validateClaims(claims); err == nil {
		t.Fatalf("validateClaims accepted foreign external with unresolved identity")
	}
}
