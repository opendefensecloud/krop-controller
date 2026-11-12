// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

// Package kcp holds kcp-specific helpers (workspace URL handling, endpoint-slice
// discovery) for the multicluster manager.
//
// Adapted (reproduced, not imported) from the access-operator reference at
// iampam/access-operator/internal/kcp.
package kcp

import (
	"context"
	"fmt"
	"strings"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const clustersPath = "/clusters/"

// ValidateKubeconfig requires a workspace-scoped host (its path contains a
// /clusters/<name> segment), which the endpoint-slice discovery relies on.
func ValidateKubeconfig(host string) error {
	if !strings.Contains(host, clustersPath) {
		return fmt.Errorf("kubeconfig host %q is not workspace-scoped (missing %q)", host, clustersPath)
	}

	return nil
}

// FindEndpointSlice lists APIExportEndpointSlices in the workspace `c` points at
// and returns the name of the one whose Spec.APIExport.Name equals apiExportName.
// The multicluster provider needs this name to watch the virtual-workspace
// endpoints for the APIExport. Errors if no matching slice exists.
func FindEndpointSlice(ctx context.Context, c client.Client, apiExportName string) (string, error) {
	var slices apisv1alpha1.APIExportEndpointSliceList
	if err := c.List(ctx, &slices); err != nil {
		return "", fmt.Errorf("listing APIExportEndpointSlices: %w", err)
	}
	for i := range slices.Items {
		if slices.Items[i].Spec.APIExport.Name == apiExportName {
			return slices.Items[i].Name, nil
		}
	}

	return "", fmt.Errorf("no APIExportEndpointSlice found for APIExport %q", apiExportName)
}
