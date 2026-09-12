package tui

import (
	"errors"
	"strings"
	"testing"
)

// TestMemoryOpsDispatch pins the /memory subcommand grammar across every
// backend (M12 #43/#44): the backend-agnostic verbs (view|stats|clear), the
// remote backend's diagnose, and the queue-backed store's queue|sync|enqueue
// each reach exactly one op, the argument is passed through verbatim, an
// unwired op degrades to an explanatory error (never a silent no-op), and a
// backend error reaches the caller unchanged.
func TestMemoryOpsDispatch(t *testing.T) {
	cleared, enqueued := 0, 0
	var enqueuedText string
	ops := &MemoryOps{
		View:     func() string { return "view body" },
		Stats:    func() string { return "stats body" },
		Clear:    func() error { cleared++; return nil },
		Diagnose: func() string { return "diagnose body" },
		Queue:    func() string { return "queue body" },
		Sync:     func() (string, error) { return "sync body", nil },
		Enqueue: func(text string) (string, error) {
			enqueued++
			enqueuedText = text
			return "1 retain queued; queue flushed", nil
		},
	}
	cases := []struct{ args, want string }{
		{"", "view body"},
		{"view", "view body"},
		{"view ", "view body"},
		{"stats", "stats body"},
		{"clear", "memory cleared"},
		{"reset", "memory cleared"},
		{"diagnose", "diagnose body"},
		{"queue", "queue body"},
		{"sync", "sync body"},
		{"enqueue", "1 retain queued; queue flushed"},
		{"rebuild", "1 retain queued; queue flushed"},
	}
	for _, c := range cases {
		got, err := ops.Dispatch(c.args)
		if err != nil {
			t.Errorf("Dispatch(%q) error: %v", c.args, err)
			continue
		}
		if got != c.want {
			t.Errorf("Dispatch(%q) = %q, want %q", c.args, got, c.want)
		}
	}
	if cleared != 2 {
		t.Errorf("clear ran %d times, want 2", cleared)
	}
	if enqueued != 2 {
		t.Errorf("enqueue ran %d times, want 2", enqueued)
	}
	// The trailing text is the subcommand's argument, verbatim (and empty
	// when there is none): whether text is required is the backend's call.
	if enqueuedText != "" {
		t.Errorf("enqueue argument = %q, want the empty tail", enqueuedText)
	}
	if got, err := ops.Dispatch("enqueue the deploy script lives in scripts/ci.sh"); err != nil || got != "1 retain queued; queue flushed" {
		t.Errorf("Dispatch(enqueue text) = (%q, %v)", got, err)
	}
	if enqueuedText != "the deploy script lives in scripts/ci.sh" {
		t.Errorf("enqueue text = %q, want the argument verbatim", enqueuedText)
	}

	// A backend that wires only the shared trio: the backend-specific verbs
	// must say which backend has them instead of panicking, and an unknown
	// verb must list the grammar.
	local := &MemoryOps{View: func() string { return "local view" }, Stats: func() string { return "local stats" }, Clear: func() error { return nil }}
	if got, err := local.Dispatch("view"); err != nil || got != "local view" {
		t.Errorf("local view = (%q, %v)", got, err)
	}
	for _, verb := range []string{"queue", "sync", "enqueue text"} {
		if _, err := local.Dispatch(verb); err == nil || !strings.Contains(err.Error(), "mnemopi") {
			t.Errorf("Dispatch(%q) on the local backend = %v, want a mnemopi hint", verb, err)
		}
	}
	if _, err := local.Dispatch("diagnose"); err == nil || !strings.Contains(err.Error(), "hindsight") {
		t.Errorf("Dispatch(diagnose) on the local backend = %v, want a hindsight hint", err)
	}
	if _, err := local.Dispatch("nonsense"); err == nil || !strings.Contains(err.Error(), "view|stats|clear") {
		t.Errorf("unknown subcommand error = %v, want the verb list", err)
	}
	if _, err := local.Dispatch("frobnicate"); err == nil {
		t.Error("an unknown subcommand reported success")
	}

	// The unwired-app case: error, not panic.
	var unwired *MemoryOps
	if _, err := unwired.Dispatch("view"); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("nil ops error = %v, want a not-wired message", err)
	}

	// A failing op's error must reach the caller unchanged.
	sentinel := errors.New("sync exploded")
	failing := &MemoryOps{Sync: func() (string, error) { return "", sentinel }}
	if _, err := failing.Dispatch("sync"); !errors.Is(err, sentinel) {
		t.Errorf("Dispatch(sync) error = %v, want the op's own error", err)
	}
	if _, err := (&MemoryOps{Clear: func() error { return errors.New("boom") }}).Dispatch("clear"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("clear failure = %v, want the backend error", err)
	}
}
