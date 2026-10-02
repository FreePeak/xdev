package main

import (
	"context"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/session"
)

func memStore(t *testing.T) *session.Store {
	t.Helper()
	s := session.OpenMem("/tmp", "test")
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTabsetOpenActivateCycle(t *testing.T) {
	a, b, c := memStore(t), memStore(t), memStore(t)
	ts := newTabset(a)
	if got := ts.store(); got != a {
		t.Fatalf("initial store = %v, want a", got)
	}
	iB, err := ts.open(b)
	if err != nil {
		t.Fatal(err)
	}
	iC, err := ts.open(c)
	if err != nil {
		t.Fatal(err)
	}
	if iB == iC {
		t.Fatalf("open returned the same index for two stores: %d", iB)
	}
	// Re-open is a no-op: same index, no growth.
	iB2, err := ts.open(b)
	if err != nil || iB2 != iB {
		t.Fatalf("re-open b: idx=%d err=%v want %d", iB2, err, iB)
	}
	if open, _, _ := ts.summary(); open != 3 {
		t.Fatalf("open count = %d, want 3", open)
	}

	if tab := ts.activate(iC); tab == nil || tab.store != c {
		t.Fatalf("activate c: %+v", tab)
	}
	if ts.store() != c {
		t.Fatal("store() did not follow activate")
	}

	// Cycle wraps: c -> a -> b -> c.
	if tab := ts.cycle(1, false); tab == nil || tab.store != a {
		t.Fatalf("cycle +1 from c: %+v", tab)
	}
	if tab := ts.cycle(1, false); tab == nil || tab.store != b {
		t.Fatalf("cycle +1 from a: %+v", tab)
	}
	if tab := ts.cycle(-1, false); tab == nil || tab.store != a {
		t.Fatalf("cycle -1 from b: %+v", tab)
	}
}

func TestTabsetClaimIsPerSession(t *testing.T) {
	a, b := memStore(t), memStore(t)
	ts := newTabset(a)
	if _, err := ts.open(b); err != nil {
		t.Fatal(err)
	}
	idA, ok := ts.claimCurrent()
	if !ok || idA != a.ID() {
		t.Fatalf("claim a: id=%q ok=%v", idA, ok)
	}
	if _, ok := ts.claimCurrent(); ok {
		t.Fatal("second claim on a succeeded")
	}
	// b is free even while a holds a turn — that is the whole feature.
	if !ts.claim(b.ID()) {
		t.Fatal("claim b while a is running failed")
	}
	if !ts.isRunning(a.ID()) || !ts.isRunning(b.ID()) {
		t.Fatal("both sessions should be mid-turn")
	}
	ts.release(a.ID())
	if ts.isRunning(a.ID()) {
		t.Fatal("a still running after release")
	}
	if !ts.isRunning(b.ID()) {
		t.Fatal("b released with a")
	}
}

func TestTabsetAbortIsPerSession(t *testing.T) {
	a, b := memStore(t), memStore(t)
	ts := newTabset(a)
	if _, err := ts.open(b); err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	ts.publish(a.ID(), cancelA)
	ts.publish(b.ID(), cancelB)

	if !ts.abortCurrent() {
		t.Fatal("abortCurrent found no cancel")
	}
	select {
	case <-ctxA.Done():
	case <-time.After(time.Second):
		t.Fatal("a not canceled")
	}
	if ctxB.Err() != nil {
		t.Fatal("abortCurrent canceled the parked session")
	}

	if !ts.abort(b.ID()) {
		t.Fatal("abort b found no cancel")
	}
	select {
	case <-ctxB.Done():
	case <-time.After(time.Second):
		t.Fatal("b not canceled")
	}
}

func TestTabsetNoteAndCycleUnread(t *testing.T) {
	a, b, c := memStore(t), memStore(t), memStore(t)
	ts := newTabset(a)
	ib, _ := ts.open(b)
	ic, _ := ts.open(c)

	// Parked output on b and c.
	ts.note(b.ID())
	ts.note(c.ID())
	// Current session never badges itself.
	ts.note(a.ID())
	if _, _, unread := ts.summary(); unread != 2 {
		t.Fatalf("unread = %d, want 2", unread)
	}

	// onlyUnread jumps a -> b, skipping nothing (c is further).
	if tab := ts.cycle(1, true); tab == nil || tab.store != b {
		t.Fatalf("cycle unread +1: %+v", tab)
	}
	// Activating clears the badge.
	if _, _, unread := ts.summary(); unread != 1 {
		t.Fatalf("after landing on b, unread = %d, want 1 (c)", unread)
	}
	_ = ib
	_ = ic
}

func TestTabsetCloseMovesToNeighbour(t *testing.T) {
	a, b, c := memStore(t), memStore(t), memStore(t)
	ts := newTabset(a)
	_, _ = ts.open(b)
	_, _ = ts.open(c)
	ts.activate(1) // b
	next := ts.close(b.ID())
	if next == nil || next.store != c {
		t.Fatalf("close b: next=%+v, want c", next)
	}
	if open, _, _ := ts.summary(); open != 2 {
		t.Fatalf("open after close = %d, want 2", open)
	}
}

func TestTabsetEvictsIdleWhenFull(t *testing.T) {
	first := memStore(t)
	ts := newTabset(first)
	// Fill to capacity with idle tabs.
	for i := 1; i < tabSlots; i++ {
		if _, err := ts.open(memStore(t)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	// One more: the oldest parked idle tab is closed to make room.
	extra := memStore(t)
	if _, err := ts.open(extra); err != nil {
		t.Fatalf("open past capacity: %v", err)
	}
	if open, _, _ := ts.summary(); open != tabSlots {
		t.Fatalf("open = %d, want %d", open, tabSlots)
	}
	// Every slot busy: nothing to evict.
	for _, info := range ts.snapshot() {
		if !ts.claim(info.ID) && info.ID != first.ID() {
			// already claimed somehow — fine
		}
		_ = ts.claim(info.ID)
	}
	// Force every tab running so eviction has nowhere to go.
	ts.mu.Lock()
	for _, tab := range ts.tabs {
		tab.running = true
	}
	ts.mu.Unlock()
	if _, err := ts.open(memStore(t)); err != errTooManyTabs {
		t.Fatalf("expected errTooManyTabs, got %v", err)
	}
}

// TestTabsetFocusByID: the /tabs row path. focus names the session instead
// of stepping to a neighbour, clears that tab's badge the way activate does,
// and is a no-op for an id that is not open (a session can be dropped while
// the picker is up).
func TestTabsetFocusByID(t *testing.T) {
	a, b, c := memStore(t), memStore(t), memStore(t)
	ts := newTabset(a)
	if _, err := ts.open(b); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.open(c); err != nil {
		t.Fatal(err)
	}
	ts.note(c.ID())
	if got := ts.focus(c.ID()); got == nil || got.store != c {
		t.Fatalf("focus c: %+v", got)
	}
	if ts.store() != c {
		t.Fatal("focus did not make c current")
	}
	if _, _, unread := ts.summary(); unread != 0 {
		t.Fatalf("focused tab kept its badge: unread = %d, want 0", unread)
	}
	if got := ts.focus(a.ID()); got == nil || got.store != a {
		t.Fatalf("focus a: %+v", got)
	}
	if got := ts.focus("nope"); got != nil {
		t.Fatalf("focus of an unopened id = %+v, want nil", got)
	}
}
