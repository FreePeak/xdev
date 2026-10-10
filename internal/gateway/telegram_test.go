package gateway

import (
	"context"
	"encoding/json"
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
	// failAfter makes sendMessage fail once this many messages have been
	// sent (0 = never fail), which is how a partial send is tested.
	failAfter int
	// lastOffset/lastTimeout capture the most recent getUpdates payload.
	lastOffset  int64
	lastTimeout int

	srv *httptest.Server
}

type sentMsg struct {
	Chat int64
	Text string
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
		var out []Update
		if len(b.updates) > 0 {
			out = b.updates
			b.updates = nil
		}
		b.mu.Unlock()
		if out == nil {
			out = []Update{}
		}
		writeOK(w, out)
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
		b.sent = append(b.sent, sentMsg{Chat: payload.ChatID, Text: payload.Text})
		n := len(b.sent)
		b.mu.Unlock()
		writeOK(w, map[string]any{"message_id": int64(n)})
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

func (b *fakeBot) sentAll() []sentMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sentMsg(nil), b.sent...)
}

func (b *fakeBot) url() string { return b.srv.URL }

func textUpdate(text string, chat int64) Update {
	return Update{UpdateID: time.Now().UnixNano(), Message: &Message{Text: text, Chat: Chat{ID: chat, Type: "private"}, From: &User{ID: chat, Username: "tester"}}}
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
