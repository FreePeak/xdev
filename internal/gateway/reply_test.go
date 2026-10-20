package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestReplyRetriesATransientFailure: a rate-limited send must not lose the
// answer. The reply went out once, so one 429 at the moment the answer was
// ready meant the user got nothing at all — and the answer was already in
// hand.
func TestReplyRetriesATransientFailure(t *testing.T) {
	b := newFakeBot(t)
	b.failFirst = 1
	d := newTestDaemon(t, b, []int64{7})
	d.sendBackoff = 10 * time.Millisecond

	if err := d.reply(context.Background(), 7, "the answer"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	got := b.sentTo(7)
	if len(got) != 1 || got[0] != "the answer" {
		t.Fatalf("chat got %v, want the answer to arrive after the retry", got)
	}
}

// TestReplyDoesNotRetryAPermanentRefusal: a refusal Telegram will repeat —
// a chat the bot cannot write to — must not be retried. Three requests that
// all fail is three chances to make it worse, and the answer is not going to
// arrive on the fourth either.
func TestReplyDoesNotRetryAPermanentRefusal(t *testing.T) {
	var mu sync.Mutex
	sends := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sends++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot is blocked"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		tg:          &Telegram{Token: "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest", BaseURL: srv.URL, HTTPClient: srv.Client()},
		chats:       newTestChats(t),
		chunk:       DefaultReplyChunk,
		sendBackoff: 10 * time.Millisecond,
	}
	if err := d.reply(context.Background(), 7, "the answer"); err == nil {
		t.Fatal("a forbidden send must report the refusal")
	}
	mu.Lock()
	defer mu.Unlock()
	if sends != 1 {
		t.Fatalf("sendMessage called %d times, want 1 — a permanent refusal must not be retried", sends)
	}
}

// TestReplyRetriesAnEdgeFailure: a 5xx from the edge (a "502 Bad Gateway"
// whose body is not a Bot API envelope) is the same "try again" moment as a
// transport error. The daemon's log has these by the hundred on a link that
// is having a bad day, and the answer must survive them.
func TestReplyRetriesAnEdgeFailure(t *testing.T) {
	var mu sync.Mutex
	sends := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sends++
		first := sends == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
			return
		}
		writeOK(w, map[string]any{"message_id": 1})
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		tg:          &Telegram{Token: "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest", BaseURL: srv.URL, HTTPClient: srv.Client()},
		chats:       newTestChats(t),
		chunk:       DefaultReplyChunk,
		sendBackoff: 10 * time.Millisecond,
	}
	if err := d.reply(context.Background(), 7, "the answer"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sends < 2 {
		t.Fatalf("sendMessage called %d times, want a retry after the 502", sends)
	}
}

// TestReplyStopsRetryingWhenTheDaemonIsClosing: a shutdown is not a network
// problem. Retrying into a cancelled context only delays the exit.
func TestReplyStopsRetryingWhenTheDaemonIsClosing(t *testing.T) {
	b := newFakeBot(t)
	b.failFirst = 5
	d := newTestDaemon(t, b, []int64{7})
	d.sendBackoff = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.reply(ctx, 7, "the answer"); err == nil {
		t.Fatal("a cancelled reply must report the failure")
	}
	if got := b.sentTo(7); len(got) != 0 {
		t.Fatalf("chat got %v, want nothing after the cancel", got)
	}
}
