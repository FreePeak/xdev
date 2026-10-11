// The headless event stream: Server-Sent Events, adopted from OpenCode's
// /api/event.
//
// The contract, and the reason it is SSE rather than a socket:
//
//   - one frame is `event: <type>\ndata: <json>\n\n`, so a client with a
//     plain HTTP reader (or `curl -N`) can consume it;
//   - the FIRST frame is `server.connected`, with no payload, so a client
//     knows the subscription is live before it sends a prompt (this is the
//     handshake OpenCode's EventHandler opens with);
//   - it is VOLATILE BY CONTRACT: a subscriber that does not keep up loses
//     frames and the stream FAILS, rather than growing a queue on the server.
//     An event stream feeding an unbounded per-client buffer is how a UI
//     stalls the agent it is watching — the failure xdev already paid for
//     once (#283). A client that reconnects re-reads state; the stream is
//     liveness, not the durable record.
//
// The bounded queue is deliberately small (see headlessSubCapacity): frames are
// deltas, and a client that falls 256 frames behind is not rendering them
// anyway.
package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// headlessSubCapacity bounds one subscriber's queue. Small on purpose: the
// drop-and-fail policy below is only safe when the buffer is too small to
// hide a stalled client.
const headlessSubCapacity = 256

// headlessHeartbeat is the comment cadence that keeps an idle stream alive
// through a proxy (OpenCode uses the same 15 s).
const headlessHeartbeat = 15 * time.Second

// headlessSub is one live /event subscriber.
type headlessSub struct {
	id   int64
	ch   chan []byte
	done chan struct{}
	once bool
}

// handleEvent opens the stream: headers first, then the connected frame, then
// whatever the session broadcasts until the client goes away.
func (h *Headless) handleEvent(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// SSE is not cacheable and must not be buffered by an intermediary; the
	// header set is the one every implementation converges on.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	frame := func(event string, payload any) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := frame("server.connected", map[string]any{}); err != nil {
		return
	}

	sub := h.subscribe()
	defer h.unsubscribe(sub)

	heartbeat := time.NewTicker(headlessHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.done:
			// Overflowed: the stream is failed rather than quietly thinned,
			// so a client can tell "I missed frames" from "nothing happened".
			_ = frame("server.overflow", map[string]any{"capacity": headlessSubCapacity})
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case payload := <-sub.ch:
			if _, err := w.Write(payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// subscribe registers one subscriber.
func (h *Headless) subscribe() *headlessSub {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	h.nextSub++
	sub := &headlessSub{
		id:   h.nextSub,
		ch:   make(chan []byte, headlessSubCapacity),
		done: make(chan struct{}),
	}
	h.subs[sub.id] = sub
	return sub
}

// unsubscribe removes a subscriber (idempotent).
func (h *Headless) unsubscribe(sub *headlessSub) {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	delete(h.subs, sub.id)
}

// Broadcast sends one frame to every subscriber. A subscriber whose queue is
// full is FAILED, not blocked: the server never waits on a client's render
// loop, which is the whole point of the bounded queue.
func (h *Headless) Broadcast(event string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// One allocation per broadcast, shared by every subscriber: a frame is
	// read-only once written.
	frame := make([]byte, 0, len(event)+len(body)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, body...)
	frame = append(frame, "\n\n"...)

	h.subMu.Lock()
	defer h.subMu.Unlock()
	for _, sub := range h.subs {
		select {
		case sub.ch <- frame:
		default:
			// Full: close the done channel once and let the handler fail the
			// stream. The entry stays until unsubscribe, so the failing
			// handler is the one that removes it.
			if !sub.once {
				sub.once = true
				close(sub.done)
			}
		}
	}
}

// Respond is the terminal (or error) answer to one prompt, correlated by id.
// It rides the same stream as the deltas so a client needs exactly one
// connection to drive a turn.
func (h *Headless) Respond(id string, resp any) {
	h.Broadcast("response", map[string]any{"id": id, "response": resp})
}

// closeSubscribers fails every live stream, so a shutting-down server does not
// leave clients waiting on a socket that will never speak again.
func (h *Headless) closeSubscribers() {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for _, sub := range h.subs {
		if !sub.once {
			sub.once = true
			close(sub.done)
		}
	}
}
