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
