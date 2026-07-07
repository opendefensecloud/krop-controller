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
