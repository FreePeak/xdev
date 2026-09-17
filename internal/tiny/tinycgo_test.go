//go:build tinycgo

package tiny

import (
	"context"
	"errors"
	"testing"
)

// The opt-in artifact must compile and behave: the tag wires a backend, and
// with no runtime linked that backend still refuses with ErrNotBuilt instead
// of degrading to the API.
func TestTinyCGOBuildStillRefusesWithoutRuntime(t *testing.T) {
	b := Backend()
	if b == nil {
		t.Fatal("Backend() = nil under -tags tinycgo")
	}
	if b.Available() {
		t.Fatal("tinycgo placeholder claims availability; it links no runtime")
	}
	if _, err := b.Complete(context.Background(), "prompt"); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Complete err = %v, want ErrNotBuilt", err)
	}
	t.Setenv(EnvLocal, "on")
	if got, err := Select(TaskMemoryExtract, nil); got != nil || !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Select = (%v, %v), want (nil, ErrNotBuilt)", got, err)
	}
}
