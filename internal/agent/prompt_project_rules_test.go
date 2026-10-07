package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// flat collapses the header's line wrapping so a needle may straddle a
// newline: the header is wrapped prose, not a fixed layout.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestProjectContextHeaderStatesTheRulesAreBinding guards the fix for the
// rules-compliance gap measured in the 42-run DSH-vs-xdev head-to-head.
//
// The block used to be headed "# Project context", which reads as background
// material. On t07-rules-only-discovery — the one task whose rules exist ONLY
// in AGENTS.md and nowhere in the prompt — xdev scored 9.3/10 against the
// competitor's 10/10, and the single failing run (invisible_rule_changelog)
// was the single run that never opened the file. Every other task restates its
// rules in the prompt, so nothing else in the suite can catch this.
//
// Each needle below is the specific claim whose absence lets the model treat
// the block as advisory. Drop one and this test fails instead of the gap
// quietly returning.
func TestProjectContextHeaderStatesTheRulesAreBinding(t *testing.T) {
	h := flat(strings.ToLower(ProjectContextHeader))
	for _, want := range []struct {
		needle string
		why    string
	}{
		{"binding", `"project context" carried no authority word, so the block read as advisory`},
		{"whether or not the task restates them", "without this the model assumes a silent task prompt cancels an unstated rule"},
		{"has not cancelled it", "the prompt gives no reason to keep a rule the task did not repeat"},
		{"take precedence", "the header must not read as overriding the user, or it is simply wrong"},
	} {
		if !strings.Contains(h, want.needle) {
			t.Errorf("ProjectContextHeader is missing %q — %s", want.needle, want.why)
		}
	}
}

// TestProjectContextHeaderBoundsAProcessRule locks the second half of the
// overthinking fix. Stating that a repository's rules are binding (the test
// above) is what makes them survive a task prompt that does not repeat them —
// and it is also what turned a 6-word ask into a delivery pipeline, because
// every repository keeps rules that describe a PROCESS (a worktree, a branch,
// a review, a ticket) and an always-loaded file states them without saying
// what they are for. The cheapest reading of "binding" is "always", so the
// header has to bound the reach as well as state the rank.
//
// Measured on this repo's own store: 44 of the 80 newest sessions ran `git
// worktree` — one of them 94 times — for asks like "the thinking box color in
// xdev" and "move the mcp and trajectory in sidebar to bottom". Each needle
// below is a part of that bound: "scope" says a rule has one, the process
// examples name what a process rule looks like, and "scale the ceremony" is
// the instruction the model acts on.
func TestProjectContextHeaderBoundsAProcessRule(t *testing.T) {
	h := flat(strings.ToLower(ProjectContextHeader))
	for _, want := range []struct {
		needle string
		why    string
	}{
		{"scope", "a binding rule with unbounded reach is read as applying to every message"},
		{"worktree", "the process rule the field report actually hit is never named"},
		{"not every message", "nothing says a process rule has a smaller reach than the whole session"},
		{"scale the ceremony", "the model is given the principle but no instruction to act on"},
	} {
		if !strings.Contains(h, want.needle) {
			t.Errorf("ProjectContextHeader is missing %q — %s", want.needle, want.why)
		}
	}
}

// TestProjectContextHeaderIsNotLoud keeps the other half of the deal: the
// block is a repository's rules, not a second system prompt, and the header
// must not blur that. A header that claims to override the user or the system
// would be a correctness regression traded for a compliance rate.
func TestProjectContextHeaderIsNotLoud(t *testing.T) {
	h := strings.ToLower(ProjectContextHeader)
	for _, unwanted := range []string{
		"override the system",
		"overrides the user",
		"always correct",
		"must obey",
	} {
		if strings.Contains(h, unwanted) {
			t.Errorf("ProjectContextHeader contains %q — repository rules are not system rules; say what they are, not what they outrank", unwanted)
		}
	}
}

// TestProjectContextBlockFramesBothInjectionSites is the anti-drift guard.
// The parent prompt (BuildSystemPrompt) and a subagent's (childSystem) both
// render this block; if either reverts to a bare heading while the other
// keeps the framing, the compliance fix silently applies to one arm only —
// which is exactly how the original measurement became untrustworthy.
func TestProjectContextBlockFramesBothInjectionSites(t *testing.T) {
	files := "## repo/AGENTS.md\n\nShip a test."
	block := ProjectContextBlock(files)
	if !strings.HasPrefix(block, ProjectContextHeader) {
		t.Error("ProjectContextBlock does not lead with ProjectContextHeader")
	}
	if !strings.Contains(block, files) {
		t.Error("ProjectContextBlock dropped the injected content")
	}
	if got := ProjectContextBlock(""); got != "" {
		t.Errorf("ProjectContextBlock(\"\") = %q, want empty — a repo with no AGENTS.md must get no empty heading", got)
	}

	// The parent arm.
	parent := BuildSystemPrompt("base", files, nil)
	if !strings.Contains(parent, ProjectContextHeader) {
		t.Error("BuildSystemPrompt no longer frames the context block with the binding header")
	}
	// The subagent arm. HOME is redirected because LoadContextFiles also picks
	// up the developer's real ~/.xdev/agent/AGENTS.md, which would otherwise
	// make the "no context files" case depend on the machine.
	t.Setenv("HOME", t.TempDir())
	if got := childSystem(SubagentSpec{System: "child base"}, t.TempDir()); got != "child base" {
		t.Errorf("childSystem with no context files = %q, want the spec verbatim", got)
	}

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(files), 0o600); err != nil {
		t.Fatal(err)
	}
	withFiles := childSystem(SubagentSpec{System: "child base"}, repo)
	if !strings.Contains(withFiles, ProjectContextHeader) {
		t.Error("childSystem no longer frames the context block with the binding header")
	}
	if !strings.Contains(withFiles, "child base") || !strings.Contains(withFiles, files) {
		t.Error("childSystem must append the rules to the spec prompt, never replace it")
	}
}
