package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// /goal: the argument IS the objective — `/goal <target goal>` sets it and
// starts the run. It used to require a verb (`/goal create …`), so the
// obvious spelling either did nothing or errored; the read verbs, the two
// closers and the old `create <objective>` spelling stay accepted.

func TestGoalDispatchTargetIsTheArgument(t *testing.T) {
	var set []string
	var dropped string
	ops := &GoalOps{
		View: func() string { return "goal: active" },
		Set: func(objective string) (string, error) {
			set = append(set, objective)
			return "created:" + objective, nil
		},
		Drop: func() (string, error) {
			dropped = "yes"
			return "dropped", nil
		},
	}
	// The target: a plain multi-word objective, no verb.
	if got, err := ops.Dispatch("fix the goal error"); err != nil || got != "created:fix the goal error" {
		t.Fatalf("target objective: %q %v (set=%q)", got, err, set)
	}
	// `create` is the spelling the docs and the tool's grammar use; it still
	// takes the objective.
	if _, err := ops.Dispatch("create finish the audit"); err != nil || set[len(set)-1] != "finish the audit" {
		t.Fatalf("create must pass the objective: %v (set=%q)", err, set)
	}
	if got, err := ops.Dispatch(""); err != nil || got != "goal: active" {
		t.Fatalf("bare /goal must view: %q %v", got, err)
	}
	// Read intent: every synonym for "show me the goal" views instead of
	// erroring. `/goal check` used to be an unknown-verb error while no help
	// surface named the read verb, so the only words to try were invented ones.
	for _, verb := range []string{"check", "show", "status", "get", "list", "info", "view"} {
		if got, err := ops.Dispatch(verb); err != nil || got != "goal: active" {
			t.Fatalf("%q must view: %q %v", verb, got, err)
		}
	}
	if len(set) != 2 {
		t.Fatalf("a read verb started a goal: %q", set)
	}
	if _, err := ops.Dispatch("drop"); err != nil || dropped != "yes" {
		t.Fatalf("drop must reach the op: %v", err)
	}
	// A verb with no objective is a usage error naming the shape.
	if _, err := ops.Dispatch("create"); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("create without an objective must ask for one, got %v", err)
	}
}

// A verb whose op is not wired says so — never a silent no-op. The failure
// mode that matters is `error: goal not wired` on a working command.
func TestGoalDispatchUnwiredVerb(t *testing.T) {
	ops := &GoalOps{View: func() string { return "v" }}
	for _, arg := range []string{"create x", "just land the thing", "complete", "drop"} {
		if _, err := ops.Dispatch(arg); err == nil || !strings.Contains(err.Error(), "not wired") {
			t.Fatalf("unwired %q must report it, got %v", arg, err)
		}
	}
	if _, err := (&GoalOps{}).Dispatch("view"); err == nil {
		t.Fatal("a nil view seam must error")
	}
}

// Complete's notes split on ; and , (the tool takes a list); a plain
// sentence stays one note.
func TestGoalCompleteNotesSplit(t *testing.T) {
	var got []string
	ops := &GoalOps{
		View: func() string { return "v" },
		Complete: func(notes []string) (string, error) {
			got = notes
			return "done", nil
		},
	}
	if _, err := ops.Dispatch("complete suite green; gofmt clean, cross builds ok"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("notes = %q, want 3 split entries", got)
	}
	if _, err := ops.Dispatch("complete one plain sentence"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "one plain sentence" {
		t.Fatalf("a plain sentence must stay one note: %q", got)
	}
}

// /goal <objective> starts the goal's first turn: the objective is what the
// model gets. Answering the command and running nothing was the defect.
func TestGoalTargetStartsTheTurn(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})
	app.SetGoalOps(&GoalOps{
		View: func() string { return "goal: active" },
		Set: func(objective string) (string, error) {
			app.SendPrompt(objective)
			return "goal created", nil
		},
	})
	typeRunes(app, "/goal land the exporter")
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if len(sent) != 1 || sent[0] != "land the exporter" {
		t.Fatalf("the objective must start the turn: sent=%q", sent)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	last := app.blocks[len(app.blocks)-1]
	if !strings.Contains(last.Text, "goal created") {
		t.Fatalf("created block = %+v", last)
	}
}

// --- composer history recall (user-reported: Up did not recall) ---

// Up recalls the previous prompt whether or not the box holds a draft, and
// Down past the newest slot returns the draft. Both guards used to block it:
// an empty composer sent Up to the transcript scroll, and a non-empty one was
// refused as "don't clobber a fresh draft".
func TestEditorRecallWithAndWithoutDraft(t *testing.T) {
	e := &Editor{}
	e.PushHistory("first prompt")
	e.PushHistory("second prompt")

	// Empty composer: Up recalls the newest entry.
	e.recall(-1)
	if got := e.Text(); got != "second prompt" {
		t.Fatalf("Up from empty = %q, want the newest prompt", got)
	}
	e.recall(-1)
	if got := e.Text(); got != "first prompt" {
		t.Fatalf("second Up = %q, want the older prompt", got)
	}
	// Down returns to the newest slot: the box empties (no draft was typed).
	e.recall(1)
	e.recall(1)
	if got := e.Text(); got != "" {
		t.Fatalf("Down past newest with no draft = %q, want empty", got)
	}

	// With a draft in the box: Up still recalls, and Down restores the draft.
	e.Reset()
	for _, r := range "typed draft" {
		e.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	e.recall(-1)
	if got := e.Text(); got != "second prompt" {
		t.Fatalf("Up with a draft = %q, want the recalled prompt", got)
	}
	e.recall(1)
	if got := e.Text(); got != "typed draft" {
		t.Fatalf("Down must restore the stashed draft, got %q", got)
	}
}

// A multi-line draft is never clobbered (the existing guard stays).
func TestEditorRecallKeepsMultilineDraft(t *testing.T) {
	e := &Editor{}
	e.PushHistory("older")
	e.buf = []rune("line one\nline two")
	e.histIdx = len(e.history)
	e.recall(-1)
	if e.Text() != "line one\nline two" {
		t.Fatalf("multi-line draft was clobbered: %q", e.Text())
	}
}

func TestEditorHasHistory(t *testing.T) {
	e := &Editor{}
	if e.HasHistory() {
		t.Fatal("a fresh editor has nothing to recall")
	}
	e.PushHistory("x")
	if !e.HasHistory() {
		t.Fatal("after one prompt Up must belong to the editor, not the scroller")
	}
}

// "/theme list" is the obvious thing to type and used to be treated as a
// theme named "list" (error: unknown theme). Bare /theme and /theme list are
// the same query.
func TestThemeListVerb(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.SetThemeOps(&ThemeOps{
		Current: func() string { return "probe" },
		List:    func() []string { return []string{"auto", "groknight", "probe"} },
		Set:     func(name string) error { return fmt.Errorf("unknown theme %q", name) },
	})
	if err := app.Theme("list"); err != nil {
		t.Fatalf("/theme list errored: %v", err)
	}
	app.mu.Lock()
	block := app.blocks[len(app.blocks)-1].Text
	app.mu.Unlock()
	if !strings.Contains(block, "active theme: probe") || !strings.Contains(block, "auto") {
		t.Fatalf("listing block = %q", block)
	}
	// An unknown name still errors — the verb is the only alias.
	if err := app.Theme("nosuchtheme"); err == nil {
		t.Fatal("an unknown theme name must error")
	}
}
