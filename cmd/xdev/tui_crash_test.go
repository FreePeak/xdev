package main

import (
	"fmt"
	"testing"

	"github.com/FreePeak/xdev/internal/session"
)

// TestAPanickingSessionRecordsFatal: a crashed session must record a FATAL
// exit, not a normal one.
//
// The exit marker is written while the panic unwinds, so the crash flag has
// to be set BEFORE the writer runs. The bug this guards was two defers: the
// recovering defer set the flag and the marker defer read it, and defers run
// last-in-first-out — so the reader ran FIRST, saw the flag while it was
// still false, and a crashed TUI recorded kind="normal".
//
// The test drives the real path: the same guard, the same kind decision and
// the same marker, written to a real store and read back.
func TestAPanickingSessionRecordsFatal(t *testing.T) {
	st := session.OpenMem(t.TempDir(), "t")
	defer st.Close()

	// The panic must still take the process down: the guard re-raises it.
	var reRaised any
	func() {
		defer func() { reRaised = recover() }()
		func() {
			defer withCrashGuard(func(crashed bool) {
				kind, code, detail := crashExitKind(crashed, 0)
				_ = st.Append(&session.CustomEntry{CustomType: "session_exit",
					Data: exitMarker("tui", code, kind, exitReason("tui", kind, detail), pendingToolCalls(st))})
			})
			panic("boom")
		}()
	}()
	if reRaised == nil {
		t.Fatal("the panic must be re-raised: the process has to die loudly")
	}
	entries := st.Entries()
	if len(entries) == 0 {
		t.Fatal("the crash wrote no exit marker")
	}
	ce, ok := entries[len(entries)-1].(*session.CustomEntry)
	if !ok || ce.CustomType != "session_exit" {
		t.Fatalf("the last entry is not the exit marker: %+v", entries[len(entries)-1])
	}
	if ce.Data["kind"] != "fatal" {
		t.Fatalf("kind = %v, want fatal (a crashed session is not a normal exit)", ce.Data["kind"])
	}
	if ce.Data["code"] != 2 {
		t.Fatalf("code = %v, want 2 (the runtime's crash exit code)", ce.Data["code"])
	}
	if reason, _ := ce.Data["reason"].(string); reason == "" {
		t.Fatalf("the marker names no reason: %v", ce.Data)
	}
}

// TestACleanExitRecordsNormal: the same guard, with no panic, records the
// normal exit — the crash flag must not leak into it.
func TestACleanExitRecordsNormal(t *testing.T) {
	st := session.OpenMem(t.TempDir(), "t")
	defer st.Close()

	func() {
		defer withCrashGuard(func(crashed bool) {
			kind, code, detail := crashExitKind(crashed, 0)
			_ = st.Append(&session.CustomEntry{CustomType: "session_exit",
				Data: exitMarker("tui", code, kind, exitReason("tui", kind, detail), pendingToolCalls(st))})
		})
	}()
	entries := st.Entries()
	if len(entries) == 0 {
		t.Fatal("the exit wrote no marker")
	}
	ce, ok := entries[len(entries)-1].(*session.CustomEntry)
	if !ok {
		t.Fatalf("the last entry is not the exit marker: %+v", entries[len(entries)-1])
	}
	if ce.Data["kind"] != "normal" {
		t.Fatalf("kind = %v, want normal", ce.Data["kind"])
	}
	if ce.Data["code"] != 0 {
		t.Fatalf("code = %v, want 0", ce.Data["code"])
	}
}

// TestCrashExitKindNamesTheCrash: the decision itself, so a change to the
// codes is caught here rather than in a transcript nobody reads.
func TestCrashExitKindNamesTheCrash(t *testing.T) {
	kind, code, detail := crashExitKind(true, 0)
	if kind != "fatal" || code != 2 || detail == "" {
		t.Fatalf("crashExitKind(true, 0) = %q/%d/%q, want fatal/2/a detail", kind, code, detail)
	}
	kind, code, detail = crashExitKind(false, 7)
	if kind != "normal" || code != 7 || detail != "" {
		t.Fatalf("crashExitKind(false, 7) = %q/%d/%q, want normal/7/no detail", kind, code, detail)
	}
	_ = fmt.Sprint(kind)
}
