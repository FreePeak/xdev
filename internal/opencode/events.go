package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Event is one frame off opencode's /api/event stream.
//
// The shape here was captured from the live stream, not from the OpenAPI
// declaration: the spec says the envelope is {id, event, data} with data as a
// JSON-encoded string, and none of that is true. The real frame has `type`
// (not `event`), an object `data`, and additionally carries `created`,
// `location` and `durable`. See docs/research/opencode-v2-spec.md §3.
type Event struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Created  int64           `json:"created"`
	Location EventLocation   `json:"location"`
	Data     json.RawMessage `json:"data"`
	// Durable is present on replayable events and absent on ephemeral ones.
	// Every *.delta is ephemeral, which is why a reconnect cannot be built by
	// replaying the stream (F2): the deltas that would fill the gap are gone,
	// and the caller reconciles from session.message.list instead.
	Durable *DurableRef `json:"durable,omitempty"`
}

// EventLocation is the project an event belongs to. One stream serves every
// working directory, so this is how a listener routes.
type EventLocation struct {
	Directory string `json:"directory"`
}

// DurableRef is opencode's replay coordinate: per-aggregate monotonic seq,
// with version for idempotency.
type DurableRef struct {
	AggregateID string `json:"aggregateID"`
	Seq         int64  `json:"seq"`
	Version     int64  `json:"version"`
}

// sessionPayload is the subset every session event carries. opencode sends
// sessionID on the event data rather than the envelope, so it is read from
// there.
type sessionPayload struct {
	SessionID string  `json:"sessionID"`
	Cost      float64 `json:"cost"`
	Tokens    struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// SessionID returns the session an event belongs to, or "" for events that
// are not about one (server.connected, project.updated).
func (e Event) SessionID() string {
	if len(e.Data) == 0 {
		return ""
	}
	var p sessionPayload
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return ""
	}
	return p.SessionID
}

// Cost and Tokens expose session.usage.updated's roll-up, which matches
// Session.Info exactly and is what a status row needs to stay live.
func (e Event) Cost() float64 { return e.payload().Cost }

func (e Event) Tokens() TokenUsage {
	t := e.payload().Tokens
	return TokenUsage{
		Input:     t.Input,
		Output:    t.Output,
		Reasoning: t.Reasoning,
		Cache:     CacheUsage{Read: t.Cache.Read, Write: t.Cache.Write},
	}
}

func (e Event) payload() sessionPayload {
	var p sessionPayload
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &p)
	}
	return p
}

// parseStream reads every complete frame and fails on a truncated tail.
func parseStream(r io.Reader) ([]Event, error) {
	evs, _, err := parseStreamCounting(r)
	return evs, err
}

// parseStreamCounting reads frames and reports how many could not be decoded.
// A malformed frame is counted rather than fatal: the stream is the only way
// to see live output, and one bad frame should cost one event, not the
// connection. The count is what lets a caller notice a systematic mismatch
// instead of quietly rendering less.
func parseStreamCounting(r io.Reader) ([]Event, int, error) {
	var out []Event
	bad := 0
	sc := bufio.NewScanner(r)
	// opencode's frames carry whole tool inputs and outputs; the default 64K
	// scanner cap truncates a large tool result mid-JSON and turns a good
	// frame into a decode failure.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // blank separator, or an SSE comment (": heartbeat")
		}
		if !strings.HasPrefix(line, "data:") {
			continue // `event:` / `id:` / `retry:` fields opencode does not send
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var e Event
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			bad++
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return out, bad, err
	}
	return out, bad, nil
}

// Stream subscribes to opencode's event feed.
//
// One Stream serves every session: opencode publishes a single global stream
// and each frame's location.directory and sessionID route it. N tabs on one
// connection, not N connections.
type Stream struct {
	Endpoint Endpoint

	// MinReconnect and MaxReconnect bound the backoff after a drop.
	// Zero values fall back to 250ms..5s.
	MinReconnect time.Duration
	MaxReconnect time.Duration

	// HTTPClient is nil for a default client. Injected by tests.
	HTTPClient *http.Client

	// Undecodable counts frames that arrived but did not parse.
	Undecodable int

	mu      sync.Mutex
	seenSeq map[string]int64
}

// Run subscribes and blocks, calling fn for each event, until ctx is done.
//
// Reconnect is automatic and the durable watermark is what makes it safe: on
// reconnect opencode replays from the last acknowledged point, so this side
// suppresses a durable event it has already delivered. That is why seq is
// tracked per aggregate rather than per connection (F2).
//
// Transient subscription errors are retried; a non-loopback endpoint or a
// missing password is not, because retrying those would spin forever against
// a configuration that will not change on its own.
func (s *Stream) Run(ctx context.Context, fn func(Event)) error {
	if !isLoopbackHTTP(s.Endpoint.URL) || s.Endpoint.Password == "" {
		return fmt.Errorf("%w: event stream needs a loopback url and a password (got %q)",
			ErrNoServer, s.Endpoint.URL)
	}
	if fn == nil {
		fn = func(Event) {}
	}
	s.mu.Lock()
	if s.seenSeq == nil {
		s.seenSeq = map[string]int64{}
	}
	s.mu.Unlock()

	min, max := s.MinReconnect, s.MaxReconnect
	if min <= 0 {
		min = 250 * time.Millisecond
	}
	if max <= 0 {
		max = 5 * time.Second
	}
	backoff := min

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.subscribe(ctx, fn)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			// A clean end of stream is still a drop for our purposes.
			err = errors.New("stream closed")
		}
		// A 401 is not transient: the password is wrong or the server
		// restarted with a new one. Surfacing it beats looping.
		if errors.Is(err, errUnauthorized) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > max {
			backoff = max
		}
	}
}

// errUnauthorized marks a 401 from the stream so Run can stop instead of
// retrying a credential that will not fix itself.
var errUnauthorized = errors.New("opencode: stream rejected credentials")

// subscribe runs one connection to completion.
func (s *Stream) subscribe(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(s.Endpoint.URL, "/")+"/api/event", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", s.Endpoint.Password)
	req.Header.Set("Accept", "text/event-stream")

	client := s.HTTPClient
	if client == nil {
		// No timeout: this request is supposed to stay open indefinitely.
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", errUnauthorized, resp.Status)
	case http.StatusOK:
	default:
		return fmt.Errorf("opencode: stream status %s", resp.Status)
	}

	// The declared content type is text/event-stream; some builds omit it.
	// A web-UI HTML shell here would mean the path is wrong, and reading it
	// as a stream would silently produce zero events forever.
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "event-stream") {
		return fmt.Errorf("%w: stream returned %q, not an event stream", ErrNotJSON, ct)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &e); err != nil {
			s.mu.Lock()
			s.Undecodable++
			s.mu.Unlock()
			continue
		}
		if !s.newDurable(e) {
			continue // already delivered on a previous connection
		}
		fn(e)
	}
	return sc.Err()
}

// newDurable reports whether an event should be delivered, suppressing a
// durable event whose seq was already seen for that aggregate.
//
// Only durable events are watermarked. An ephemeral delta carries no seq, so
// there is nothing to compare and it always passes — which is correct, and
// also why a reconnect cannot be made lossless here.
func (s *Stream) newDurable(e Event) bool {
	if e.Durable == nil {
		return true
	}
	key := e.Durable.AggregateID
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.seenSeq[key]; ok && e.Durable.Seq <= prev {
		return false
	}
	s.seenSeq[key] = e.Durable.Seq
	return true
}
