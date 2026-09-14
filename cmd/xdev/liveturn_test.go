package main

import (
	"context"
	"testing"
)

// The abort paths (Esc, Ctrl+C, a full-link guest's interrupt) must cancel the
// live turn and nothing else. They used to call baseCancel(), and because every
// turn derives its context from baseCtx, one cancel left all later turns born
// already-canceled: the TUI kept accepting input but printed "· turn canceled"
// to every message until restart.
func TestLiveTurnAbortSparesTheSessionContext(t *testing.T) {
	var turn liveTurn
	baseCtx, baseCancel := context.WithCancel(context.Background())
	defer baseCancel()

	if turn.abort() {
		t.Fatal("abort reported a live turn with none published")
	}
	if baseCtx.Err() != nil {
		t.Fatal("an abort with no live turn touched the session context")
	}

	ctx, cancel := context.WithCancel(baseCtx)
	turn.set(cancel)
	if !turn.abort() {
		t.Fatal("abort found no live turn after set")
	}
	if ctx.Err() == nil {
		t.Fatal("the live turn survived the abort")
	}
	if baseCtx.Err() != nil {
		t.Fatal("aborting a turn canceled the session context — later turns would never run")
	}

	// A finished turn releases the slot, so a stale cancel outlives neither
	// the turn nor an abort aimed at it.
	turn.clear()
	if turn.abort() {
		t.Fatal("abort reported a live turn after clear")
	}
}
