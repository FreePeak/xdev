package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// streamInterval is how often a running turn paints its answer into the chat.
// Telegram's limiter is per chat and a chatty model emits a delta every few
// milliseconds, so the message is edited at a human pace rather than per
// token. Hermes uses 0.8s for the same job
// (gateway/config.py DEFAULT_STREAMING_EDIT_INTERVAL) and 1.5s on the turn
// runner's own path (gateway/run_turn_runner.py:729); 1.5s is the one that
// does not trip the limiter on the tool-heavy turns this bridge runs.
const streamInterval = 1500 * time.Millisecond

// streamTail marks a preview cut short by Telegram's 4096 cap. It says why
// the message stops mid-sentence, because a truncated answer with no marker
// reads as a bug in the model.
const streamTail = "\n… (still writing)"

// preview is the one chat message a turn's answer grows in. It holds the
// answer so far, the message it was first painted into, and whether Telegram
// has started refusing edits for this chat.
//
// The text is stdout only: print mode writes the model's words there and
// every narration (thinking, tool lines, notices) to stderr, so a preview
// can never show the phone something the model did not say.
type preview struct {
	mu      sync.Mutex
	text    string
	sent    bool
	msgID   int64
	flooded bool
	// shown is the text the chat already displays — the body of the last
	// successful paint. It exists so the finished answer costs no API call
	// when the last paint was already the whole answer.
	shown string
	// wake carries one pending "there is new text" signal. The painter
	// consumes it and then checks the interval, so a burst of deltas
	// between two paints collapses into the one signal.
	wake chan struct{}
}

// newPreview makes an empty preview: no message exists yet, nothing is
// painted, and the first write wakes the painter immediately.
func newPreview() *preview { return &preview{wake: make(chan struct{}, 1)} }

// write appends one stdout chunk. It runs on os/exec's copier goroutine, so

// it only takes the lock and wakes the painter — no I/O, or a slow API call
// would stall the worker's own stdout.
func (p *preview) write(s string) {
	p.mu.Lock()
	p.text += s
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// state is one consistent read of everything the painter decides on.
func (p *preview) state() (text string, sent bool, id int64, flooded bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.text, p.sent, p.msgID, p.flooded
}

// sentTo records that the chat was written to, and which message now holds
// preview that switched its edit target would leave the earlier partial text
// in the chat forever, which is the duplication Hermes' overflow split warns
// about (adapter.py:3880). An edit that outgrows the cap is cut instead —
// see previewText.
func (p *preview) sentTo(id int64) {
	p.mu.Lock()
	if !p.sent {
		p.sent, p.msgID = true, id
	}
	p.mu.Unlock()
}

// shownBody is the text the chat already displays — the body of the last
// successful paint, so the finished answer can skip an edit that would
// change nothing.
func (p *preview) shownBody() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shown
}

// painted records the body the chat is now displaying.
func (p *preview) painted(text string) {
	p.mu.Lock()
	p.shown = text
	p.mu.Unlock()
}
func (p *preview) markFlooded() {
	p.mu.Lock()
	p.flooded = true
	p.mu.Unlock()
}

// runPreview paints the answer into one chat message while the turn runs: the
// first chunk creates the message, later chunks edit it in place. Without
// this the chat is silent for the whole turn — a two-minute run is two
// minutes of nothing — and then the answer arrives as a run of
// 4096-character bubbles.
//
// It stops on its own channel, on the worker's done channel, and permanently
// after a flood refusal: Telegram's limiter is per chat, and a bridge that
// keeps editing into it escalates the penalty. Hermes backs the interval off
// instead (stream_consumer_transport.py:586) — that is the upgrade path if
// this ever trips in practice.
func (d *Daemon) runPreview(ctx context.Context, chat int64, p *preview, stop <-chan struct{}, done <-chan struct{}) {
	every := d.streamEvery
	if every <= 0 {
		every = streamInterval
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-tick.C:
		}
		text, sent, id, flooded := p.state()
		if flooded {
			return
		}
		body := strings.TrimRight(text, "\n")
		if strings.TrimSpace(body) == "" {
			continue
		}
		if !sent {
			body = previewText(body, d.chunk)
			newID, err := d.tg.SendMessageID(ctx, chat, body)
			if err != nil {
				// Not fatal: the next tick retries, and the finished answer
				// is delivered at the end either way.
				d.logFn("gateway: preview for chat %d: %v", chat, err)
				continue
			}
			p.sentTo(newID)
			p.painted(body)
			continue
		}
		body = previewText(body, d.chunk)
		// Nothing new since the last paint: the tick and the wake can both
		// fire between two deltas, and an identical edit is a 400 from
		// Telegram ("message is not modified") that buys the phone nothing.
		if body == p.shownBody() {
			continue
		}
		if err := d.tg.EditMessageText(ctx, chat, id, body); err != nil {
			if isFlood(err) {
				p.markFlooded()
				d.logFn("gateway: preview for chat %d: %v; the answer will arrive when the turn ends", chat, err)
				return
			}
			d.logFn("gateway: preview edit for chat %d: %v", chat, err)
			continue
		}
		p.painted(body)
	}
}

// deliver puts the finished answer in the chat. When the preview created a
// message the answer is finished IN it: the head chunk is edited and any
// overflow is sent as follow-ups, so a long answer stays one conversation
// instead of a fresh bubble per 4096 characters. Without a preview — a turn
// that finished before the first paint, or one whose send never succeeded —
// it is the single reply it always was.
//
// A refused final edit is not a lost answer: the same text goes out as a new
// message (Hermes' _fallback_final_send). The chat then shows the partial
// preview above the full answer, which is worse than one clean message and
// much better than nothing.
func (d *Daemon) deliver(ctx context.Context, chat int64, p *preview, caption, answer string) error {
	body := strings.TrimRight(answer, "\n")
	switch {
	case caption == "":
	case strings.TrimSpace(body) == "":
		body = caption
	default:
		body = caption + "\n\n" + body
	}
	var sent bool
	var id int64
	var shown string
	if p != nil {
		_, sent, id, _ = p.state()
		shown = p.shownBody()
	}
	if !sent || strings.TrimSpace(body) == "" {
		return d.reply(ctx, chat, body)
	}
	parts := ChunkText(body, d.chunk)
	if len(parts) == 0 {
		return d.reply(ctx, chat, body)
	}
	// The preview already shows the whole answer: finish without a call.
	// Telegram answers an identical edit with 400 "message is not modified",
	// so this saves a round trip and a pointless refusal.
	if len(parts) == 1 && parts[0] == shown {
		return nil
	}
	if err := d.tg.EditMessageText(ctx, chat, id, parts[0]); err != nil {
		d.logFn("gateway: final edit for chat %d: %v; sending it as a new message", chat, err)
		return d.reply(ctx, chat, body)
	}
	for _, part := range parts[1:] {
		if err := d.tg.SendMessage(ctx, chat, part); err != nil {
			return err
		}
	}
	return nil
}

// previewText bounds an interim edit to Telegram's 4096 cap. A preview that
// outgrows the cap is CUT, not split: splitting would move the edit target to
// a new message id, and the next tick would re-edit the whole text into it —
// the duplication loop Hermes documents at adapter.py:3880. The finished
// answer is split properly (deliver), where there is no next tick.
func previewText(text string, limit int) string {
	if utf16Len(text) <= limit {
		return text
	}
	room := limit - utf16Len(streamTail)
	if room < 1 {
		return text[:utf16Cut(text, limit)]
	}
	cut := utf16Cut(text, room)
	if cut <= 0 {
		return text[:utf16Cut(text, limit)]
	}
	return strings.TrimRight(text[:cut], "\n") + streamTail
}

// isFlood reports Telegram's rate-limit refusal, which is the one preview
// failure that must stop the preview rather than be retried.
func isFlood(err error) bool {
	var api *APIError
	return errors.As(err, &api) &&
		(api.Code == http.StatusTooManyRequests || api.HTTPStatus == http.StatusTooManyRequests)
}
