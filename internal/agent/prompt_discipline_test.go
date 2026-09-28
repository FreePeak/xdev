package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBasePromptCarriesToolDiscipline locks in the behavioural rules the
// session store showed going wrong. The 6-line prompt this replaces said
// nothing about delegation, and its one tool-usage line was ignored (bash:read
// ran 15:1 against omp's 3.9:1 — whole files were arriving through the shell).
//
// Every needle below is verified absent from the pre-change prompt, so this is
// a real guard rather than a restatement of what was already there. A future
// "the prompt is too long, trim it" change must fail here instead of silently
// restoring serial delegation and shell-driven reads.
func TestBasePromptCarriesToolDiscipline(t *testing.T) {
	p := strings.ToLower(SystemPromptBase)

	for _, want := range []struct {
		needle string
		why    string
	}{
		{"task", "the task tool is never mentioned, so fan-out is undiscoverable"},
		{"batch", "no batching guidance, so independent slices run serially"},
		{"parallel", "parallelism is never stated as the default for independent work"},
		{"actually runs", "bash is not scoped to work that executes, so reads go through the shell"},
		{"burn the context window", "the cost of pulling whole files in is not explained"},
		{"context window", "the context-window tradeoff is not named at all"},
	} {
		if !strings.Contains(p, want.needle) {
			t.Errorf("SystemPromptBase is missing %q — %s", want.needle, want.why)
		}
	}
}

// TestBasePromptCarriesConfirmFirst locks the "confirm before implementing"
// rule. Field report: a request that read like a question ("find the root
// causes and fix why the decisions are held at gates") was answered with 12
// turns of read/grep/curl against the wrong repo before anyone checked what
// was actually being asked, and the run was still mid-investigation when the
// user stopped it. The prompt is where that is fixed: the agent has to say
// what it read the request to mean, and ask when two readings survive.
func TestBasePromptCarriesConfirmFirst(t *testing.T) {
	p := strings.ToLower(SystemPromptBase)
	for _, want := range []struct {
		needle string
		why    string
	}{
		{"confirm", "nothing in the prompt asks the agent to confirm the requirement"},
		{"ask", "the ask tool is never named as the way to resolve an ambiguous request"},
		{"ambiguous", "there is no trigger describing WHEN asking is right"},
		{"explicit", "nothing excuses asking when the instruction was already explicit"},
	} {
		if !strings.Contains(p, want.needle) {
			t.Errorf("SystemPromptBase is missing %q — %s", want.needle, want.why)
		}
	}
}

// TestBasePromptKeepsItsOriginalRules guards the other direction: the
// behavioural additions must not quietly drop a rule that was already there.
func TestBasePromptKeepsItsOriginalRules(t *testing.T) {
	p := SystemPromptBase
	for _, want := range []string{
		"current working directory",
		"minimal, surgical edits",
		"build/test",
		"Never invent file contents",
		"say so plainly",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("SystemPromptBase dropped the pre-existing rule containing %q", want)
		}
	}
}

// TestBasePromptStaysCompact keeps the additions honest. The PRD budget for
// the whole assembled prompt is 1000 tokens (maxPromptTokens, enforced in
// cmd/xdev/prompt_test.go); the base is the part a change to this file can
// move, so it gets its own ceiling well under the total.
func TestBasePromptStaysCompact(t *testing.T) {
	const maxRunes = 2000
	if n := len([]rune(SystemPromptBase)); n > maxRunes {
		t.Fatalf("SystemPromptBase is %d runes (max %d) — trim the additions, do not raise the ceiling", n, maxRunes)
	}
}

// TestTaskDescriptionCarriesConstraints pins the behavioural contract on the
// tool itself. A bare capability statement ("spawn a subagent") gave the model
// no reason to prefer or avoid delegation, which is how a 3-call lookup ends
// up costing a full subagent round trip.
func TestTaskDescriptionCarriesConstraints(t *testing.T) {
	d := strings.ToLower((&TaskTool{}).Description())
	for _, want := range []struct {
		needle string
		why    string
	}{
		{"skip it", "no negative constraint — trivial jobs get delegated"},
		{"transcript", "the caller cannot see the work, which is the whole tradeoff"},
		{"batch", "the batch shape is not advertised"},
		{"concurrently", "concurrency is implemented but not stated"},
		{"max_turns", "a child that hits the turn cap returns partial work, unmentioned"},
	} {
		if !strings.Contains(d, want.needle) {
			t.Errorf("task tool description is missing %q — %s", want.needle, want.why)
		}
	}
}

// TestTaskParametersAdvertisesBothShapes is the schema half of the batch fix.
// The batch shape has been parsed since parity finding T3 #1 but was absent
// from the tool contract, so a model could not see the concurrency the engine
// implements. required:["prompt"] additionally made the batch unsendable on
// providers that validate strictly.
func TestTaskParametersAdvertisesBothShapes(t *testing.T) {
	raw := (&TaskTool{}).Parameters()
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
			Items       *struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"items"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("task parameters are not valid JSON: %v", err)
	}

	for _, k := range []string{"prompt", "context", "tasks", "background", "max_turns"} {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("task schema does not advertise %q", k)
		}
	}
	if len(schema.Required) != 0 {
		t.Errorf("task schema pins required=%v; a batch sends tasks[] with no prompt, so this rejects a legal call", schema.Required)
	}
	tasks, ok := schema.Properties["tasks"]
	if !ok {
		t.Fatal("tasks missing")
	}
	if tasks.Type != "array" || tasks.Items == nil {
		t.Fatalf("tasks is not an array of objects: type=%q items=%v", tasks.Type, tasks.Items)
	}
	for _, k := range []string{"task", "agent", "name", "max_turns"} {
		if _, ok := tasks.Items.Properties[k]; !ok {
			t.Errorf("batch item schema does not advertise %q", k)
		}
	}
	// The batch parser accepts `prompt` as an alias for `task`; the schema
	// should say so, or a model sending the alias reads as a mistake.
	if !strings.Contains(string(tasks.Items.Properties["task"]), "prompt") {
		t.Error("the batch item's `task` field does not mention its `prompt` alias")
	}
}
