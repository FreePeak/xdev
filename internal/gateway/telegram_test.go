package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBot is a Bot API stub: it answers getMe, serves queued updates from
// getUpdates, and records every sendMessage. Everything the daemon does is
// asserted against what this server actually received.
type fakeBot struct {
	mu    sync.Mutex
	token string

	// updates are served one per getUpdates call.
	updates []Update
	// sent records (chatID, text) per sendMessage.
	sent []sentMsg
	// edits records (messageID, text) per editMessageText — the streaming
	// preview's only write path.
	edits []sentEdit
	// failEdit makes editMessageText answer 400 "not a parse problem", the
	// shape of a permanent refusal the preview must seal on.
	failEdit bool
	// actions records (chatID, action) per sendChatAction.
	actions []sentAction
	// failAfter makes sendMessage fail once this many messages have been
	// sent (0 = never fail), which is how a partial send is tested.
	failAfter int
	// conflictOnce makes the first getUpdates answer 409 — the shape of
	// "another client poked this token just now".
	conflictOnce bool
	// alwaysConflict makes every getUpdates answer 409, the shape of a
	// real rival poller.
	alwaysConflict bool
	// conflicts counts the 409s served.
	conflicts int
	// lastOffset/lastTimeout capture the most recent getUpdates payload.
	lastOffset  int64
	lastTimeout int

	srv *httptest.Server
}

type sentMsg struct {
	Chat      int64
	Text      string
	MessageID int64
}

type sentEdit struct {
	MessageID int64
	Text      string
}

type sentAction struct {
	Chat   int64
	Action string
}

func newFakeBot(t *testing.T) *fakeBot {
	t.Helper()
	b := &fakeBot{token: "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest"}
	mux := http.NewServeMux()
	mux.HandleFunc("/bot"+b.token+"/getMe", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"id": 42, "username": "xdevtestbot", "first_name": "xdev"})
	})
	mux.HandleFunc("/bot"+b.token+"/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Offset  int64 `json:"offset"`
			Timeout int   `json:"timeout"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		b.mu.Lock()
		b.lastOffset, b.lastTimeout = payload.Offset, payload.Timeout
		// A 409 is how Telegram terminates an EXISTING long poll when a new
		// getUpdates arrives. conflictOnce is the "something poked the API"
		// shape; alwaysConflict is a real rival poller.
		conflict := b.alwaysConflict
		if !conflict && b.conflictOnce {
			conflict = true
			b.conflictOnce = false
		}
		var out []Update
		if len(b.updates) > 0 {
			out = b.updates
			b.updates = nil
		}
		b.mu.Unlock()
		if conflict {
			b.mu.Lock()
			b.conflicts++
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`))
			return
		}
		if out == nil {
			out = []Update{}
		}
		writeOK(w, out)
	})
	mux.HandleFunc("/bot"+b.token+"/sendChatAction", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			ChatID int64  `json:"chat_id"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		b.actions = append(b.actions, sentAction{Chat: payload.ChatID, Action: payload.Action})
		b.mu.Unlock()
		writeOK(w, true)
	})
	mux.HandleFunc("/bot"+b.token+"/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		if b.failAfter > 0 && len(b.sent) >= b.failAfter {
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry later"}`))
			return
		}
		n := len(b.sent) + 1
		b.sent = append(b.sent, sentMsg{Chat: payload.ChatID, Text: payload.Text, MessageID: int64(n)})
		b.mu.Unlock()
		writeOK(w, map[string]any{"message_id": int64(n)})
	})
	mux.HandleFunc("/bot"+b.token+"/editMessageText", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			MessageID int64  `json:"message_id"`
			Text      string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		fail := b.failEdit
		if !fail {
			b.edits = append(b.edits, sentEdit{MessageID: payload.MessageID, Text: payload.Text})
		}
		b.mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry later"}`))
			return
		}
		writeOK(w, map[string]any{"message_id": payload.MessageID})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	b.srv = srv
	return b
}

func writeOK(w http.ResponseWriter, result any) {
	body, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (b *fakeBot) queue(u ...Update) {
	b.mu.Lock()
	b.updates = append(b.updates, u...)
	b.mu.Unlock()
}

func (b *fakeBot) sentTo(chat int64) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, m := range b.sent {
		if m.Chat == chat {
			out = append(out, m.Text)
		}
	}
	return out
}

// actionCount is how many chat actions the fake bot has served.
func (b *fakeBot) actionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.actions)
}

// sentActions returns a copy of the recorded chat actions.
func (b *fakeBot) sentActions() []sentAction {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sentAction(nil), b.actions...)
}

func (b *fakeBot) sentAll() []sentMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sentMsg(nil), b.sent...)
}

// sentEdits returns a copy of the recorded editMessageText calls.
func (b *fakeBot) sentEdits() []sentEdit {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sentEdit(nil), b.edits...)
}

func (b *fakeBot) url() string { return b.srv.URL }

// conflictCount is how many 409s the fake bot has served (mutex-guarded read,
// the same one the handlers take).
func (b *fakeBot) conflictCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conflicts
}

func textUpdate(text string, chat int64) Update {
	return Update{UpdateID: time.Now().UnixNano(), Message: &Message{Text: text, Chat: Chat{ID: chat, Type: "private"}, From: &User{ID: chat, Username: "tester"}}}
}

// TestTypingKeepsTheBubbleAliveDuringATurn is the reported behaviour: while a
// turn is running and nothing has been sent yet, the chat must show the
// typing indicator, refreshed — Telegram expires it after ~5s, so one post at
// t=0 would leave a long model call looking idle. The loop is driven with a
// short interval so the test measures the cadence rather than sleeping 5s.
func TestTypingKeepsTheBubbleAliveDuringATurn(t *testing.T) {
	b := newFakeBot(t)
	tg := &Telegram{Token: b.token, BaseURL: b.url(), HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	done := make(chan struct{})
	stop := startTyping(context.Background(), tg, 42, done, 30*time.Millisecond)
	time.Sleep(140 * time.Millisecond)
	stop()
	time.Sleep(80 * time.Millisecond)

	posted := b.sentActions()
	if len(posted) < 3 {
		t.Fatalf("typing posted %d times in 140ms at a 30ms interval, want >= 3 (the bubble expires every ~5s)", len(posted))
	}
	for _, a := range posted {
		if a.Action != "typing" {
			t.Errorf("chat action %q, want typing", a.Action)
		}
		if a.Chat != 42 {
			t.Errorf("chat action went to chat %d, want 42", a.Chat)
		}
	}
	// After the stop func, a turn that has finished must not keep posting:
	// the bubble belongs to the running turn alone. The settle sleep lets a
	// post already in flight land, so the assertion is about the loop, not
	// about a race with the request in progress.
	time.Sleep(60 * time.Millisecond)
	after := len(b.sentActions())
	time.Sleep(120 * time.Millisecond)
	if got := len(b.sentActions()); got != after {
		t.Errorf("typing kept posting after stop (%d -> %d)", after, got)
	}
}

// TestTypingStopsWhenTheWorkerEnds: the same loop must also exit when the
// worker's done channel closes, with no stop call — that is how /stop and a
// crashed worker end the bubble.
func TestTypingStopsWhenTheWorkerEnds(t *testing.T) {
	b := newFakeBot(t)
	tg := &Telegram{Token: b.token, BaseURL: b.url(), HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	done := make(chan struct{})
	_ = startTyping(context.Background(), tg, 7, done, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	close(done)
	time.Sleep(60 * time.Millisecond) // let a post already in flight land
	after := len(b.sentActions())
	time.Sleep(120 * time.Millisecond)
	if got := len(b.sentActions()); got != after {
		t.Errorf("typing kept posting after the worker ended (%d -> %d)", after, got)
	}
}

// TestTypingFailureIsNotFatal: a chat action the API rejects must stay a log
// line. The daemon's contract is that the ANSWER is what matters, so the
// typing loop keeps running and the turn is unaffected.
func TestTypingFailureIsNotFatal(t *testing.T) {
	tg := &Telegram{Token: "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest",
		BaseURL:    "http://127.0.0.1:1", // reserved: nothing listens
		HTTPClient: &http.Client{Timeout: 200 * time.Millisecond}}
	done := make(chan struct{})
	stop := startTyping(context.Background(), tg, 1, done, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	stop() // must not panic or block on the failed sends
}

// TestTurnPostsTypingWhileTheWorkerRuns: the wiring, not just the loop — an
// inbound chat message must produce a typing action before its answer.
func TestTurnPostsTypingWhileTheWorkerRuns(t *testing.T) {
	b := newFakeBot(t)
	b.queue(textUpdate("hi", 5))
	d := newTestDaemon(t, b, []int64{5})
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(b.sentActions()) == 0 {
		t.Fatal("a turn ran with no sendChatAction — the phone would show no typing bubble")
	}
}

// TestChunkTextSplitsOnUTF16NotRunes pins the rule a rune-count split gets
// wrong: the limit is UTF-16 code units, so an emoji-heavy message must be
// cut on a code-unit boundary and must never leave a lone surrogate behind.
func TestChunkTextSplitsOnUTF16NotRunes(t *testing.T) {
	// 3000 emoji = 6000 UTF-16 units, 3000 runes, 12000 bytes.
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		sb.WriteRune('😀')
	}
	chunks := ChunkText(sb.String(), 4000)
	if len(chunks) < 2 {
		t.Fatalf("want at least 2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if got := utf16Len(c); got > 4000 {
			t.Errorf("chunk %d = %d UTF-16 units, want <= 4000", i, got)
		}
	}
	joined := strings.Join(chunks, "")
	if joined != sb.String() {
		t.Errorf("chunks do not reassemble into the original text (lost %d units)", utf16Len(sb.String())-utf16Len(joined))
	}
}

// TestChunkTextPrefersNewlineBoundary: the second rule is that a cut near a
// newline moves to it, so a code block is not sliced mid-line when a nearby
// break exists.
func TestChunkTextPrefersNewlineBoundary(t *testing.T) {
	line := strings.Repeat("x", 3900)
	text := line + "\n" + strings.Repeat("y", 200)
	chunks := ChunkText(text, 4000)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d: %q", len(chunks), chunks)
	}
	if !strings.HasSuffix(chunks[0], "\n") {
		t.Errorf("first chunk should break at the newline, got suffix %q", chunks[0][len(chunks[0])-8:])
	}
}

// TestChunkTextEmptyIsNothing: a turn with no answer sends nothing rather
// than an empty bubble.
func TestChunkTextEmptyIsNothing(t *testing.T) {
	if got := ChunkText("", 4000); got != nil {
		t.Errorf("ChunkText(\"\") = %v, want nil", got)
	}
	if got := ChunkText("\n\n", 4000); got != nil {
		t.Errorf("ChunkText(\"\\n\\n\") = %v, want nil", got)
	}
}

// TestChunkTextDefaultLimitCapsAtTelegram: a caller passing 0 or an
// over-large limit gets the safe default, never a value the API rejects.
func TestChunkTextDefaultLimitCapsAtTelegram(t *testing.T) {
	long := strings.Repeat("a", 9000)
	for _, limit := range []int{0, -1, 99999} {
		chunks := ChunkText(long, limit)
		if len(chunks) == 0 {
			t.Fatalf("limit %d: no chunks", limit)
		}
		rebuilt := strings.Join(chunks, "")
		if rebuilt != long {
			t.Errorf("limit %d: chunks lost text (%d of 9000 units)", limit, len(rebuilt))
		}
		for i, c := range chunks {
			if got := len(c); got > TelegramMessageLimit {
				t.Errorf("limit %d: chunk %d = %d units, want <= %d", limit, i, got, TelegramMessageLimit)
			}
		}
	}
}

// TestValidTokenShape pins the BotFather shape check, including the two
// mistakes that matter: a pasted sentence and a token with an illegal
// separator character.
func TestValidTokenShape(t *testing.T) {
	cases := map[string]bool{
		"111111111:" + strings.Repeat("a", 30):               true,
		"111111111:TESTtestTESTtestTESTtestTESTtestTESTtest": true,
		"not a token at all":                                 false,
		"123456789:":                                         false,
		":AA" + strings.Repeat("a", 30):                      false,
		"123456789:short":                                    false,
		"12:34:AA" + strings.Repeat("a", 30):                 false, // colon in the secret
		" 111111111:" + strings.Repeat("a", 30) + " ":        true,  // trimmed
		"abc:AA" + strings.Repeat("a", 30):                   false, // non-numeric id
	}
	for tok, want := range cases {
		if got := ValidTokenShape(tok); got != want {
			t.Errorf("ValidTokenShape(%q) = %v, want %v", tok, got, want)
		}
	}
}

// TestMaskTokenNeverLeaksSecret: a masked token must not contain the middle
// of the secret, because status output and logs are the only places a token
// could leak.
func TestMaskTokenNeverLeaksSecret(t *testing.T) {
	tok := "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest"
	masked := MaskToken(tok)
	if strings.Contains(masked, tok[len(tok)-16:]) {
		t.Errorf("masked token leaked the secret: %q", masked)
	}
	if len(masked) > 8 {
		t.Errorf("masked token is %d chars, want <= 8", len(masked))
	}
}

// TestGetUpdatesSendsOffsetAndTimeout: the long-poll request must carry the
// acknowledgement offset and the timeout, or Telegram redelivers every
// update the daemon already handled.
func TestGetUpdatesSendsOffsetAndTimeout(t *testing.T) {
	b := newFakeBot(t)
	b.queue(textUpdate("hello", 100))
	tg := &Telegram{Token: b.token, BaseURL: b.url()}
	updates, err := tg.GetUpdates(context.Background(), 4242, 7)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 1 || updates[0].Message == nil || updates[0].Message.Text != "hello" {
		t.Fatalf("want the queued text update, got %+v", updates)
	}
	b.mu.Lock()
	gotOffset, gotTimeout := b.lastOffset, b.lastTimeout
	b.mu.Unlock()
	if gotOffset != 4242 {
		t.Errorf("offset sent = %d, want 4242", gotOffset)
	}
	if gotTimeout != 7 {
		t.Errorf("timeout sent = %d, want 7", gotTimeout)
	}
}

// TestTransportErrorNeverCarriesTheToken: a net/http error quotes the URL,
// and the URL carries the bot token — so one stale-token retry logs the
// secret. Measured on the live bridge 2026-10-11: the daemon log held
// `getMe: Post "https://api.telegram.org/bot8271667235:AAH…/getMe"`.
func TestTransportErrorNeverCarriesTheToken(t *testing.T) {
	tok := "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest"
	url := "https://api.telegram.org/bot" + tok + "/getMe"
	// The real transport's shape for a cancelled request.
	raw := errors.New(`Post "` + url + `": context canceled`)
	got := maskTokenInError(raw, tok).Error()
	if strings.Contains(got, tok) {
		t.Fatalf("the token survived the mask: %q", got)
	}
	if !strings.Contains(got, "…") || !strings.Contains(got, "/bot") {
		t.Errorf("the masked error lost the endpoint too: %q", got)
	}
	if !strings.Contains(got, "context canceled") {
		t.Errorf("the underlying reason was lost: %q", got)
	}
}

// TestTransportErrorIsLeftAlone: a token-free error (a dial failure naming
// the host but not the bot) must pass through untouched, so the masking is
// not a blanket rewrite.
func TestTransportErrorIsLeftAlone(t *testing.T) {
	raw := errors.New(`dial tcp 127.0.0.1:8899: connect: connection refused`)
	if got := maskTokenInError(raw, "111111111:secretvalue").Error(); got != raw.Error() {
		t.Errorf("error changed: %q", got)
	}
}

// TestCallMasksTokenToAnUnreachableAPI: the client against a dead endpoint
// must report the masked endpoint, not the secret.
func TestCallMasksTokenToAnUnreachableAPI(t *testing.T) {
	tg := &Telegram{Token: "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest",
		BaseURL:    "http://127.0.0.1:1", // reserved: nothing listens
		HTTPClient: &http.Client{Timeout: time.Second}}
	_, err := tg.GetMe(context.Background())
	if err == nil {
		t.Fatal("an unreachable API must error")
	}
	if strings.Contains(err.Error(), "111111111:TESTtest") {
		t.Errorf("the error leaked the token: %v", err)
	}
}
