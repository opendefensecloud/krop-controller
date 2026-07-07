// internal/engine/apply_test.go
package engine

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeApplier records applied objects and echoes them back as "observed",
// optionally injecting extra status fields to simulate a controller populating
// the object after apply. It is the test double for the engine loop tests.
type fakeApplier struct {
	applied []*unstructured.Unstructured
	// mutate, if set, is called on a deep copy of the applied object to produce
	// the observed object (e.g. to set a status field a readyWhen depends on).
	mutate func(*unstructured.Unstructured)
}

func (f *fakeApplier) Apply(_ context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.applied = append(f.applied, o.DeepCopy())
	observed := o.DeepCopy()
	if f.mutate != nil {
		f.mutate(observed)
	}
	return observed, nil
}

func TestFakeApplier_SatisfiesInterface(t *testing.T) {
	var _ Applier = &fakeApplier{}
	obs, err := (&fakeApplier{}).Apply(context.Background(), obj(nil))
	if err != nil || obs == nil {
		t.Fatalf("apply returned %v,%v", obs, err)
	}
}
