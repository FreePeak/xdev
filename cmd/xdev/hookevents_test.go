package main

import (
	"context"
	"testing"
)

// recordingEmitter is a stub hook bus: it records the events the session
// switch path emits.
type recordingEmitter struct{ events []string }

func (r *recordingEmitter) Emit(_ context.Context, event string, _ any) {
	r.events = append(r.events, event)
}

// TestEmitSwitchEvents pins the session-switch lifecycle coverage: a switch
// brackets the store swap with session_before_switch / session_switch
// (research §4), and a nil bus is a no-op so a hookless session never
// panics on /resume or /fork.
func TestEmitSwitchEvents(t *testing.T) {
	rec := &recordingEmitter{}
	emitSwitchEvents(rec, true, "01abc", "a title")
	emitSwitchEvents(rec, false, "01abc", "a title")
	if len(rec.events) != 2 || rec.events[0] != "session_before_switch" || rec.events[1] != "session_switch" {
		t.Fatalf("events = %v", rec.events)
	}
	emitSwitchEvents(nil, true, "", "") // must not panic
	if len(rec.events) != 2 {
		t.Fatalf("nil bus emitted: %v", rec.events)
	}
}

// TestBuildHookBusPlumbsCLI proves the --hook plumbing reaches the bus the
// run modes install (settings are empty under test).
func TestBuildHookBusPlumbsCLI(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	b := buildHookBus(t.TempDir(), printOptions{Hooks: []string{"turn_start=exit 0"}}, nil)
	if b == nil || len(b.Hooks) != 1 || b.Hooks[0].Source != "cli" {
		t.Fatalf("bus = %+v", b)
	}
	if b.Hooks[0].Event != "turn_start" || b.Hooks[0].Command != "exit 0" {
		t.Fatalf("hook = %+v", b.Hooks[0])
	}
}
