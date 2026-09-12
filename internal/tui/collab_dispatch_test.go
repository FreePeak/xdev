package tui

import (
	"reflect"
	"strings"
	"testing"
)

// TestCollabCommandDispatch pins the /collab and /join contract: which
// subcommand reaches which op, and that a bad subcommand is a notice rather
// than a model turn (a typo must never become a prompt).
func TestCollabCommandDispatch(t *testing.T) {
	var (
		starts []CollabMode
		stops  int
		joins  []string
	)
	old := Collab
	Collab = &CollabOps{
		Start:  func(m CollabMode) (string, error) { starts = append(starts, m); return "started", nil },
		Status: func() string { return "status-line" },
		Stop:   func() error { stops++; return nil },
		Join:   func(link string) (string, error) { joins = append(joins, link); return "joined " + link, nil },
	}
	t.Cleanup(func() { Collab = old })

	app := &fakeAPI{}
	for _, input := range []string{"/collab", "/collab view", "/collab remote view", "/collab status", "/collab stop", "/join room1.key1"} {
		if !dispatch(app, input) {
			t.Fatalf("dispatch(%q) = false, want the command consumed", input)
		}
	}
	if want := []CollabMode{{}, {View: true}, {View: true, Remote: true}}; !reflect.DeepEqual(starts, want) {
		t.Fatalf("Start modes = %+v, want %+v", starts, want)
	}
	if stops != 1 {
		t.Fatalf("Stop called %d times, want 1", stops)
	}
	if want := []string{"room1.key1"}; !reflect.DeepEqual(joins, want) {
		t.Fatalf("Join links = %v, want %v", joins, want)
	}
	if len(app.sent) != 0 {
		t.Fatalf("collab commands reached the model: %v", app.sent)
	}
	for i, want := range []string{"started", "started", "started", "status-line", "· collab stopped", "joined room1.key1"} {
		if i >= len(app.blocks) || app.blocks[i] != want {
			t.Fatalf("block %d = %q, want %q (blocks: %v)", i, blockAt(app.blocks, i), want, app.blocks)
		}
	}

	// A bad subcommand and a link-less /join are notices, never prompts.
	for _, input := range []string{"/collab bogus", "/join"} {
		bad := &fakeAPI{}
		if !dispatch(bad, input) {
			t.Fatalf("dispatch(%q) = false, want consumed", input)
		}
		if len(bad.blocks) != 1 || !strings.Contains(bad.blocks[0], "usage: /") {
			t.Fatalf("dispatch(%q) blocks = %v, want a usage notice", input, bad.blocks)
		}
		if len(bad.sent) != 0 {
			t.Fatalf("dispatch(%q) reached the model: %v", input, bad.sent)
		}
	}

	// Hosts without the collab seam degrade to a notice.
	Collab = nil
	unwired := &fakeAPI{}
	if !dispatch(unwired, "/collab") || !strings.Contains(unwired.blocks[0], "not wired") {
		t.Fatalf("unwired /collab blocks = %v, want an unwired notice", unwired.blocks)
	}
}

func blockAt(blocks []string, i int) string {
	if i < 0 || i >= len(blocks) {
		return ""
	}
	return blocks[i]
}
