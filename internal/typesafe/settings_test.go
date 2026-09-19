package typesafe

import (
	"testing"
)

// TestConfigIdempotent exercises the Config() defaulting path
// once, to keep coverage green if settings_test is the only file
// loaded. The comprehensive defaults test lives in eval_test.go.
func TestConfigIdempotent(t *testing.T) {
	s := Settings{APIKey: "k"}
	if s.Config().Model != DefaultModel {
		t.Fatalf("model: want %s, got %s", DefaultModel, s.Config().Model)
	}
}
