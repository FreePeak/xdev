package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPreviewTextCutsInsteadOfSplitting pins the rule the whole preview rests
// on: a preview that outgrows Telegram's cap is CUT with a marker, never
// split. Splitting would create a second message and move the edit target,
// and the next tick would re-edit the whole text into it — the duplication
// loop Hermes documents at adapter.py:3880.
func TestPreviewTextCutsInsteadOfSplitting(t *testing.T) {
	long := strings.Repeat("a", 9000)
	got := previewText(long, 4000)
	if n := utf16Len(got); n > 4000 {
		t.Errorf("preview = %d UTF-16 units, want <= 4000", n)
	}
	if !strings.HasSuffix(got, streamTail) {
		t.Errorf("a cut preview must say so, got suffix %q", got[len(got)-20:])
	}
	// Emoji are two UTF-16 units each: a cut through one must not leave a lone
	// surrogate behind (Telegram rejects the whole edit).
	emoji := strings.Repeat("😀", 3000)
	cut := previewText(emoji, 4000)
	if n := utf16Len(cut); n > 4000 {
		t.Errorf("emoji preview = %d units, want <= 4000", n)
	}
	if body := strings.TrimSuffix(cut, streamTail); body == "" || !strings.HasPrefix(emoji, body) {
		t.Errorf("the cut is not a prefix of the answer: %q", cut)
	}
	// Under the cap: untouched, no marker.
	short := "**hi**"
	if got := previewText(short, 4000); got != short {
		t.Errorf("short preview was altered: %q", got)
	}
}

// TestEditMessageTextTreatsStaleEditsAsSuccess: three Telegram answers mean
// "this edit had nothing to do" — the text is already there, the message is
// gone, or it is past the edit window. Reporting them as failures would fill
// the log with non-events on every quiet turn, and the answer is delivered
// either way.
func TestEditMessageTextTreatsStaleEditsAsSuccess(t *testing.T) {
	for _, desc := range []string{
		"Bad Request: message is not modified: specified new message content and reply markup are exactly the same",
		"Bad Request: message to edit not found",
		"Bad Request: message can't be edited",
		"Bad Request: message is too old to be edited",
	} {
		b := newFakeBot(t)
		b.editFails = map[string]string{"editMessageText": desc}
		tg := &Telegram{Token: b.token, BaseURL: b.url()}
		if err := tg.EditMessageText(context.Background(), 1, 2, "same"); err != nil {
			t.Errorf("%q: want success, got %v", desc, err)
		}
	}
	// A real refusal still surfaces.
	b := newFakeBot(t)
	b.editFails = map[string]string{"editMessageText": "Forbidden: bot was blocked by the user"}
	tg := &Telegram{Token: b.token, BaseURL: b.url()}
	if err := tg.EditMessageText(context.Background(), 1, 2, "x"); err == nil {
		t.Error("a forbidden edit must be reported, not swallowed")
	}
}

// TestSendMessageIDReturnsTheEditTarget is the preview's contract with the
// API: the id sendMessage returns is what every later edit addresses. Without
// it there is nothing to edit, and the preview would have to re-send.
func TestSendMessageIDReturnsTheEditTarget(t *testing.T) {
	b := newFakeBot(t)
	tg := &Telegram{Token: b.token, BaseURL: b.url()}
	id, err := tg.SendMessageID(context.Background(), 7, "**hi**")
	if err != nil {
		t.Fatalf("SendMessageID: %v", err)
	}
	if id == 0 {
		t.Fatal("SendMessageID returned no message id — nothing would be editable")
	}
	if got := b.sentTo(7); len(got) != 1 || !strings.Contains(got[0], "<b>hi</b>") {
		t.Errorf("the rendered body did not reach the chat: %v", got)
	}
}

// TestRunPreviewGrowsOneMessage is the feature's core claim: while the turn
// runs, the chat shows ONE message whose text grows — the first chunk creates
// it and every later chunk edits that same message id.
func TestRunPreviewGrowsOneMessage(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.streamEvery = 10 * time.Millisecond
	pv := newPreview()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); d.runPreview(ctx, 1, pv, stop, done) }()

	pv.write("part one\n")
	waitFor(t, func() bool { return len(b.sentAll()) >= 1 }, "the first paint")
	pv.write("part two\n")
	waitFor(t, func() bool { return len(b.editedAll()) >= 1 }, "the second paint")
	close(stop)
	wg.Wait()

	sends := b.sentAll()
	if len(sends) != 1 {
		t.Fatalf("the chat saw %d sends, want exactly 1 (one growing message)", len(sends))
	}
	edits := b.editedAll()
	if len(edits) == 0 {
		t.Fatal("the answer never grew: no edit addressed the preview message")
	}
	for _, e := range edits {
		if e.MessageID != b.sentID(0) {
			t.Fatalf("an edit addressed message %d, not the one the send created (%d)", e.MessageID, b.sentID(0))
		}
	}
	if last := edits[len(edits)-1].Text; !strings.Contains(last, "part two") {
		t.Errorf("the last paint was %q, want it to carry the newest text", last)
	}
}

// TestFloodStopsThePreview: Telegram's limiter is per chat, and a bridge that
// keeps editing into it escalates the penalty. One 429 must end the preview —
// the finished answer is still delivered when the turn ends.
func TestFloodStopsThePreview(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.streamEvery = 10 * time.Millisecond
	var mu sync.Mutex
	var logged []string
	logf := func(f string, a ...any) { mu.Lock(); logged = append(logged, f); mu.Unlock() }
	d.logf, d.tg.Logf = logf, logf
	pv := newPreview()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() { d.runPreview(ctx, 1, pv, stop, done); close(finished) }()

	// The first paint CREATES the message (sendMessage is not the failing
	// method); the flood only bites on the edit that follows new text.
	pv.write("growing")
	waitFor(t, func() bool { return len(b.sentAll()) >= 1 }, "the first paint")
	b.editFails = map[string]string{"editMessageText": "Too Many Requests: retry later"}
	pv.write(" and growing")
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("a 429 did not stop the preview — it would keep editing into the limiter")
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "when the turn ends") {
		t.Errorf("the flood was not reported as such: %q", joined)
	}
	if len(b.editedAll()) != 0 {
		t.Errorf("a refused edit was recorded as painted: %+v", b.editedAll())
	}
}

// TestTurnFinishesTheAnswerInThePreviewMessage: the finished answer is written
// INTO the message the preview created — one message the whole way through,
// not a partial bubble plus a fresh one.
func TestTurnFinishesTheAnswerInThePreviewMessage(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	pv := newPreview()
	pv.write("the whole ")
	pv.sentTo(4242)
	pv.painted("the whole ")

	if err := d.deliver(context.Background(), 1, pv, "", "the whole answer"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if sends := b.sentAll(); len(sends) != 0 {
		t.Fatalf("the answer was re-sent instead of edited in: %+v", sends)
	}
	edits := b.editedAll()
	if len(edits) != 1 {
		t.Fatalf("want one final edit, got %+v", edits)
	}
	if edits[0].MessageID != 4242 || edits[0].Text != "the whole answer" {
		t.Errorf("final edit = id %d %q, want id 4242 carrying the whole answer", edits[0].MessageID, edits[0].Text)
	}
}

// TestDeliverSkipsAnIdenticalFinalEdit: when the last paint already showed the
// whole answer, an identical edit is Telegram's "message is not modified" 400.
// The answer is already on screen, so the right number of calls is none.
func TestDeliverSkipsAnIdenticalFinalEdit(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	pv := newPreview()
	pv.sentTo(4242)
	pv.painted("the answer")

	if err := d.deliver(context.Background(), 1, pv, "", "the answer"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if edits := b.editedAll(); len(edits) != 0 {
		t.Errorf("an identical final edit was sent: %+v", edits)
	}
	if sends := b.sentAll(); len(sends) != 0 {
		t.Errorf("the answer was re-sent although the preview showed it: %+v", sends)
	}
}

// TestDeliverPrefixesTheSwitchCaption: when the gateway served another model,
// the caption rides the SAME message as the answer — a separate bubble would
// read as a second event.
func TestDeliverPrefixesTheSwitchCaption(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	pv := newPreview()
	pv.sentTo(77)
	pv.painted("partial")

	if err := d.deliver(context.Background(), 1, pv, "⇄ model switched: a → b", "the answer"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	edits := b.editedAll()
	if len(edits) != 1 {
		t.Fatalf("want one final edit, got %+v", edits)
	}
	if !strings.HasPrefix(edits[0].Text, "⇄ ") || !strings.HasSuffix(edits[0].Text, "the answer") {
		t.Errorf("caption and answer must share one message, got %q", edits[0].Text)
	}
}

// TestDeliverSplitsOverflowAfterTheFirstChunk: an answer longer than the cap
// finishes in the preview message and continues in follow-ups — the phone
// shows one answer, not a fresh bubble per 4096 characters.
func TestDeliverSplitsOverflowAfterTheFirstChunk(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.chunk = 1000
	pv := newPreview()
	pv.sentTo(55)
	pv.painted("head")

	long := strings.Repeat("x", 2500)
	if err := d.deliver(context.Background(), 1, pv, "", long); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	edits := b.editedAll()
	if len(edits) != 1 {
		t.Fatalf("want exactly one edit (the head chunk), got %+v", edits)
	}
	if utf16Len(edits[0].Text) > 1000 {
		t.Errorf("the edited head is %d units, over the cap", utf16Len(edits[0].Text))
	}
	sends := b.sentAll()
	if len(sends) != 2 {
		t.Fatalf("want the overflow as 2 follow-up chunks, got %d", len(sends))
	}
	rebuilt := edits[0].Text + sends[0].Text + sends[1].Text
	if rebuilt != long {
		t.Errorf("the split lost text: %d of %d characters", len(rebuilt), len(long))
	}
}

// TestStreamOffMeansOneReply: gateway.stream false is the operator's choice —
// no message until the turn ends, exactly the behaviour before the preview.
func TestStreamOffMeansOneReply(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.stream = false // what NewDaemon sets from gateway.stream: false
	pv := newPreview()
	if err := d.deliver(context.Background(), 1, pv, "", "the answer"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := b.sentTo(1); len(got) != 1 || got[0] != "the answer" {
		t.Errorf("want the answer as one plain reply, got %v", got)
	}
	if len(b.editedAll()) != 0 {
		t.Errorf("nothing must be edited when the preview never ran: %+v", b.editedAll())
	}
}

// TestStaleEditDetection is the narrow rule behind the swallowed 400s: it must
// not swallow a 400 that means something else, and must not swallow a 429 —
// flood has its own path (stop the preview).
func TestStaleEditDetection(t *testing.T) {
	cases := map[string]bool{
		"message is not modified":     true,
		"message to edit not found":   true,
		"message can't be edited":     true,
		"message is too old":          true,
		"chat not found":              false,
		"can't parse entities":        false,
		"bot was blocked by the user": false,
	}
	for desc, want := range cases {
		err := &APIError{Method: "editMessageText", Code: http.StatusBadRequest, Description: desc}
		if got := isStaleEdit(err); got != want {
			t.Errorf("isStaleEdit(%q) = %v, want %v", desc, got, want)
		}
	}
	if isStaleEdit(nil) {
		t.Error("nil must not be a stale edit")
	}
	if isStaleEdit(&APIError{Code: http.StatusTooManyRequests, Description: "message is not modified"}) {
		t.Error("a 429 must not be swallowed as a stale edit")
	}
}

// waitFor polls a condition with a deadline. A preview is a timing behaviour,
// so the tests wait for the effect rather than sleeping a guessed duration.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
