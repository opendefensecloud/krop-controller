// Copyright BWI GmbH and krop-controller contributors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"

	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ProjectStatus returns the instance object with its blueprint-mapped status.*
// fields resolved from observed child state. The runtime must have observed all
// nodes the status expressions reference (i.e. call after Reconcile's loop).
func ProjectStatus(rt *runtime.Runtime) (*unstructured.Unstructured, error) {
	desired, err := rt.Instance().GetDesired()
	if err != nil {
		return nil, err
	}
	// GetDesired returns one object per instance; guard against an empty slice so
	// a misbehaving runtime yields an error rather than an index-out-of-range panic.
	if len(desired) == 0 {
		return nil, fmt.Errorf("instance produced no desired object")
	}

	return desired[0], nil
}
