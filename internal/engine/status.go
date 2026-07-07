// internal/engine/status.go
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

	return desired[0], nil
}
