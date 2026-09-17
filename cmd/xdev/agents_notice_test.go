package main

import (
	"fmt"
	"strings"
	"testing"
)

// #272: the TUI's alt screen swallowed every discovery diagnostic, so an
// empty agent set was indistinguishable from a working one. The startup
// notice is the user-facing surface; it must state the count and the names.
func TestTaskAgentsAtStartupNotice(t *testing.T) {
	cwd := t.TempDir()
	notice, warnings, count := taskAgentsAtStartup(cwd)
	if len(warnings) != 0 {
		t.Fatalf("stock discovery warns: %v", warnings)
	}
	// The issue's headline: a stock install discovers the bundled set.
	if count < 3 {
		t.Fatalf("stock install discovered %d agents, want the bundled set", count)
	}
	for _, want := range []string{"task agents: " + fmt.Sprint(count), "scout", "reviewer", "sonic", "task"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice %q omits %q", notice, want)
		}
	}
}
