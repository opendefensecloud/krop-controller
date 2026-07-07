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

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestDeriveClaims_CoreAndForeign(t *testing.T) {
	foreign := []schema.GroupResource{
		{Group: "", Resource: "configmaps"},                     // core → empty identity
		{Group: "access.opendefense.cloud", Resource: "scopes"}, // foreign → identity from map
	}
	identity := map[schema.GroupResource]string{
		{Group: "access.opendefense.cloud", Resource: "scopes"}: "abc123hash",
	}
	claims := DeriveClaims(foreign, identity)
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
