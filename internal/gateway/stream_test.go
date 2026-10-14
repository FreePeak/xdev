package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The streaming preview's whole contract: ONE bubble that grows, at a bounded
// cadence, and a final send that does not repeat what the phone already shows.
// The bot stub records edits, so every claim here is about what the API saw.
func TestStreamerEditsOneBubbleAsTheAnswerGrows(t *testing.T) {
	b := newFakeBot(t)
	tg := &Telegram{Token: b.token, BaseURL: b.url(), HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	s := newStreamer(context.Background(), tg, 9, func(string, ...any) {})

	s.write([]byte("the answer is"))
	time.Sleep(editInterval + 300*time.Millisecond) // first tick posts the bubble
	s.write([]byte(" forty-two, and here is why: because."))
	time.Sleep(editInterval + 300*time.Millisecond) // second tick edits it
	s.finish()

	sent := b.sentAll()
	if len(sent) != 1 {
		t.Fatalf("the preview sent %d messages, want exactly 1 (one bubble per answer head): %v", len(sent), sent)
	}
	if !strings.HasPrefix(sent[0].Text, "the answer is") {
		t.Errorf("first bubble = %q, want the answer's head", sent[0].Text)
	}
	edits := b.sentEdits()
	if len(edits) == 0 {
		t.Fatal("no editMessageText: the answer never grew in place")
	}
	last := edits[len(edits)-1]
	if last.MessageID != sent[0].MessageID {
		t.Errorf("the edit targeted message %d, want the bubble %d", last.MessageID, sent[0].MessageID)
	}
	if !strings.Contains(last.Text, "forty-two") {
		t.Errorf("the edit did not carry the grown text: %q", last.Text)
	}
	// The cadence, not the delta count, is what bounds API traffic: 40 writes
	// in one interval must still be one call.
	before := len(b.sentEdits())
	for i := 0; i < 40; i++ {
		s.write([]byte(" more"))
	}
	if got := len(b.sentEdits()); got != before {
		t.Errorf("writes triggered %d immediate edits, want 0 (the ticker owns the cadence)", got-before)
	}
}

// TestStreamerSendsNothingForAnEmptyAnswer: a turn that produces no text must
// not leave an empty bubble behind. Same rule ChunkText("") follows.
func TestStreamerSendsNothingForAnEmptyAnswer(t *testing.T) {
	b := newFakeBot(t)
	tg := &Telegram{Token: b.token, BaseURL: b.url(), HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	s := newStreamer(context.Background(), tg, 9, func(string, ...any) {})
	s.write([]byte("   \n  "))
	time.Sleep(editInterval + 300*time.Millisecond)
	s.finish()
	if got := b.sentAll(); len(got) != 0 {
		t.Errorf("an empty answer produced %v", got)
	}
	if got := s.previewed(); got != "" {
		t.Errorf("previewed() = %q, want empty", got)
	}
}

// TestStreamerSealsOnAPermanentFailure: when the API refuses an edit for a
// reason that is not formatting, the preview must stop — and the answer must
// still arrive, because the final send is what delivers it.
func TestStreamerSealsOnAPermanentFailure(t *testing.T) {
	b := newFakeBot(t)
	b.failEdit = true
	tg := &Telegram{Token: b.token, BaseURL: b.url(), HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	var logged []string
	s := newStreamer(context.Background(), tg, 9, func(f string, a ...any) { logged = append(logged, f) })
	// The first flush sends the bubble (sendMessage is fine); the next one
	// edits it, and THAT is what the API refuses.
	s.write([]byte("an answer that will not stream"))
	time.Sleep(editInterval + 300*time.Millisecond)
	s.write([]byte(" and keeps going with more text than the delta floor"))
	time.Sleep(editInterval + 300*time.Millisecond)
	s.finish()

	if len(logged) == 0 {
		t.Error("a sealed preview must be logged, not silent")
	}
	// previewed() must report nothing once sealed, so the final send delivers
	// the whole answer rather than stripping text that is not on screen.
	if got := s.previewed(); got != "" {
		t.Errorf("previewed() = %q after a failure, want empty (the answer is sent in full)", got)
	}
}

// TestReplySeenStripsOnlyThePreviewedHead: the final send must deliver what the
// phone has NOT seen, and must never mistake a different text (a failure tail,
// a switch caption) for a prefix.
func TestReplySeenStripsOnlyThePreviewedHead(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{7})
	ctx := context.Background()

	seen, err := d.replySeen(ctx, 7, "the answer is forty-two", "the answer is")
	if err != nil {
		t.Fatalf("replySeen: %v", err)
	}
	if strings.TrimSpace(seen) != "forty-two" {
		t.Errorf("sent %q, want only the unseen tail", seen)
	}

	// The whole answer was already on screen: nothing to send, and the preview
	// is the message.
	b2 := newFakeBot(t)
	d2 := newTestDaemon(t, b2, []int64{8})
	if _, err := d2.replySeen(ctx, 8, "the answer is forty-two", "the answer is forty-two"); err != nil {
		t.Fatalf("replySeen: %v", err)
	}
	if got := b2.sentAll(); len(got) != 0 {
		t.Errorf("a fully previewed answer was sent again: %v", got)
	}

	// A text that does not start with what was previewed is sent whole.
	b3 := newFakeBot(t)
	d3 := newTestDaemon(t, b3, []int64{9})
	if _, err := d3.replySeen(ctx, 9, "The turn failed (exit 1).", "some other text"); err != nil {
		t.Fatalf("replySeen: %v", err)
	}
	if got := strings.Join(b3.sentTo(9), ""); !strings.Contains(got, "The turn failed") {
		t.Errorf("an unrelated text was mangled: %q", got)
	}
}

// TestStreamerSealsTheHeadPastTheCap: an answer longer than one Telegram
// message must keep streaming. The bubble on screen becomes the complete head
// of the answer (a message of its own), a fresh preview opens for the tail, and
// the whole answer still arrives as messages the chat can hold — the split is
// ChunkText's, so the boundary rule is the same one the final send uses.
func TestStreamerSealsTheHeadPastTheCap(t *testing.T) {
	b := newFakeBot(t)
	tg := &Telegram{Token: b.token, BaseURL: b.url(), HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	s := newStreamer(context.Background(), tg, 11, func(string, ...any) {})

	line := "0123456789 abcdefghij klmnopqrst\n"
	s.write([]byte(strings.Repeat(line, 700))) // ~20 KB: four times the cap
	time.Sleep(editInterval + 400*time.Millisecond)
	// The tail is now the live preview; more text must EDIT it rather than
	// open yet another bubble.
	s.write([]byte(strings.Repeat(line, 5)))
	time.Sleep(editInterval + 400*time.Millisecond)
	s.finish()

	sent := b.sentAll()
	if len(sent) < 2 {
		t.Fatalf("a 20 KB answer produced %d messages, want at least 2 (the head seals and the tail streams on)", len(sent))
	}
	for i, m := range sent {
		if got := utf16Len(m.Text); got > TelegramMessageLimit {
			t.Errorf("message %d is %d UTF-16 units, over the %d cap", i, got, TelegramMessageLimit)
		}
	}
	// The head is complete, not a truncated stem: it ends where the chunker
	// chose (a newline near the cut).
	if !strings.HasSuffix(sent[0].Text, "\n") {
		t.Errorf("the sealed head does not end on the chunker's boundary: %q", sent[0].Text[len(sent[0].Text)-40:])
	}
	// The preview moved past the sealed head: an edit targets the NEW bubble,
	// not the head message.
	edits := b.sentEdits()
	if len(edits) == 0 {
		t.Fatal("no edit after the seal: the tail never streamed")
	}
	last := edits[len(edits)-1]
	if last.MessageID == sent[0].MessageID {
		t.Errorf("the preview kept editing the sealed head (id %d)", last.MessageID)
	}
	if last.MessageID != sent[len(sent)-1].MessageID {
		t.Errorf("the edit targeted message %d, want the live preview %d", last.MessageID, sent[len(sent)-1].MessageID)
	}
}
