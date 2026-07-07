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

// internal/engine/graphsource_test.go
package engine

import "testing"

// The production EndpointGraphSource wraps graph.NewBuilder, which needs a live
// discovery/OpenAPI endpoint, so we can only assert its construction contract
// here; the real build against a workspace is validated in test/e2e (Task 11).
func TestEndpointGraphSource_NilConfigErrors(t *testing.T) {
	if _, err := NewEndpointGraphSource(nil); err == nil {
		t.Fatal("want error constructing EndpointGraphSource with nil rest.Config")
	}
}
