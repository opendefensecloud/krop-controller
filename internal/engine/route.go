// internal/engine/route.go
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

// Package engine drives kro's client-free runtime for a single instance,
// owning all apply/observe I/O and routing each child to its target workspace.
package engine

import (
	"fmt"
)

// Target is a per-resource destination for an applied child object.
type Target string

const (
	// TargetConsumer routes a child into the consumer (tenant) workspace. Default.
	TargetConsumer Target = "consumer"
	// TargetProvider routes a child into the provider workspace.
	TargetProvider Target = "provider"
	// TargetHost routes a child into the host (physical) cluster.
	TargetHost Target = "host"
)

// allTargets is the set of valid routing targets (also enforced by the CRD enum).
var allTargets = map[Target]bool{TargetConsumer: true, TargetProvider: true, TargetHost: true}

// ParseTarget validates a raw target string. Empty ⇒ TargetConsumer (the default).
func ParseTarget(s string) (Target, error) {
	if s == "" {
		return TargetConsumer, nil
	}
	t := Target(s)
	if !allTargets[t] {
		return "", fmt.Errorf("invalid target %q (want %q, %q or %q)", s, TargetConsumer, TargetProvider, TargetHost)
	}

	return t, nil
}

// TargetForNode resolves a node's routing target from the build-time routing map
// (keyed by resource id == node.Spec.Meta.ID), defaulting to consumer when absent.
func TargetForNode(id string, routing map[string]Target) Target {
	if t, ok := routing[id]; ok {
		return t
	}

	return TargetConsumer
}
