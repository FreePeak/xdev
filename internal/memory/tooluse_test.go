package memory

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// The tool-usage loop: a failed call is recorded, the agent's recovery
// becomes the recipe, and the recipe is what later sessions read.

func toolCall(name, args string) ai.ToolCallBlock {
	return ai.ToolCallBlock{ID: "c1", Name: name, Arguments: json.RawMessage(args)}
}

func newTracker(t *testing.T) (*ToolUseTracker, *ToolUseStore) {
	t.Helper()
	store := &ToolUseStore{Dir: filepath.Join(t.TempDir(), "memory")}
	return NewToolUseTracker(store, nil), store
}

func TestToolUseTrackerLearnsRecipeFromRecovery(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("read", `{"path":"notes.md"}`),
		tool.Result{Text: "file not found: notes.md", IsError: true})
	tr.ObserveTool(toolCall("read", `{"path":"/abs/notes.md"}`), tool.Result{Text: "ok"})

	recipes := store.Recipes()
	if len(recipes) != 1 {
		t.Fatalf("want one recipe, got %d: %+v", len(recipes), recipes)
	}
	if recipes[0].Tool != "read" {
		t.Fatalf("the recipe must name the tool: %+v", recipes[0])
	}
	if !strings.Contains(recipes[0].Symptom, "file not found") {
		t.Fatalf("the recipe must keep the symptom: %+v", recipes[0])
	}
	if !strings.Contains(recipes[0].Fix, "/abs/notes.md") {
		t.Fatalf("the recipe must name the working form: %+v", recipes[0])
	}

	block := store.Block(nil)
	if !strings.Contains(block, "# Tool Usage") || !strings.Contains(block, "read") {
		t.Fatalf("the block must carry the recipe:\n%s", block)
	}
}

// A failure followed by a DIFFERENT call that happens to work says
// nothing about how to use the tool.
func TestToolUseTrackerIgnoresUnrelatedSuccess(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("read", `{"path":"a.md"}`),
		tool.Result{Text: "file not found: a.md", IsError: true})
	tr.ObserveTool(toolCall("bash", `{"command":"ls"}`), tool.Result{Text: "ok"})
	if got := store.Recipes(); len(got) != 0 {
		t.Fatalf("another tool's success is not a recipe: %+v", got)
	}
}

// The same call failed and then worked: a transient failure, not a
// lesson.
func TestToolUseTrackerIgnoresTransientFailure(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("bash", `{"command":"curl example.com"}`),
		tool.Result{Text: "connection reset", IsError: true})
	tr.ObserveTool(toolCall("bash", `{"command":"curl example.com"}`), tool.Result{Text: "ok"})
	if got := store.Recipes(); len(got) != 0 {
		t.Fatalf("an unchanged call is not a recipe: %+v", got)
	}
}

// A denied, blocked or abandoned call is not a usage mistake: there is no
// form of it that would have worked.
func TestToolUseTrackerIgnoresHarnessOutcomes(t *testing.T) {
	for _, text := range []string{
		"tool call refused by user: needs approval",
		"tool call denied: outside the workspace",
		"tool call blocked: policy",
		`tool "read" canceled before execution: context canceled`,
		`tool "read" did not finish within 2m0s and was abandoned`,
	} {
		tr, store := newTracker(t)
		tr.ObserveTool(toolCall("read", `{"path":"a.md"}`), tool.Result{Text: text, IsError: true})
		if got := store.Records(); len(got) != 0 {
			t.Fatalf("a harness outcome is not a failure to remember: %q -> %+v", text, got)
		}
	}
}

// The failure is remembered even when the agent never finds a way
// through: the record is what tells a later session the call is a dead
// end.
func TestToolUseTrackerRecordsUnresolvedFailure(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("read", `{"path":"a.md"}`),
		tool.Result{Text: "file not found: a.md", IsError: true})
	recs := store.Records()
	if len(recs) != 1 {
		t.Fatalf("the failure must be recorded: %+v", recs)
	}
	if recs[0].Fix != "" {
		t.Fatalf("an unresolved failure has no recipe yet: %+v", recs[0])
	}
	if recs[0].Hits != 1 {
		t.Fatalf("the record must count the hits: %+v", recs[0])
	}
	// An unresolved failure is not injected: there is nothing to teach.
	if block := store.Block(nil); block != "" {
		t.Fatalf("an unresolved failure must not reach the prompt:\n%s", block)
	}
}

// The same symptom twice is one record with two hits.
func TestToolUseTrackerCountsRepeatedSymptom(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("read", `{"path":"a.md"}`),
		tool.Result{Text: "file not found: a.md", IsError: true})
	tr.ObserveTool(toolCall("read", `{"path":"b.md"}`),
		tool.Result{Text: "file not found: a.md", IsError: true})
	recs := store.Records()
	if len(recs) != 1 {
		t.Fatalf("the same symptom is one record: %+v", recs)
	}
	if recs[0].Hits != 2 {
		t.Fatalf("the record must count the hits: %+v", recs[0])
	}
}

// The mistake worth remembering most: the argument was RENAMED.
func TestToolUseRecipeNamesRenamedArgument(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("read", `{"file_path":"notes.md"}`),
		tool.Result{Text: "read: unknown parameter \"file_path\"", IsError: true})
	tr.ObserveTool(toolCall("read", `{"path":"notes.md"}`), tool.Result{Text: "ok"})

	recipes := store.Recipes()
	if len(recipes) != 1 {
		t.Fatalf("want one recipe, got %d: %+v", len(recipes), recipes)
	}
	fix := recipes[0].Fix
	if !strings.Contains(fix, "path") || !strings.Contains(fix, "file_path") {
		t.Fatalf("the recipe must name both keys: %q", fix)
	}
}

// The agent supplied what the failing call left out.
func TestToolUseRecipeNamesSuppliedArgument(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("read", `{}`), tool.Result{Text: "read: path is required", IsError: true})
	tr.ObserveTool(toolCall("read", `{"path":"notes.md"}`), tool.Result{Text: "ok"})

	recipes := store.Recipes()
	if len(recipes) != 1 {
		t.Fatalf("want one recipe, got %d: %+v", len(recipes), recipes)
	}
	if !strings.Contains(recipes[0].Fix, "path") {
		t.Fatalf("the recipe must name the supplied argument: %q", recipes[0].Fix)
	}
}

// The block names the tools it talks about, so it may only ride a prompt
// that HAS them.
func TestToolUseBlockFiltersToolsTheSessionLacks(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("read", `{"path":"a.md"}`),
		tool.Result{Text: "file not found: a.md", IsError: true})
	tr.ObserveTool(toolCall("read", `{"path":"/abs/a.md"}`), tool.Result{Text: "ok"})

	block := store.Block(func(name string) bool { return name == "bash" })
	if block != "" {
		t.Fatalf("a recipe for a tool the session lacks must not reach the prompt:\n%s", block)
	}
	if got := store.Block(func(name string) bool { return name == "read" }); got == "" {
		t.Fatal("the recipe must reach a session that has the tool")
	}
}

// A recipe quotes the call that failed, and a call can carry a token.
func TestToolUseTrackerRedactsSecrets(t *testing.T) {
	store := &ToolUseStore{Dir: filepath.Join(t.TempDir(), "memory")}
	secret := "ghp_live-token-value"
	tr := NewToolUseTracker(store, func(s string) string {
		return strings.ReplaceAll(s, secret, "$$REDACTED$$")
	})
	tr.ObserveTool(toolCall("bash", `{"command":"gh api -H 'auth: `+secret+`'"}`),
		tool.Result{Text: "gh: bad credentials", IsError: true})
	tr.ObserveTool(toolCall("bash", `{"command":"gh api -H 'auth: `+secret+`' --jq ."}`), tool.Result{Text: "ok"})

	raw := readFileOrEmpty(store.path())
	if strings.Contains(raw, secret) {
		t.Fatalf("the live credential was stored verbatim:\n%s", raw)
	}
	if !strings.Contains(raw, "$$REDACTED$$") {
		t.Fatalf("no redaction applied:\n%s", raw)
	}
}

// The symptom is the tool's own error text, and an error can quote the
// secret that failed.
func TestToolUseTrackerRedactsTheSymptom(t *testing.T) {
	store := &ToolUseStore{Dir: filepath.Join(t.TempDir(), "memory")}
	secret := "ghp_live-token-value"
	tr := NewToolUseTracker(store, func(s string) string {
		return strings.ReplaceAll(s, secret, "$$REDACTED$$")
	})
	tr.ObserveTool(toolCall("bash", `{"command":"gh api"}`),
		tool.Result{Text: "gh: bad credentials for " + secret, IsError: true})

	raw := readFileOrEmpty(store.path())
	if strings.Contains(raw, secret) {
		t.Fatalf("the live credential was stored verbatim:\n%s", raw)
	}
}

// The records outlive the process that wrote them: the next session reads
// them from the file.
func TestToolUseStoreRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	store := &ToolUseStore{Dir: dir}
	tr := NewToolUseTracker(store, nil)
	tr.ObserveTool(toolCall("read", `{"path":"a.md"}`),
		tool.Result{Text: "file not found: a.md", IsError: true})
	tr.ObserveTool(toolCall("read", `{"path":"/abs/a.md"}`), tool.Result{Text: "ok"})

	reopened := &ToolUseStore{Dir: dir}
	if got := reopened.Recipes(); len(got) != 1 {
		t.Fatalf("the recipe must survive the process: %+v", got)
	}
}

// A store keeps a bounded number of records.
func TestToolUseStoreKeepsBoundedRecords(t *testing.T) {
	store := &ToolUseStore{Dir: filepath.Join(t.TempDir(), "memory"), Max: 3}
	for i := 0; i < 6; i++ {
		if err := store.NoteFailure("read", "symptom-"+string(rune('a'+i)), "{}"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.Records()); got != 3 {
		t.Fatalf("want 3 records, got %d", got)
	}
}

// A garbled tool name is a tool-usage mistake too: the failure names the
// closest registered tool, and a call to that tool is the recovery.
func TestToolUseTrackerLearnsRenamedTool(t *testing.T) {
	tr, store := newTracker(t)
	tr.ObserveTool(toolCall("ash_edit", `{"file":"/tmp/a.go"}`),
		tool.Result{Text: `unknown tool "ash_edit"; the closest registered tool is "ast_edit" — resend the call as "ast_edit"`, IsError: true})
	tr.ObserveTool(toolCall("ast_edit", `{"file":"/tmp/a.go"}`), tool.Result{Text: "ok"})

	recipes := store.Recipes()
	if len(recipes) != 1 {
		t.Fatalf("want one recipe, got %d: %+v", len(recipes), recipes)
	}
	if recipes[0].FixTool != "ast_edit" {
		t.Fatalf("the recipe must name the tool to use: %+v", recipes[0])
	}
	if !strings.Contains(recipes[0].Fix, "ast_edit") {
		t.Fatalf("the recipe must name the tool to use: %q", recipes[0].Fix)
	}
	// The block filters on the tool the recipe recommends: the garbled
	// name is not a tool this session has, the real one is.
	block := store.Block(func(name string) bool { return name == "ast_edit" })
	if !strings.Contains(block, "ast_edit") {
		t.Fatalf("the recipe must reach a session that has the real tool:\n%s", block)
	}
	if empty := store.Block(func(name string) bool { return name == "ash_edit" }); empty != "" {
		t.Fatalf("the recipe must not ride a session without the real tool:\n%s", empty)
	}
}

// The block is bounded: the system prompt has a budget, and a recipe is
// worth a few hundred characters, not a few thousand.
func TestToolUseBlockStaysWithinItsBudget(t *testing.T) {
	store := &ToolUseStore{Dir: filepath.Join(t.TempDir(), "memory")}
	for i := 0; i < 40; i++ {
		symptom := "read: the call failed in a way that repeats itself over and over " + strconv.Itoa(i)
		if err := store.NoteFailure("read", symptom, "{}"); err != nil {
			t.Fatal(err)
		}
		if err := store.NoteFix("read", symptom, "pass path="+strings.Repeat("x", 200), ""); err != nil {
			t.Fatal(err)
		}
	}
	block := store.Block(nil)
	lines := 0
	for _, l := range strings.Split(block, "\n") {
		if strings.HasPrefix(l, "- ") {
			lines++
		}
	}
	if lines > toolUseBlockMax {
		t.Fatalf("the block carries %d recipes, want at most %d", lines, toolUseBlockMax)
	}
	if len(block) > toolUseBlockChars+700 {
		t.Fatalf("the block is %d chars, over the %d bound", len(block), toolUseBlockChars)
	}
}

// The shared store is one per directory: two sessions in one process share
// one file and one lock.
func TestSharedToolUseIsOneStorePerDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	if SharedToolUse(dir) != SharedToolUse(dir) {
		t.Fatal("the store must be shared per directory")
	}
	if SharedToolUse(dir) == SharedToolUse(filepath.Join(dir, "other")) {
		t.Fatal("a different directory is a different store")
	}
}

// The records are readable through the memory:// seam, so a session can
// look at what it has learned, and /memory clear takes them with the rest
// of the memory.
func TestToolUseRecordsAreReadableAndClearable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	b := &Backend{Dir: dir}
	if err := b.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := SharedToolUse(dir)
	if err := store.NoteFailure("read", "file not found: a.md", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := store.NoteFix("read", "file not found: a.md", "pass path=/abs/a.md", ""); err != nil {
		t.Fatal(err)
	}

	raw, err := b.Read("memory://root/tool-use.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "file not found: a.md") {
		t.Fatalf("the raw records are not readable:\n%s", raw)
	}
	if !strings.Contains(b.Stats(), "1 recipe(s)") {
		t.Fatalf("the stats report does not count the recipes:\n%s", b.Stats())
	}

	if err := b.Clear(); err != nil {
		t.Fatal(err)
	}
	if got := store.Records(); len(got) != 0 {
		t.Fatalf("/memory clear must take the tool-usage records with it: %+v", got)
	}
}
