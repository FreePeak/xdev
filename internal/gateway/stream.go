package gateway

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Streaming the answer into the chat.
//
// Print mode already streams its deltas to stdout as the model produces them
// (cmd/xdev/print.go printHooks.OnEvent → fmt.Print), and the worker's stdout
// used to be a buffer that was read once the child exited. So the text existed
// live and was thrown away: the phone showed a typing bubble for the whole
// turn and then the finished answer.
//
// What this adds is the transport half: the worker's stdout is a pipe, the
// daemon reads it as it arrives, and the first bubble of the turn is edited in
// place as the text grows — the behaviour the user asked for ("the answer
// grows in the same message"), and the one Hermes' gateway has
// (plugins/platforms/telegram/adapter.py edit_message + gateway/stream_consumer.py,
// DEFAULT_STREAMING_EDIT_INTERVAL = 0.8s).
//
// Three rules make it safe to do against a real chat:
//
//  1. Bounded API traffic. One edit per editInterval, never per delta. Hermes
//     measured 0.8s as the rate that stops Telegram's per-chat flood control
//     refusing a preview edit; a delta-driven loop would send hundreds of
//     calls for one answer.
//  2. ONE bubble, and only for the head. The preview edits the first message
//     of the answer; the moment the text would outgrow Telegram's cap the head
//     is sealed (one send of its own) and the tail becomes the new preview.
//     That is Hermes' _split_first_send, and it is why a 10 KB answer still
//     streams instead of failing at 4096 UTF-16 units.
//  3. The final body is sent as a real message, not left as an edit. An edit
//     that fails after a 429 would leave the chat with a half-written answer
//     and no way to tell; the final send is the same path a non-streaming turn
//     uses (chunked, HTML, plain-text fallback), so the answer is never lost.

// editInterval is how often the preview bubble is refreshed. Hermes' default
// (gateway/config.py DEFAULT_STREAMING_EDIT_INTERVAL); an edit is counted
// against the same per-chat budget as a send, so this is the knob that keeps a
// long answer inside it.
const editInterval = 800 * time.Millisecond

// streamMinDelta is the smallest growth worth an API call. Below this the edit
// would cost a request to add a word or two — the visible cadence is
// editInterval either way, so the floor is what keeps a fast model from
// spending the chat's budget on edits nobody can see.
const streamMinDelta = 24

// streamHeadroom keeps the preview under Telegram's hard cap with room for the
// HTML tags ToTelegramHTML adds (a <pre><code class="language-x"> is ~40 bytes
// of markup on text that may be one byte), and for the escape pass that turns
// one character into six.
const streamHeadroom = 256

// maxStreamRunes bounds the pending text held in memory between edits. The head
// is sealed and dropped as the answer grows, so this only bounds ONE edit's
// worth of growth; past it the turn is a runaway and the tail is what a phone
// wants anyway.
const maxStreamRunes = 8192

// streamer turns a worker's live stdout into a growing Telegram message.
//
// It is owned by one turn: the daemon reads the worker's pipe on a goroutine,
// the streamer decides when an edit is due, and a ticker applies the pending
// text. Every method is safe to call from either goroutine (the reader writes,
// the ticker edits, finish joins them).
type streamer struct {
	tg   *Telegram
	chat int64
	ctx  context.Context
	logf func(format string, args ...any)

	// first is true until the answer's first bubble exists. Until then there
	// is nothing to edit, so the accumulated text is held and the send
	// happens on the first tick that has text (never on the first delta: a
	// bubble per token would be a message per token).
	mu      sync.Mutex
	pending string
	sent    string
	msgID   int64
	// sealed is the prefix of the answer a head seal already delivered; sent is
	// the body of the live preview bubble. Together they are what the chat has
	// seen, which is what previewed() reports and what the final reply strips
	// so the answer is not delivered twice. They are separate fields because a
	// seal RESETS the live bubble: the next edit compares against the new,
	// shorter text while the delivered prefix only ever grows.
	sealed string
	// failed is true once a permanent API refusal stopped the preview. The
	// turn is unaffected (the final send carries the rest), but nothing more
	// is written and no further call is attempted.
	failed bool
	// posted counts the API calls made, so a test can assert the cadence.
	posted int

	// done closes when the worker's output is complete; the ticker stops.
	done chan struct{}
	stop chan struct{}
	wg   sync.WaitGroup
}

// newStreamer starts the edit ticker. finish() must be called exactly once.
func newStreamer(ctx context.Context, tg *Telegram, chat int64, logf func(string, ...any)) *streamer {
	s := &streamer{
		tg:   tg,
		chat: chat,
		ctx:  ctx,
		logf: logf,
		done: make(chan struct{}),
		stop: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.loop()
	return s
}

// loop applies the pending text at the configured cadence. The first tick also
// posts the bubble, so a turn that produces nothing sends nothing (a turn with
// no answer must not leave an empty bubble behind — the same rule
// ChunkText("") follows).
func (s *streamer) loop() {
	defer s.wg.Done()
	tick := time.NewTicker(editInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.done:
			// One last chance to show the tail before the caller finalizes.
			s.flush()
			return
		case <-tick.C:
			s.flush()
		}
	}
}

// write accumulates one chunk of the worker's stdout. It never calls the API:
// the cadence belongs to the ticker, so a burst of deltas is one edit.
func (s *streamer) write(p []byte) {
	if len(p) == 0 {
		return
	}
	s.mu.Lock()
	if !s.failed {
		s.pending += string(p)
		if utf8.RuneCountInString(s.pending) > maxStreamRunes {
			// A runaway answer: keep the tail, which is what the phone shows.
			s.pending = tailRunes(s.pending, maxStreamRunes/2)
		}
	}
	s.mu.Unlock()
}

// flush posts the pending text: a new bubble when there is none yet, an edit
// afterwards. It is the only place that talks to the API.
//
// It LOOPS while the text is over the cap, because one seal per tick would make
// a 20 KB answer lag six ticks behind the model: each pass turns the live
// bubble into a complete head chunk and opens a fresh preview for the rest, so
// a burst converges in one tick. The loop is bounded by the text itself —
// every pass removes at least DefaultReplyChunk UTF-16 units.
func (s *streamer) flush() {
	for {
		s.mu.Lock()
		text := strings.TrimSpace(s.pending)
		if s.failed || text == "" || text == s.sent {
			s.mu.Unlock()
			return
		}
		id := s.msgID
		s.mu.Unlock()

		if utf16Len(text) > TelegramMessageLimit-streamHeadroom {
			parts := ChunkText(text, DefaultReplyChunk)
			if len(parts) < 2 {
				// Nothing to seal (a single unit-window chunk): post it as the
				// preview below and let the next flush split what grows on.
				break
			}
			head, rest := parts[0], strings.Join(parts[1:], "")
			if id != 0 {
				// The bubble on screen carries the answer's head: complete it.
				if err := s.tg.EditMessageText(s.ctx, s.chat, id, head); err != nil {
					s.seal("edit", err)
					return
				}
			} else if _, err := s.tg.SendMessageID(s.ctx, s.chat, head); err != nil {
				s.seal("send", err)
				return
			}
			s.mu.Lock()
			s.posted++
			// The head is delivered, so the final reply must not repeat it:
			// sealed is exactly the prefix of the answer the chat has seen.
			s.sealed += head
			s.msgID, s.sent, s.pending = 0, "", rest
			s.mu.Unlock()
			continue
		}

		grew := utf16Len(text) - utf16Len(s.sent)
		// A first bubble is always worth posting; an edit only when the text
		// grew enough to be worth a request.
		if id != 0 && grew < streamMinDelta {
			return
		}
		if id == 0 {
			newID, err := s.tg.SendMessageID(s.ctx, s.chat, text)
			if err != nil {
				s.seal("send", err)
				return
			}
			s.mu.Lock()
			s.msgID, s.sent, s.posted = newID, text, s.posted+1
			s.mu.Unlock()
			return
		}
		if err := s.tg.EditMessageText(s.ctx, s.chat, id, text); err != nil {
			s.seal("edit", err)
			return
		}
		s.mu.Lock()
		s.sent = text
		s.posted++
		s.mu.Unlock()
		return
	}
	// break above lands here: the pending text is a single over-cap chunk the
	// chunker cannot split. Post it once so the chat is not left empty; the
	// next flush (with more text) seals properly.
	s.mu.Lock()
	text := strings.TrimSpace(s.pending)
	id := s.msgID
	s.mu.Unlock()
	if text == "" {
		return
	}
	if id == 0 {
		if newID, err := s.tg.SendMessageID(s.ctx, s.chat, text); err == nil {
			s.mu.Lock()
			s.msgID, s.posted, s.sent = newID, s.posted+1, text
			s.mu.Unlock()
		}
	}
}

// seal stops the preview after a permanent failure. The turn is unaffected: the
// answer still arrives from the final send, which is the same path a turn that
// never streamed uses. A refusal is logged once, never retried per tick (a
// flood penalty retried every 800ms is how a bridge earns a longer one), and
// what the last successful edit showed stays in sealed so the final send does
// not repeat it.
func (s *streamer) seal(op string, err error) {
	s.mu.Lock()
	already := s.failed
	s.failed = true
	s.pending = ""
	s.mu.Unlock()
	if !already {
		s.logf("gateway: stream %s for chat %d stopped: %v", op, s.chat, err)
	}
}

// finish stops the ticker, applies whatever the last deltas left pending, and
// reports the text the preview left on screen so the caller sends only what the
// chat has not seen.
//
// The final flush is what makes the bubble the FINISHED body rather than the
// body as of the last tick: when the worker's last delta lands inside the tick
// window, the preview would otherwise stop one edit short of the answer and the
// chat would receive the last sentence twice (once as a new message). Here the
// whole remainder goes into the bubble, and the caller's final send is the
// leftover that did not fit — which rule 3 of the package comment is about.
func (s *streamer) finish() {
	close(s.done)
	s.wg.Wait()
	s.flush()
}

// previewed is what the streaming path already put in the chat ("" when it
// never posted). The final send strips exactly this much, so the answer is
// delivered once and whole.
func (s *streamer) previewed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed || s.msgID == 0 {
		return s.sealed
	}
	return s.sealed + s.sent
}

// postedCount is the number of API calls the preview made (tests).
func (s *streamer) postedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posted
}

// tailRunes keeps the last n runes of s.
func tailRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := len(s)
	for c := 0; c < n; c++ {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}
