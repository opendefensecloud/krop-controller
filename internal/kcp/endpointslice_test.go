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

// internal/kcp/endpointslice_test.go
package kcp

import "testing"

func TestValidateKubeconfig_RejectsNonWorkspaceHost(t *testing.T) {
	if err := ValidateKubeconfig("https://example.com"); err == nil {
		t.Fatal("want error: host is not workspace-scoped (missing /clusters/ path)")
	}
}

func TestValidateKubeconfig_AcceptsWorkspaceHost(t *testing.T) {
	if err := ValidateKubeconfig("https://kcp.example.com/clusters/root:providers:acme"); err != nil {
		t.Fatalf("want nil for workspace-scoped host, got %v", err)
	}
}
