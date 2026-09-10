package agent

import (
	"github.com/FreePeak/xdev/internal/tool"
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

// TestBuildSystemPromptCapsRemoteDescriptions pins the choke-point cap:
// MCP/extension prose arrives at whatever length its author chose and must
// never land in the prompt whole.
func TestBuildSystemPromptCapsRemoteDescriptions(t *testing.T) {
	verbose := strings.Repeat("detailed remote tool documentation. ", 200)
	got := BuildSystemPrompt("base", "", []NamedToolDef{
		{Name: "chatty", Description: verbose},
		{Name: "terse", Description: "short one"},
	})
	if strings.Contains(got, verbose) {
		t.Fatalf("uncapped remote prose landed in the prompt (%d chars)", len(verbose))
	}
	if !strings.Contains(got, "chatty:") || !strings.Contains(got, "terse: short one") {
		t.Fatalf("tools missing from the prompt:\n%s", got)
	}
	// Rune-safe cut: the marker terminates the line and no replacement
	// char is emitted mid-codepoint.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "chatty:") {
			if !strings.HasSuffix(line, "…") {
				t.Fatalf("cap marker missing: %q", line)
			}
			if len([]rune(line)) > MaxToolDescriptionChars+len("chatty: ")+1 {
				t.Fatalf("line longer than the cap: %d runes", len([]rune(line)))
			}
		}
	}
}

// TestBundledToolsFitTheCap guards against silent clipping: a CORE tool
// whose description exceeds MaxToolDescriptionChars would lose its
// semantics for every provider call, and the budget tests still pass
// either way. Bundled prose must fit by design.
func TestBundledToolsFitTheCap(t *testing.T) {
	reg := tool.NewRegistry()
	for _, td := range []tool.Tool{
		tool.NewReadTool(), tool.NewWriteTool(), tool.NewEditTool(), tool.NewBashTool(t.TempDir()),
	} {
		reg.Register(td)
	}
	for _, d := range reg.Defs() {
		if n := len([]rune(d.Description)); n > MaxToolDescriptionChars {
			t.Fatalf("core tool %q has a %d-char description (cap %d): it is silently clipped in the prompt",
				d.Name, n, MaxToolDescriptionChars)
		}
	}
}
