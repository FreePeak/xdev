package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chainFixture builds a root→cwd directory chain, each level carrying an
// AGENTS.md whose body is `body(level, n)`, and returns the deepest dir.
func chainFixture(t *testing.T, root string, sizes []int) string {
	t.Helper()
	dir := root
	for i, n := range sizes {
		if i > 0 {
			dir = filepath.Join(dir, fmt.Sprintf("lvl%d", i))
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := strings.Repeat(fmt.Sprintf("root level %d rule. ", i), n/23+1)
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# rules\n\n"+body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestContextBudgetKeepsTheMostSpecificRules is the fix for the inversion.
//
// The chain is root→cwd and precedence runs the opposite way: the CWD's own
// rules override its ancestors. The pre-fix loop walked the chain in order
// and `break`ed once MaxContextBytes was spent, so an overflowing chain kept
// the broad root rules and dropped the CWD's file — the most specific
// instructions in the repository, the ones that say which of the ancestor
// rules do not apply.
func TestContextBudgetKeepsTheMostSpecificRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	// Root alone overruns the budget; every deeper level must still arrive.
	// 4 files x ~12 KB each = ~48 KB against a 32 KB budget.
	cwd := chainFixture(t, root, []int{12000, 12000, 12000, 12000})

	out := LoadContextFiles(cwd)
	if out == "" {
		t.Fatal("LoadContextFiles returned nothing for a chain of four files")
	}

	// The deepest file is the one that must never be the casualty.
	deepest := filepath.Join(cwd, "AGENTS.md")
	if !strings.Contains(out, deepest) {
		t.Errorf("the CWD's own AGENTS.md was dropped from the prompt — the ancestor rules that it overrides were kept instead\n--- output ---\n%s", head(out, 600))
	}
	if !strings.Contains(out, "level 3 rule") {
		t.Errorf("the closest rules file's content is missing\n--- output ---\n%s", head(out, 600))
	}

	// The broad root file is the correct thing to lose.
	rootFile := filepath.Join(root, "AGENTS.md")
	if strings.Contains(out, "Global conventions ("+rootFile+")") {
		t.Error("the broadest ancestor file survived an overflow that could have been paid by it instead")
	}
	// And the loss must be declared, not silent.
	if !strings.Contains(out, "Rules budget reached") {
		t.Error("files were dropped with no marker — a silently-shortened rules list is indistinguishable from a complete one")
	}
	if !strings.Contains(out, rootFile) {
		t.Error("the marker does not name the omitted path, so the model cannot go read it")
	}
}

// TestContextBudgetOrderingIsStillRootToCwd guards the precedence invariant
// the fit pass could easily break: it walks specific→first to decide what
// fits, so the rendered block has to be put back in root→cwd order. A block
// with the general rule last reads as the more specific one.
func TestContextBudgetOrderingIsStillRootToCwd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	// Small enough that nothing is dropped: this is the ordinary case.
	cwd := chainFixture(t, root, []int{40, 40})

	out := LoadContextFiles(cwd)
	if !strings.Contains(out, "level 0 rule") || !strings.Contains(out, "level 1 rule") {
		t.Fatalf("both levels should be present\n--- output ---\n%s", out)
	}
	iRoot := strings.Index(out, "level 0 rule")
	iCwd := strings.Index(out, "level 1 rule")
	if iRoot < 0 || iCwd < 0 || iRoot > iCwd {
		t.Errorf("rules are not in root→cwd order (root at %d, cwd at %d) — the more specific rule must come last so it wins", iRoot, iCwd)
	}
	if strings.Contains(out, "Rules budget reached") {
		t.Error("the budget marker rendered for a chain that fits — it would train the model to ignore it")
	}
}

// TestContextBudgetFitsASmallAncestorSkippedByABigOne is the mid-size case
// that the two-pass fit has to get right: the CWD's file is too big for the
// budget on its own, and the root file is small enough to fit alongside it.
// The right answer keeps both. Pre-fix, the budget filled with the root file
// first and the CWD file was never read.
func TestContextBudgetFitsASmallAncestorSkippedByABigOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	// cwd ~ 20 KB, root ~ 1 KB: together under 32 KB.
	cwd := chainFixture(t, root, []int{1000, 20000})

	out := LoadContextFiles(cwd)
	if !strings.Contains(out, "level 1 rule") {
		t.Error("the CWD's rules were dropped even though the chain fits the budget")
	}
	if !strings.Contains(out, "level 0 rule") {
		t.Error("the small root file was dropped even though both files fit — a skipped ancestor must not end the walk")
	}
	if strings.Contains(out, "Rules budget reached") {
		t.Error("the budget marker rendered for a chain that fits")
	}
}

// TestContextBudgetTruncatesRatherThanDropsTheClosestFile is the floor case.
// If the CWD's own file alone exceeds the budget, dropping it would leave the
// session holding only the ancestors' rules — the exact inversion this change
// exists to remove, one level down. It is injected truncated, and says so.
func TestContextBudgetTruncatesRatherThanDropsTheClosestFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	cwd := chainFixture(t, root, []int{500, 60000}) // cwd alone is > 32 KB

	out := LoadContextFiles(cwd)
	deepest := filepath.Join(cwd, "AGENTS.md")
	if !strings.Contains(out, deepest) {
		t.Fatalf("the CWD's own rules file was dropped entirely — the model would keep only the ancestor's rules\n--- head ---\n%s", head(out, 400))
	}
	// Assert on the rendered RULES, not the path: the budget marker names
	// omitted paths, so a path-only assertion passes even when the file itself
	// was dropped — which is the exact bug this guards. A path-only version of
	// this assertion survived mutation; this one does not.
	if !strings.Contains(out, "level 1 rule") {
		t.Errorf("the CWD's rules CONTENT is absent — the file was dropped, not truncated\n--- head ---\n%s", head(out, 400))
	}
	if !strings.Contains(out, "truncated") {
		t.Error("a truncated rules file does not say so")
	}
	if !strings.Contains(out, "Rules budget reached") {
		t.Error("the truncation is not declared to the model")
	}
	// Still bounded.
	if n := len(out); n > MaxContextBytes+512 {
		t.Errorf("output is %d bytes, well past the %d budget — truncation did not hold", n, MaxContextBytes)
	}
	// Rune safety: the cut must not land inside a multi-byte character.
	for _, r := range out {
		if r == '\uFFFD' {
			t.Error("output contains a replacement character — the byte cut split a rune")
			break
		}
	}
}

// TestContextBudgetEmptyChainIsUnchanged keeps the zero case honest: no
// files means no marker and no headings.
func TestContextBudgetEmptyChainIsUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if out := LoadContextFiles(t.TempDir()); out != "" {
		t.Errorf("LoadContextFiles with no AGENTS.md = %q, want empty", head(out, 200))
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("… [%d bytes total]", len(s))
}
