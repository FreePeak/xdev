package tui

import (
	"errors"
	"strings"
	"testing"
)

// TestMemoryOpsDispatch pins the /memory subcommand grammar: every verb
// reaches exactly one op, an unwired op degrades to an explanatory error
// (never a silent no-op), and the argument is passed through verbatim.
func TestMemoryOpsDispatch(t *testing.T) {
	var cleared bool
	var enqueued string
	ops := &MemoryOps{
		View:    func() string { return "VIEW" },
		Stats:   func() string { return "STATS" },
		Clear:   func() error { cleared = true; return nil },
		Queue:   func() string { return "QUEUE" },
		Sync:    func() (string, error) { return "SYNC", nil },
		Enqueue: func(text string) (string, error) { enqueued = text; return "ENQUEUED", nil },
	}
	cases := []struct {
		args string
		want string
	}{
		{"", "VIEW"},
		{"view", "VIEW"},
		{"view ", "VIEW"},
		{"stats", "STATS"},
		{"queue", "QUEUE"},
		{"sync", "SYNC"},
		{"enqueue the deploy script lives in scripts/ci.sh", "ENQUEUED"},
	}
	for _, tc := range cases {
		got, err := ops.Dispatch(tc.args)
		if err != nil {
			t.Errorf("Dispatch(%q) = %v, want no error", tc.args, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Dispatch(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
	if got, err := ops.Dispatch("clear"); err != nil || !strings.Contains(got, "cleared") {
		t.Errorf("Dispatch(clear) = (%q, %v)", got, err)
	}
	if !cleared {
		t.Error("a clear subcommand must call Clear")
	}
	if enqueued != "the deploy script lives in scripts/ci.sh" {
		t.Errorf("enqueue text = %q, want the argument verbatim", enqueued)
	}

	// The queue verbs are mnemopi-only: the local backend must say so.
	local := &MemoryOps{View: ops.View, Stats: ops.Stats, Clear: ops.Clear}
	for _, verb := range []string{"queue", "sync", "enqueue text"} {
		if _, err := local.Dispatch(verb); err == nil || !strings.Contains(err.Error(), "mnemopi") {
			t.Errorf("Dispatch(%q) on the local backend = %v, want a mnemopi hint", verb, err)
		}
	}
	if _, err := local.Dispatch("nonsense"); err == nil || !strings.Contains(err.Error(), "view|stats|clear") {
		t.Errorf("unknown subcommand error = %v, want the verb list", err)
	}
	// The argument is passed through verbatim, empty included: whether text
	// is required is the backend's call (mnemopi's store rejects an empty
	// retain), and a backend whose enqueue takes no argument stays usable.
	if got, err := ops.Dispatch("enqueue"); err != nil || got != "ENQUEUED" || enqueued != "" {
		t.Errorf("Dispatch(enqueue) = (%q, %v) enqueued=%q, want the empty argument passed through", got, err, enqueued)
	}
	if _, err := (*MemoryOps)(nil).Dispatch("view"); err == nil {
		t.Error("an unwired backend must error, not panic")
	}

	// A failing op's error must reach the caller unchanged.
	sentinel := errors.New("sync exploded")
	failing := &MemoryOps{Sync: func() (string, error) { return "", sentinel }}
	if _, err := failing.Dispatch("sync"); !errors.Is(err, sentinel) {
		t.Errorf("Dispatch(sync) error = %v, want the op's own error", err)
	}
}
