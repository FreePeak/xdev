package main

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// The `impact` tool is only honest if it is absent when there is no graph
// behind it. Advertising a tool that can never answer teaches the model to
// reach for it and come back empty, which is worse than not offering it.

func newGraphTestRegistry(t *testing.T, s *config.Settings) *tool.Registry {
	t.Helper()
	// nil provider/model: newToolRegistry only stores them for tool
	// constructors that need a live provider (imagegen, computer). The
	// registration decisions under test never dial out.
	return newToolRegistry(t.TempDir(), nil, "test", "test-model", s, nil, nil)
}

func TestImpactIsAbsentWithoutAConfiguredGraph(t *testing.T) {
	reg := newGraphTestRegistry(t, &config.Settings{})
	if _, ok := reg.Get(tool.ImpactToolName); ok {
		t.Fatal("impact must not be registered when no code graph is configured")
	}
	for _, e := range reg.Deferred() {
		if e.Name == tool.ImpactToolName {
			t.Fatal("impact must not appear in the deferred index either")
		}
	}
}

func TestImpactIsDeferredWhenAGraphIsConfigured(t *testing.T) {
	reg := newGraphTestRegistry(t, &config.Settings{
		CodeGraph: config.CodeGraphSettings{BaseURL: "http://127.0.0.1:9700", Project: "xdev"},
	})
	if _, ok := reg.Get(tool.ImpactToolName); !ok {
		t.Fatal("impact must be registered when a graph is configured")
	}
	// Deferred, not eager: it must cost the prompt nothing until the model
	// asks for it, which is the whole point of the catalog.
	for _, d := range reg.Defs() {
		if d.Name == tool.ImpactToolName {
			t.Fatal("impact must stay out of the eager tool schema")
		}
	}
	idx := ""
	for _, e := range reg.Deferred() {
		if e.Name == tool.ImpactToolName {
			idx = e.Index
		}
	}
	if !strings.Contains(idx, "callers") {
		t.Fatalf("the catalog line must teach what the tool answers, got %q", idx)
	}
}

// TestCodeGraphExpectDirFallsBackToGitRoot pins the identity guard's default:
// with no configured expectDir, the session's git root is what the server's
// project selector is supposed to resolve to.
func TestCodeGraphExpectDirFallsBackToGitRoot(t *testing.T) {
	dir := t.TempDir()
	if got := codeGraphExpectDir(config.CodeGraphSettings{}, dir); got != "" {
		t.Fatalf("a non-repository must yield no expectation (got %q) so the check disables rather than guesses", got)
	}
	// An explicit value always wins, even when it is nonsense -- the user
	// said so.
	if got := codeGraphExpectDir(config.CodeGraphSettings{ExpectDir: "/x"}, dir); got != "/x" {
		t.Fatalf("configured expectDir = %q, want /x", got)
	}
}

// The map-first guidance rides in the deferred catalog line rather than in the
// base system prompt. That is not a stylistic choice: the bundled prompt was
// measured at ~993 tokens against the PRD's 1,000-token goal
// (TestBundledPromptStaysUnderBudget), so there is no room for a rule block,
// and a rule block would be the first thing to push the default over.

// TestNoGraphMeansNoGraphProse keeps the default honest: a run without a code
// graph is not told about one, and stays inside the budget the
// pi-minimalism goal is measured against.
func TestNoGraphMeansNoGraphProse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", &config.Settings{}, nil, nil)
	got := promptFn(basePrompt(printOptions{}, t.TempDir()), t.TempDir(), reg, "")()
	if strings.Contains(got, "impact") {
		t.Fatalf("a run without a code graph must not mention the tool:\n%s", got)
	}
	if tokens := len([]rune(got)) / 4; tokens >= maxPromptTokens {
		t.Fatalf("default prompt is ~%d tokens (budget %d)", tokens, maxPromptTokens)
	}
}

// TestGraphPromptCarriesTheGuidanceAndStaysBounded pins what enabling the
// graph actually costs. The opt-in integration puts the prompt a little past
// the bundled goal, and that drift must stay bounded rather than grow with
// every deferred tool added after it.
func TestGraphPromptCarriesTheGuidanceAndStaysBounded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", &config.Settings{
		CodeGraph: config.CodeGraphSettings{BaseURL: "http://127.0.0.1:9700", Project: "xdev"},
	}, nil, nil)
	got := promptFn(basePrompt(printOptions{}, t.TempDir()), t.TempDir(), reg, "")()
	if !strings.Contains(got, "impact") {
		t.Fatal("a configured graph must be discoverable in the prompt")
	}
	// The line has to teach the when, or the model will not reach for the
	// tool before the edit it exists to precede.
	if !strings.Contains(got, "before editing shared code") {
		t.Fatalf("the catalog line lost its guidance:\n%s", got)
	}
	const graphPromptCeiling = 1100
	if tokens := len([]rune(got)) / 4; tokens > graphPromptCeiling {
		t.Fatalf("graph-enabled prompt is ~%d tokens (ceiling %d): trim the catalog line",
			tokens, graphPromptCeiling)
	}
}
