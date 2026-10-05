package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/session"
)

// tabset is the TUI's live session set: an ordered list of open sessions, one
// of them current. It replaces the one `store` variable plus the one
// process-wide `running` flag the TUI used to hold.
//
// The rule it exists to enforce: a session that is not current KEEPS RUNNING.
// A parked session's turn persists everything it produces and renders nothing
// (tuiHooks gates its App writes on tuiSession.focused), so its transcript is
// rebuilt from the store on switch-back — swapStoreTo already did exactly
// that for /resume, so the rebuild is reused rather than reinvented.
// opencode does the same thing one layer down: switching is route navigation
// (session-tabs.tsx select -> route.navigate) and the previous session's
// execution fiber is never torn down — only its events stop painting.
//
// ponytail: the App still owns ONE transcript (blocks, scroll, HUD stats) and
// a switch rebuilds it by replaying the target's store. The ceiling that buys:
// a parked session shows nothing until it is focused, so the unread dot is the
// whole "it did something" signal, and its HUD counters are not accumulated
// while parked. The upgrade path is a per-tab block list inside the App;
// nothing above this type changes.
//
// The open set is also written to disk (tabset.persist → sessionops.go's
// tab-set sidecar), so a later launch in the same directory reopens the same
// sessions as tabs — opencode persists its own tab set the same way
// (tui/<channel>/tui/tabs.json, scoped per cwd by default). Without it a
// restart loses the set even though every session is still on disk.

// tabSlots bounds the open set. Every entry holds an open writer plus its
// windowed entries, so this is the memory ceiling the feature trades for
// "switch away without losing a running turn".
const tabSlots = 6

// errTooManyTabs is returned when every slot is held by a running session —
// nothing idle to evict, and a seventh concurrent turn would be unbounded.
var errTooManyTabs = errors.New("too many open sessions (all slots busy)")

// tab is one open session.
type tab struct {
	id    string
	title string
	store *session.Store

	// running is the session's single-turn claim: the per-tab replacement for
	// the old process-wide atomic.Bool. A turn in a parked session holds it
	// exactly as a foreground one does — that is what makes switching away
	// mid-turn possible at all.
	running bool
	// cancel publishes the in-flight turn's cancel, so Esc aborts the session
	// on screen and never a parked one.
	cancel context.CancelFunc
	// liveAgent is the session's in-flight turn agent. startTurn publishes it
	// BEFORE the UI says "running" and clears it before the UI says "not
	// running", so a mid-turn submit can never find a live turn with no agent
	// to steer — the window in which the queue used to refuse the prompt and
	// withdraw its row (#157). It was one process-wide pointer, so with two
	// sessions working whichever turn registered last also took every steer
	// meant for the other one.
	liveAgent *agent.Agent

	// unread is the badge: a parked session produced output nobody has looked
	// at. Cleared when the tab becomes current.
	unread bool
}

type tabset struct {
	mu   sync.Mutex
	tabs []*tab
	cur  int
	// closed is the recently-closed stack (opencode's session_tab_reopen):
	// the file paths of closed tabs, newest first, capped. The tabset is
	// process memory and a path is all a reopen needs — reopening opens the
	// same JSONL again, so nothing is kept that could go stale.
	closed []string
}

func newTabset(s *session.Store) *tabset {
	return &tabset{tabs: []*tab{{id: s.ID(), title: s.Title(), store: s}}}
}

// current is the active tab, or nil on an empty set (which the TUI never has:
// there is always a session).
func (ts *tabset) current() *tab {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.cur < 0 || ts.cur >= len(ts.tabs) {
		return nil
	}
	return ts.tabs[ts.cur]
}

// store is the active session — the one value every closure in tui.go reads.
func (ts *tabset) store() *session.Store {
	if t := ts.current(); t != nil {
		return t.store
	}
	return nil
}

func (ts *tabset) indexOfLocked(id string) int {
	for i, t := range ts.tabs {
		if t.id == id {
			return i
		}
	}
	return -1
}

// open returns the index of the tab for a store, adding it when it is not
// already there. A session can only be open once: opening one file twice
// would be a second writer on one JSONL, which the store refuses anyway.
func (ts *tabset) open(s *session.Store) (int, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i := ts.indexOfLocked(s.ID()); i >= 0 {
		// Title may have changed on disk since we last saw it.
		if t := s.Title(); t != "" {
			ts.tabs[i].title = t
		}
		return i, nil
	}
	if len(ts.tabs) >= tabSlots {
		// Evict the oldest PARKED, idle tab. A running one is never
		// evicted: its turn is mid-flight with an open writer, and
		// closing it under a live append is data loss.
		evicted := false
		for i, t := range ts.tabs {
			if i == ts.cur || t.running {
				continue
			}
			_ = t.store.Close()
			ts.tabs = append(ts.tabs[:i], ts.tabs[i+1:]...)
			if ts.cur > i {
				ts.cur--
			}
			evicted = true
			break
		}
		if !evicted {
			return -1, errTooManyTabs
		}
	}
	ts.tabs = append(ts.tabs, &tab{id: s.ID(), title: s.Title(), store: s})
	return len(ts.tabs) - 1, nil
}

// activate makes tab i current and clears its badge. Nothing here touches the
// App: the caller rebuilds the view, which keeps this testable without a
// screen.
func (ts *tabset) activate(i int) *tab {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i < 0 || i >= len(ts.tabs) {
		return nil
	}
	ts.cur = i
	t := ts.tabs[i]
	t.unread = false
	return t
}

// focus makes the tab with this id current, or nil when it is not open.
// It is the /tabs row path: the user names the session, so there is
// nothing to cycle to.
func (ts *tabset) focus(id string) *tab {
	ts.mu.Lock()
	i := ts.indexOfLocked(id)
	ts.mu.Unlock()
	if i < 0 {
		return nil
	}
	return ts.activate(i)
}

// close drops a tab, aborting its turn first — a parked session whose turn is
// still running would otherwise keep a writer nobody can reach. The active tab
// closes onto its neighbour (next, else previous), which is what /drop needs.
// The closed store is already Closed; the caller must not touch it again.
func (ts *tabset) close(id string) *tab {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	i := ts.indexOfLocked(id)
	if i < 0 {
		return nil
	}
	t := ts.tabs[i]
	if t.running && t.cancel != nil {
		t.cancel()
	}
	_ = t.store.Close()
	// Remember the file so C-shift-T can reopen it. A session dropped on
	// purpose (/drop removes the file) lands here too, and the reopen
	// simply finds the path gone — which is the honest answer for a tab the
	// user deleted.
	ts.pushClosedLocked(t.store.Path())
	ts.tabs = append(ts.tabs[:i], ts.tabs[i+1:]...)
	switch {
	case ts.cur == i:
		ts.cur = min(i, len(ts.tabs)-1)
	case ts.cur > i:
		ts.cur--
	}
	if len(ts.tabs) == 0 {
		return nil
	}
	return ts.tabs[min(max(ts.cur, 0), len(ts.tabs)-1)]
}

// closedStack bounds the reopen list. Ten matches the tab ceiling: a stack
// deeper than the set it restores from is memory nobody can reach a session
// from.
const closedStack = 10

// pushClosedLocked remembers a closed tab's file, newest first. An empty path
// (a session that never persisted) is skipped, and reopening the same file
// again moves it back to the top rather than stacking it twice.
func (ts *tabset) pushClosedLocked(path string) {
	if path == "" {
		return
	}
	out := ts.closed[:0]
	for _, p := range ts.closed {
		if p != path {
			out = append(out, p)
		}
	}
	ts.closed = append([]string{path}, out...)
	if len(ts.closed) > closedStack {
		ts.closed = ts.closed[:closedStack]
	}
}

// takeClosed pops the newest closed path off the stack whose session is not
// already open, so C-shift-T never re-opens a tab that is on screen. "" when
// nothing is left to reopen.
func (ts *tabset) takeClosed() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for len(ts.closed) > 0 {
		path := ts.closed[0]
		ts.closed = ts.closed[1:]
		id := sessionIDOfPath(path)
		open := false
		for _, t := range ts.tabs {
			if t.id == id {
				open = true
				break
			}
		}
		if open {
			continue // skipping it is the whole point of the scan
		}
		return path
	}
	return ""
}

// sessionIDOfPath is the session id a canonical <stamp>_<uuid>.jsonl file
// name carries, or "" when the name carries none. The store's own id always
// equals it (session.sessionIDFromPath enforces that on every write), so the
// reopen check reads the same id the tabset holds.
func sessionIDOfPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndex(base, "_"); i >= 0 && base[i+1:] != "" {
		return base[i+1:]
	}
	return ""
}

// cycle steps the current tab by dir, wrapping, and skips tabs with a badge
// when onlyUnread is set (opencode's cycleUnread). nil on a single tab, so the
// chord degrades to a notice rather than a silent no-op.
func (ts *tabset) cycle(dir int, onlyUnread bool) *tab {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.tabs) < 2 {
		return nil
	}
	n := len(ts.tabs)
	for step := 1; step <= n; step++ {
		i := (ts.cur + dir*step + n*step) % n
		t := ts.tabs[i]
		if onlyUnread && !t.unread {
			continue
		}
		ts.cur = i
		t.unread = false
		return t
	}
	return nil
}

// claim takes a tab's single-turn slot, reporting false when one already holds
// it. Every "a turn is running" refusal in tui.go now asks the SESSION, not
// the process: that is what lets a background session run while another is
// current.
func (ts *tabset) claim(id string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	i := ts.indexOfLocked(id)
	if i < 0 || ts.tabs[i].running {
		return false
	}
	ts.tabs[i].running = true
	return true
}

func (ts *tabset) release(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i := ts.indexOfLocked(id); i >= 0 {
		ts.tabs[i].running = false
	}
}

// isRunning answers "is this session mid-turn" — the guard /clear and the
// store-mutating commands consult per session now.
func (ts *tabset) isRunning(id string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	i := ts.indexOfLocked(id)
	return i >= 0 && ts.tabs[i].running
}

// currentRunning is isRunning of the active tab — the common "is the screen's
// session mid-turn" check the UI loop used to make with one atomic.Bool.
func (ts *tabset) currentRunning() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.cur < 0 || ts.cur >= len(ts.tabs) {
		return false
	}
	return ts.tabs[ts.cur].running
}

// claimCurrent claims the active tab's turn slot.
func (ts *tabset) claimCurrent() (id string, ok bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.cur < 0 || ts.cur >= len(ts.tabs) {
		return "", false
	}
	t := ts.tabs[ts.cur]
	if t.running {
		return t.id, false
	}
	t.running = true
	return t.id, true
}

// abort cancels one session's live turn and reports whether one was running.
func (ts *tabset) abort(id string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	i := ts.indexOfLocked(id)
	if i < 0 || ts.tabs[i].cancel == nil {
		return false
	}
	ts.tabs[i].cancel()
	return true
}

// abortCurrent cancels the active session's turn — Esc / Ctrl+C.
func (ts *tabset) abortCurrent() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.cur < 0 || ts.cur >= len(ts.tabs) {
		return false
	}
	t := ts.tabs[ts.cur]
	if t.cancel == nil {
		return false
	}
	t.cancel()
	return true
}

// abortAll cancels every session's turn — the exit path. A turn outliving the
// TUI would keep writing to a store the defers are closing.
func (ts *tabset) abortAll() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range ts.tabs {
		if t.cancel != nil {
			t.cancel()
		}
	}
}

// publish hands a turn's cancel to its session. The session context must
// never be the abort target — each turn derives its own ctx from baseCtx, so
// cancelling baseCtx leaves every later turn born already-canceled and the
// TUI prints "· turn canceled" to every future message until restart.
func (ts *tabset) publish(id string, cancel context.CancelFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i := ts.indexOfLocked(id); i >= 0 {
		ts.tabs[i].cancel = cancel
	}
}

// clear drops a published cancel so a finished turn outlives neither an abort
// aimed at it nor a later turn's slot.
func (ts *tabset) clear(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i := ts.indexOfLocked(id); i >= 0 {
		ts.tabs[i].cancel = nil
	}
}

// setAgent publishes a turn's agent against the session it runs for. It is the
// turn's READY edge and must be called BEFORE the UI is told a turn is running.
func (ts *tabset) setAgent(id string, ag *agent.Agent) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i := ts.indexOfLocked(id); i >= 0 {
		ts.tabs[i].liveAgent = ag
	}
}

// clearAgent drops a registration, but only while it still names that agent:
// the pointer check is what keeps an older turn's teardown from unregistering
// the newer turn that replaced it on the same session.
func (ts *tabset) clearAgent(id string, ag *agent.Agent) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i := ts.indexOfLocked(id); i >= 0 && ts.tabs[i].liveAgent == ag {
		ts.tabs[i].liveAgent = nil
	}
}

// agentOf is one session's live agent, nil when that session is idle.
func (ts *tabset) agentOf(id string) *agent.Agent {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	i := ts.indexOfLocked(id)
	if i < 0 {
		return nil
	}
	return ts.tabs[i].liveAgent
}

// currentAgent is the agent of the session on screen. Every steer seam — the
// mid-turn queue, extensions, the mailbox — routes through this, so a prompt
// reaches the run the person is actually looking at.
func (ts *tabset) currentAgent() *agent.Agent {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.cur < 0 || ts.cur >= len(ts.tabs) {
		return nil
	}
	return ts.tabs[ts.cur].liveAgent
}

// liveAgents is every registered agent, for the exit path's one-shot session
// shutdown notice: any session may be mid-turn when the TUI quits.
func (ts *tabset) liveAgents() []*agent.Agent {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]*agent.Agent, 0, len(ts.tabs))
	for _, t := range ts.tabs {
		if t.liveAgent != nil {
			out = append(out, t.liveAgent)
		}
	}
	return out
}

// currentID names the session on screen, or "" on an empty set. Send-now pins
// it at the keystroke so a tab switch before the host delivers cannot send the
// message to a different conversation (#157).
func (ts *tabset) currentID() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.cur < 0 || ts.cur >= len(ts.tabs) {
		return ""
	}
	return ts.tabs[ts.cur].id
}

// note marks a parked session as having produced output the user has not seen.
func (ts *tabset) note(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i := ts.indexOfLocked(id); i >= 0 && i != ts.cur {
		ts.tabs[i].unread = true
	}
}

// setTitle refreshes a tab's display title after the cascade rewrites it.
func (ts *tabset) setTitle(id, title string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i := ts.indexOfLocked(id); i >= 0 {
		ts.tabs[i].title = title
	}
}

// persistIDs returns the open session ids in tab order — the set a later
// launch in the same directory should reopen. The path, not the id: a
// session's file is what survives a restart, and a store that never
// materialized has nothing to reopen.
func (ts *tabset) persistIDs() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]string, 0, len(ts.tabs))
	for _, t := range ts.tabs {
		if t.store != nil && t.store.Path() != "" {
			out = append(out, t.store.Path())
		}
	}
	return out
}

// summary is the status-row reading: how many sessions are open, how many are
// mid-turn, and how many are parked with unread output.
func (ts *tabset) summary() (open, busy, unread int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for i, t := range ts.tabs {
		open++
		if t.running {
			busy++
		}
		if i != ts.cur && t.unread {
			unread++
		}
	}
	return
}

// snapshot returns a copy of the open tabs for the UI (id, title, flags).
func (ts *tabset) snapshot() []tabInfo {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]tabInfo, len(ts.tabs))
	for i, t := range ts.tabs {
		out[i] = tabInfo{
			ID:      t.id,
			Title:   t.title,
			Running: t.running,
			Unread:  t.unread,
			Current: i == ts.cur,
		}
	}
	return out
}

// tabInfo is the UI-facing view of one open session.
type tabInfo struct {
	ID, Title                string
	Running, Unread, Current bool
}

// closeAll closes every store, for the process-exit path.
func (ts *tabset) closeAll() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range ts.tabs {
		_ = t.store.Close()
	}
	ts.tabs = nil
	ts.cur = 0
}
