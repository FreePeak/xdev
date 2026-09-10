package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandImports: @path resolves relative to the importing file,
// cycles stay literal, missing targets stay literal, depth is bounded.
func TestExpandImports(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("root.md", "root rules\n@sub/child.md\nuser@host.com stays")
	write("sub/child.md", "child rules\n@root.md") // cycle back to root
	write("sub/leaf.md", "leaf rules")

	budget := MaxContextBytes
	got := expandImports("see @root.md and @sub/leaf.md and @missing.md", dir, &budget, map[string]bool{})
	for _, want := range []string{"root rules", "child rules", "leaf rules", "user@host.com stays", "@missing.md"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expansion missing %q:\n%s", want, got)
		}
	}
	// The cycle back to root.md must appear literally, not expand again.
	if strings.Count(got, "root rules") != 1 {
		t.Fatalf("cycle expanded root.md twice:\n%s", got)
	}
}
