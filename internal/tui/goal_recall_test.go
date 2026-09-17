package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// /goal verb dispatch (user-reported: `/goal create …` did nothing). The
// command used to drop its arguments and only ever render the view, so a
// user following the goal tool's grammar got "goal: none" back.

func TestGoalDispatchVerbs(t *testing.T) {
	var created, dropped string
	ops := &GoalOps{
		View: func() string { return "goal: active" },
		Create: func(obj string) (string, error) {
			created = obj
			return "created:" + obj, nil
		},
		Drop: func() (string, error) {
			dropped = "yes"
			return "dropped", nil
		},
	}
	if got, err := ops.Dispatch(""); err != nil || got != "goal: active" {
		t.Fatalf("bare /goal must view: %q %v", got, err)
	}
	if got, err := ops.Dispatch("create finish the audit"); err != nil || created != "finish the audit" {
		t.Fatalf("create must pass the objective: %q %v (created=%q)", got, err, created)
	}
	if _, err := ops.Dispatch("drop"); err != nil || dropped != "yes" {
		t.Fatalf("drop must reach the op: %v", err)
	}
	// Read intent: every synonym for "show me the goal" views instead of
	// erroring. `/goal check` used to be an unknown-verb error while no help
	// surface named the read verb, so the only words to try were invented ones.
	for _, verb := range []string{"check", "show", "status", "get", "list", "info", "view"} {
		if got, err := ops.Dispatch(verb); err != nil || got != "goal: active" {
			t.Fatalf("%q must view: %q %v", verb, got, err)
		}
	}
	// An unknown verb is a usage error, not a silent view.
	if _, err := ops.Dispatch("frobnicate x"); err == nil || !strings.Contains(err.Error(), "unknown /goal verb") {
		t.Fatalf("unknown verb must report the grammar, got %v", err)
	}
	// A verb with no objective is a usage error naming the shape.
	if _, err := ops.Dispatch("create"); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("create without an objective must ask for one, got %v", err)
	}
}

// A verb whose op is not wired says so — never a silent no-op (the failure
// mode this command shipped with).
func TestGoalDispatchUnwiredVerb(t *testing.T) {
	ops := &GoalOps{View: func() string { return "v" }}
	if _, err := ops.Dispatch("create x"); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("unwired create must report it, got %v", err)
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
