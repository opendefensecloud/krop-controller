// internal/engine/apply.go
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
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// FieldManager is the server-side-apply field owner for all engine writes.
const FieldManager = "krop-controller"

// Applier applies one desired object into a single target workspace and returns
// the object as observed afterwards (server-side apply result, read back). The
// engine owns all I/O through this interface, keeping the reconcile loop
// client-agnostic and unit-testable.
type Applier interface {
	Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}
