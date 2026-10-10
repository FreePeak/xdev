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
	d, err := NewDaemon(Options{Settings: st, DataDir: dataDir, Exe: "/bin/true", BaseURL: b.url()})
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	return d
}
