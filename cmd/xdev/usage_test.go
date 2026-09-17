package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUsageDocumentsEverySubcommand guards two failure modes found on
// 2026-09-12: a merge artifact shipping a literal "@both" line in the help
// every user reads, and the help silently drifting from the dispatch table
// (13 subcommands were undocumented at once). The usage text must name every
// subcommands entry (aliases may share a line, so the check is
// contains-per-name, not one-line-per-name).
func TestUsageDocumentsEverySubcommand(t *testing.T) {
	for name := range subcommands {
		if !strings.Contains(rootUsage, name) {
			t.Errorf("subcommand %q is dispatchable but the root usage does not mention it (help drift: a user cannot discover it)", name)
		}
	}
}

// TestNoMergeMarkersInSource guards the other half of the same failure: a
// merge artifact reaching users through any string the binary prints.
// Scans the package's non-test sources for the three conflict markers; if a
// future merge produces one, this names the file and line.
// Repo-wide (docs, fixtures, every package) is repohygiene_test.go's job;
// that one exists because the package-local glob below could not see the
// conflict marker #311 committed into docs/PRD.md.
func TestNoMergeMarkersInSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, marker := range []string{"@both", "@ours", "@theirs"} {
				if strings.Contains(line, marker) {
					t.Errorf("%s:%d contains merge marker %q", f, i+1, marker)
				}
			}
		}
	}
}
