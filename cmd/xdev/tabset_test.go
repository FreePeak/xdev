package main

import (
	"context"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
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

// TestTabsetLiveAgentIsPerSession: the mid-turn queue steers "the live agent",
// and it used to be ONE process-wide pointer every turn overwrote. With two
// sessions working, whichever turn registered most recently also took every
// steer meant for the other one — a prompt typed into the foreground tab was
// injected into the background run, and its row retired off the wrong
// conversation (#157). Registration is per session, and currentAgent is the
// only one a steer seam may reach.
func TestTabsetLiveAgentIsPerSession(t *testing.T) {
	a, b := memStore(t), memStore(t)
	ts := newTabset(a)
	if _, err := ts.open(b); err != nil {
		t.Fatal(err)
	}
	agA, agB := &agent.Agent{}, &agent.Agent{}

	ts.setAgent(a.ID(), agA)
	if got := ts.currentAgent(); got != agA {
		t.Fatalf("currentAgent with only A live = %v, want A's agent", got)
	}
	ts.setAgent(b.ID(), agB)

	// Foreground is still A, so a mid-turn submit there must reach A's run.
	if got := ts.currentAgent(); got != agA {
		t.Fatalf("currentAgent = %v, want A's agent (B registering last must not win)", got)
	}
	if got := ts.agentOf(b.ID()); got != agB {
		t.Fatalf("agentOf(b) = %v, want B's agent", got)
	}

	// Switch to B: the same seam now reaches B's run.
	ts.focus(b.ID())
	if got := ts.currentAgent(); got != agB {
		t.Fatalf("currentAgent after switching to B = %v, want B's agent", got)
	}

	// Tearing A's turn down leaves B's registration alone.
	ts.clearAgent(a.ID(), agA)
	if got := ts.agentOf(a.ID()); got != nil {
		t.Fatal("A's turn left its agent registered after the run ended")
	}
	if got := ts.agentOf(b.ID()); got != agB {
		t.Fatal("clearing A unregistered B")
	}
	// Every live agent is named for the exit notice: a parked session may be
	// mid-turn when the TUI quits.
	if all := ts.liveAgents(); len(all) != 1 || all[0] != agB {
		t.Fatalf("liveAgents = %v, want just B's agent", all)
	}
}

// TestTabsetClearAgentIgnoresAStaleTurn: a turn that finished late must not
// unregister the NEWER turn that already replaced it on the same session. The
// pointer check is the whole point of passing the agent back in.
func TestTabsetClearAgentIgnoresAStaleTurn(t *testing.T) {
	a := memStore(t)
	ts := newTabset(a)
	old, cur := &agent.Agent{}, &agent.Agent{}

	ts.setAgent(a.ID(), old)
	ts.setAgent(a.ID(), cur) // a retry started before the old turn unwound
	ts.clearAgent(a.ID(), old)

	if got := ts.agentOf(a.ID()); got != cur {
		t.Fatalf("a stale turn's teardown unregistered the live one: %v", got)
	}
	ts.clearAgent(a.ID(), cur)
	if got := ts.agentOf(a.ID()); got != nil {
		t.Fatal("the live turn's teardown left its agent registered")
	}
}

// TestTabsetCurrentIDPinsASession: send-now captures the id at the keystroke
// and delivers against it, so a tab switch in between cannot send the message
// to whichever session happens to be on screen when the host acts (#157).
func TestTabsetCurrentIDPinsASession(t *testing.T) {
	a, b := memStore(t), memStore(t)
	ts := newTabset(a)
	if _, err := ts.open(b); err != nil {
		t.Fatal(err)
	}
	pinned := ts.currentID()
	if pinned != a.ID() {
		t.Fatalf("currentID = %q, want A", pinned)
	}
	ts.focus(b.ID())
	if ts.currentID() == pinned {
		t.Fatal("currentID did not follow the switch, so a pinned id proves nothing")
	}
	empty := &tabset{}
	if got := empty.currentID(); got != "" {
		t.Fatalf("currentID on an empty set = %q, want empty", got)
	}
	if got := empty.currentAgent(); got != nil {
		t.Fatal("currentAgent on an empty set returned an agent")
	}
}

// TestTabsetReopenRestoresTheLastClosed: C-shift-T (opencode
// session_tab_reopen) is the inverse of close, so the closed stack has to
// hand back the newest session that is not already on screen — a stack that
// returned an open tab would silently do nothing, and an empty path (a
// memory-only session) must not be offered at all.
func TestTabsetReopenRestoresTheLastClosed(t *testing.T) {
	const path = "/tmp/xdev-tabset-fixture/2026-10-04T00-00-00.000Z_abc12345-6789-4abc-8def-0123456789ab.jsonl"
	id := sessionIDOfPath(path)
	if id != "abc12345-6789-4abc-8def-0123456789ab" {
		t.Fatalf("sessionIDOfPath(%q) = %q; the closed stack dedupes on it", path, id)
	}
	ts := newTabset(memStore(t))
	ts.pushClosedLocked(path)
	ts.pushClosedLocked("")
	if got := ts.takeClosed(); got != path {
		t.Fatalf("takeClosed = %q, want %q", got, path)
	}
	if got := ts.takeClosed(); got != "" {
		t.Fatalf("takeClosed on a drained stack = %q, want empty", got)
	}
	// A file that is open again is skipped, so C-shift-T never re-opens a tab
	// the user is already looking at.
	ts2 := &tabset{tabs: []*tab{{id: id, store: memStore(t)}}}
	ts2.closed = []string{path}
	if got := ts2.takeClosed(); got != "" {
		t.Fatalf("takeClosed returned an already-open tab: %q", got)
	}
}
