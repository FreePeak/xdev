package agent

import (
	"os"
	"testing"
)

func TestResolveMaxContextTokensDefault(t *testing.T) {
	_ = os.Unsetenv(MaxContextTokensEnv)
	if got := ResolveMaxContextTokens(); got != MaxContextTokensDefault {
		t.Fatalf("default = %d, want %d", got, MaxContextTokensDefault)
	}
}

func TestResolveMaxContextTokensEnv(t *testing.T) {
	t.Setenv(MaxContextTokensEnv, "500000")
	if got := ResolveMaxContextTokens(); got != 500000 {
		t.Fatalf("env = %d, want 500000", got)
	}
}

func TestResolveMaxContextTokensEmptyFallback(t *testing.T) {
	t.Setenv(MaxContextTokensEnv, "")
	if got := ResolveMaxContextTokens(); got != MaxContextTokensDefault {
		t.Fatalf("empty env = %d, want %d", got, MaxContextTokensDefault)
	}
}

func TestResolveMaxContextTokensInvalidFallback(t *testing.T) {
	t.Setenv(MaxContextTokensEnv, "not-a-number")
	if got := ResolveMaxContextTokens(); got != MaxContextTokensDefault {
		t.Fatalf("invalid env = %d, want %d", got, MaxContextTokensDefault)
	}
}
