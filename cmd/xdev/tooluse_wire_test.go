package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/memory"
	"github.com/FreePeak/xdev/internal/tool"
)

// The tool-usage recipes reach the prompt: what one session learned about
// a tool, the next session reads before it trips over the same thing.

func seedToolUseRecipe(t *testing.T, toolName, symptom, fix string) {
	t.Helper()
	store := memory.SharedToolUse(toolUseDir())
	if store == nil {
		t.Fatal("no tool-use store for the agent dir")
	}
	if err := store.NoteFailure(toolName, symptom, "{}"); err != nil {
		t.Fatal(err)
	}
	if err := store.NoteFix(toolName, symptom, fix, ""); err != nil {
		t.Fatal(err)
	}
}

func TestToolUseRecipesReachThePrompt(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	seedToolUseRecipe(t, "read", "file not found: notes.md", `path: "notes.md" → "/abs/notes.md"`)

	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	got := promptFn("BASE", t.TempDir(), reg, "")()

	if !strings.Contains(got, "# Tool Usage") {
		t.Fatalf("the tool-usage block is missing from the prompt:\n%s", got)
	}
	if !strings.Contains(got, "file not found: notes.md") {
		t.Fatalf("the recipe lost its symptom:\n%s", got)
	}
	if !strings.Contains(got, "/abs/notes.md") {
		t.Fatalf("the recipe lost its working form:\n%s", got)
	}
}

// The block names the tools it talks about, so a recipe for a tool this
// session does not have must not ride its prompt.
func TestToolUseRecipeNeedsTheTool(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	seedToolUseRecipe(t, "read", "file not found: notes.md", `path: "notes.md" → "/abs/notes.md"`)

	bare := tool.NewRegistry()
	bare.Register(tool.NewWriteTool())
	got := promptFn("BASE", t.TempDir(), bare, "")()
	if strings.Contains(got, "# Tool Usage") {
		t.Fatalf("a recipe for a tool the session lacks must not reach the prompt:\n%s", got)
	}
}

// A session with nothing remembered pays nothing: no block, no prompt
// budget.
func TestToolUseBlockIsEmptyWithoutRecipes(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	reg := tool.NewRegistry()
	reg.Register(tool.NewReadTool())
	got := promptFn("BASE", t.TempDir(), reg, "")()
	if strings.Contains(got, "# Tool Usage") {
		t.Fatalf("an empty store must not add a block:\n%s", got)
	}
}

// wireAgentMode is the one place every build site goes through, so the
// tool-usage learning is wired there — and the setting turns it off.
func TestWireAgentModeWiresTheToolLearning(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	on := &agent.Agent{}
	wireAgentMode(on, nil, &config.Settings{}, "p", "m", t.TempDir(), false)
	if on.ToolLessons == nil {
		t.Fatal("every session must get the tool-usage learning")
	}

	off := false
	disabled := &agent.Agent{}
	wireAgentMode(disabled, nil, &config.Settings{ToolLessons: &off}, "p", "m", t.TempDir(), false)
	if disabled.ToolLessons != nil {
		t.Fatal("toolLessons: false must turn the learning off")
	}
}

// `xdev memory recipes` lists what the session learned: the failures and
// the recipes that answered them.
func TestMemoryRecipesVerbListsTheRecords(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	seedToolUseRecipe(t, "read", "file not found: notes.md", `path: "notes.md" → "/abs/notes.md"`)

	code, out, errOut := runMemory(t, "", "recipes")
	if code != 0 {
		t.Fatalf("recipes = %d (%s)", code, errOut)
	}
	if !strings.Contains(out, "read") || !strings.Contains(out, "file not found: notes.md") {
		t.Fatalf("recipes output lost the record:\n%s", out)
	}
	if !strings.Contains(out, "fixed by") {
		t.Fatalf("recipes output lost the recipe:\n%s", out)
	}

	code, out, errOut = runMemory(t, "", "recipes", "--json")
	if code != 0 {
		t.Fatalf("recipes --json = %d (%s)", code, errOut)
	}
	var recs []memory.ToolUseRecord
	if err := json.Unmarshal([]byte(out), &recs); err != nil {
		t.Fatalf("recipes --json is not JSON: %v\n%s", err, out)
	}
	if len(recs) != 1 || recs[0].Tool != "read" {
		t.Fatalf("recipes --json = %+v", recs)
	}
}

// The records are a local store: the verb reads them whichever backend is
// configured (with hindsight on, the backend's own memories are on the
// server and the recipes are still here).
func TestMemoryRecipesVerbReadsTheLocalStore(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	seedToolUseRecipe(t, "read", "file not found: notes.md", `path: "notes.md" → "/abs/notes.md"`)

	var out, errOut bytes.Buffer
	code := memoryCmd([]string{"recipes"}, strings.NewReader(""), &out, &errOut, &config.Settings{Memory: "hindsight"})
	if code != 0 {
		t.Fatalf("recipes with the hindsight backend = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "file not found: notes.md") {
		t.Fatalf("recipes output lost the record:\n%s", out.String())
	}
}
