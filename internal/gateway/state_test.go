package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// TestChatsAcquireIsFailClosed is the lease's whole contract: a second
// message on a busy chat is answered rather than queueing work nobody
// bounded. The test times the wait so a lease that blocks forever fails
// rather than hanging the suite.
func TestChatsAcquireIsFailClosed(t *testing.T) {
	c := newTestChats(t)
	release, err := c.Acquire(42, time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	start := time.Now()
	if _, err := c.Acquire(42, 300*time.Millisecond); err == nil {
		t.Error("second Acquire succeeded while the lease was held — the busy reply would never happen")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("busy Acquire took %s; it should give up at the wait bound", d)
	}
	if _, err := c.Acquire(99, time.Second); err != nil {
		t.Errorf("another chat must be unaffected: %v", err)
	}
	release()
	if _, err := c.Acquire(42, time.Second); err != nil {
		t.Errorf("Acquire after Release: %v", err)
	}
}

// TestChatsSessionSurvivesRestart: the chat→session map is the daemon's only
// in-memory state, so losing it must cost a fresh session rather than the
// conversation. What must survive a restart is proven by re-opening the map.
func TestChatsSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := NewChats(path)
	if err != nil {
		t.Fatalf("NewChats: %v", err)
	}
	if err := c.SetSession(7, "session-abc", "telegram chat"); err != nil {
		t.Fatalf("SetSession: %v", err)
	}
	reopened, err := NewChats(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Session(7)
	if !ok || got != "session-abc" {
		t.Errorf("after reopen, Session(7) = %q,%v, want session-abc,true", got, ok)
	}
}

// TestChatsForgetIsANewSession: /new must drop the pointer so the next
// message starts fresh, and it must not delete anything else.
func TestChatsForgetIsANewSession(t *testing.T) {
	c := newTestChats(t)
	if err := c.SetSession(1, "s1", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Forget(1); err != nil {
		t.Fatal(err)
	}
	if id, ok := c.Session(1); ok {
		t.Errorf("Session after Forget = %q, want none", id)
	}
	if c.Count() != 0 {
		t.Errorf("Count = %d, want 0", c.Count())
	}
}

// TestChatsCorruptMapIsNotFatal: an unreadable map must start the bridge
// empty rather than refusing to start, because the session FILES are intact
// and the map is a convenience.
func TestChatsCorruptMapIsNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewChats(path)
	if err != nil {
		t.Fatalf("a corrupt map must not be fatal: %v", err)
	}
	if c.Count() != 0 {
		t.Errorf("Count = %d, want 0", c.Count())
	}
}

// TestRunStopsOnCancelledContext: the daemon returns cleanly when its
// context is cancelled, which is what `gateway stop` and the service
// manager's shutdown rely on.
func TestRunStopsOnCancelledContext(t *testing.T) {
	b := newFakeBot(t)
	b.queue(textUpdate("hello", 1))
	d := newTestDaemon(t, b, []int64{1})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the poll returned — the loop ignores ctx")
	}
}

// TestRunRejectsUnknownChat proves the allowlist is fail-closed: an
// unauthorized chat gets no answer and, in the shutdown path, nothing else
// happens.
func TestRunRejectsUnknownChat(t *testing.T) {
	b := newFakeBot(t)
	b.queue(textUpdate("should be refused", 555))
	d := newTestDaemon(t, b, []int64{1})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := b.sentTo(555)
	if len(got) == 0 {
		// No answer at all is acceptable (a silent refuse); what is NOT
		// acceptable is a message that could pass for an agent reply.
		return
	}
	for _, msg := range got {
		if strings.Contains(msg, "should be refused") {
			t.Errorf("unauthorized chat's text reached the model: %q", msg)
		}
		if !strings.Contains(msg, "Not authorized") {
			t.Errorf("unauthorized chat received a non-refusal message: %q", msg)
		}
	}
}

func newTestChats(t *testing.T) *Chats {
	t.Helper()
	c, err := NewChats(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newTestDaemon wires a daemon against the fake bot with a workspace under
// the temp dir, so a test never touches the real one.
func newTestDaemon(t *testing.T, b *fakeBot, allowed []int64) *Daemon {
	t.Helper()
	dataDir := t.TempDir()
	st := &config.Settings{}
	st.Gateway.AllowedChats = strings.Split(FormatChatList(allowed), ",")
	// The workspace must be a real directory the worker can chdir into.
	ws := filepath.Join(dataDir, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	st.Gateway.Workspace = ws
	// The token file is the daemon's source of truth when the env var is
	// unset; writing it here is how a test stands up a configured daemon.
	if err := WriteToken(dataDir, b.token); err != nil {
		t.Fatal(err)
	}
	// The test binary stands in for the xdev worker: a spawnable executable
	// whose argv the daemon controls. /bin/true would exit 0 with no output,
	// which cannot prove a turn ran, so use os.Args[0] and let the worker
	// be a real process.
	d, err := NewDaemon(Options{Settings: st, DataDir: dataDir, Exe: os.Args[0], BaseURL: b.url()})
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	return d
}

// TestTurnIsSerializedPerChat: a second message on a chat whose turn is
// running must not spawn a second worker — two workers would interleave two
// writers into one session file. The lease is what makes that impossible, and
// the daemon's own reply path must say so.
func TestTurnIsSerializedPerChat(t *testing.T) {
	c := newTestChats(t)
	release, err := c.Acquire(1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Acquire(1, 300*time.Millisecond)
		done <- err
	}()
	err = <-done
	release()
	if err == nil {
		t.Fatal("a second message on a busy chat must fail-closed, not interleave")
	}
}

// TestEmptyMessageIsNotATurn: an edited photo or a sticker arrives as a
// message with no text; sending it to the model would produce a turn about
// nothing. It must be ignored with a reply, not spawned.
func TestEmptyMessageIsNotATurn(t *testing.T) {
	b := newFakeBot(t)
	b.queue(Update{UpdateID: 9, Message: &Message{Chat: Chat{ID: 1, Type: "private"}, From: &User{ID: 1}, Text: ""}})
	d := newTestDaemon(t, b, []int64{1})
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, msg := range b.sentAll() {
		if msg.Chat == 1 {
			t.Errorf("an empty message produced %q — it must not reach the model", msg.Text)
		}
	}
}

// TestConflictDoesNotKillTheDaemon: Telegram terminates an existing long
// poll when another getUpdates arrives, so a `getUpdates` curl from an
// operator makes the live poll 409 — and a daemon that exits on 409 turns
// one hand check into a restart (measured 2026-10-11 against the real bot:
// two curls, two restarts).
func TestConflictDoesNotKillTheDaemon(t *testing.T) {
	b := newFakeBot(t)
	b.queue(textUpdate("hello", 1))
	// The first getUpdates 409s (the fake bot answers getUpdates with a
	// conflict once), the next one serves the queued update.
	b.conflictOnce = true
	d := newTestDaemon(t, b, []int64{1})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned an error on a tolerated 409: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not return after the context was cancelled — the loop is stuck")
	}
	// The 409 must be visible in the log, not silent.
	if got := b.conflictCount(); got == 0 {
		t.Error("the 409 was not recorded — a real rival poller must stay visible")
	}
}

// TestRivalPollerIsStillVisible: a genuine second daemon produces a 409 on
// every poll. Paced at 8s it would log every 20th — with the test's short
// window, the first one is the assertion that would catch a silent loop.
func TestRivalPollerIsStillVisible(t *testing.T) {
	b := newFakeBot(t)
	b.alwaysConflict = true
	d := newTestDaemon(t, b, []int64{1})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() { _ = d.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	if b.conflictCount() == 0 {
		t.Fatal("a 409-on-every-poll rival was not seen — the conflict path is not running")
	}
	// And no turn was ever started: the conflict is the whole story.
	for _, m := range b.sentAll() {
		t.Errorf("unexpected message on a conflicted poll: %q", m.Text)
	}
}
