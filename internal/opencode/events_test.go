package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The event stream is the only surface that cannot go through the CLI, so it
// is the only one that speaks HTTP directly — and therefore the only one that
// touches the password. Everything below is against httptest, never a live
// server.

func TestEventStreamParsesBareDataLines(t *testing.T) {
	body := ": heartbeat\n\n" +
		`data: {"id":"evt_1","type":"server.connected","data":{}}` + "\n\n" +
		`data: {"id":"evt_2","created":1792311887244,"type":"session.usage.updated",` +
		`"data":{"sessionID":"ses_a","cost":8.68e-05,` +
		`"tokens":{"input":10,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}}` + "\n\n"

	evs, err := parseStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (heartbeat is a comment, not an event)", len(evs))
	}
	if evs[0].Type != "server.connected" || evs[0].ID != "evt_1" {
		t.Errorf("ev0 = %+v", evs[0])
	}
	if evs[1].Type != "session.usage.updated" {
		t.Errorf("ev1 type = %q", evs[1].Type)
	}
	if evs[1].SessionID() != "ses_a" {
		t.Errorf("SessionID = %q, want ses_a", evs[1].SessionID())
	}
	if evs[1].Cost() != 8.68e-05 {
		t.Errorf("cost = %v", evs[1].Cost())
	}
	tk := evs[1].Tokens()
	if tk.Input != 10 || tk.Output != 2 {
		t.Errorf("tokens = %+v", tk)
	}
}

// Deltas carry no durable block, which is exactly why a reconnect cannot
// replay them. Surface that rather than hiding it behind a zero value.
func TestEventDurableIsOptional(t *testing.T) {
	body := `data: {"id":"evt_1","type":"session.reasoning.delta","data":{"sessionID":"ses_a","delta":"hi"}}` + "\n" +
		`data: {"id":"evt_2","type":"session.tool.called","data":{"sessionID":"ses_a","id":"c1"},` +
		`"durable":{"aggregateID":"ses_a","seq":7,"version":1}}` + "\n"

	evs, err := parseStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if evs[0].Durable != nil {
		t.Errorf("delta should have no durable block, got %+v", evs[0].Durable)
	}
	if evs[1].Durable == nil || evs[1].Durable.Seq != 7 {
		t.Fatalf("durable = %+v, want seq 7", evs[1].Durable)
	}
}

// A malformed frame must not take the stream down, and must not be silently
// swallowed either — it is counted and surfaced.
func TestEventStreamCountsUndecodableFrames(t *testing.T) {
	body := "data: {not json}\n\n" +
		`data: {"id":"evt_1","type":"session.created","data":{}}` + "\n"

	evs, bad, err := parseStreamCounting(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseStreamCounting: %v", err)
	}
	if bad != 1 {
		t.Errorf("undecodable = %d, want 1", bad)
	}
	if len(evs) != 1 {
		t.Errorf("decoded %d, want the one good frame", len(evs))
	}
}

func TestEventStreamSkipsCommentsAndBlanks(t *testing.T) {
	body := "\n: heartbeat\n\n\n: another\n" +
		`data: {"id":"e","type":"server.connected","data":{}}` + "\n: trailing\n\n"
	evs, bad, err := parseStreamCounting(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseStreamCounting: %v", err)
	}
	if len(evs) != 1 || bad != 0 {
		t.Fatalf("events=%d bad=%d, want 1/0", len(evs), bad)
	}
}

// --- the HTTP stream -------------------------------------------------------

func TestStreamSubscribesWithBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"evt_1","type":"server.connected","data":{}}`+"\n")
	}))
	defer srv.Close()

	s := &Stream{Endpoint: Endpoint{URL: srv.URL, Password: "s3cr3t"}}
	// Run blocks for the life of the stream by design, so ending on the
	// context deadline is the success path here, not a failure.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := s.Run(ctx, func(Event) {})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
	if gotUser != "opencode" {
		t.Errorf("basic user = %q, want opencode", gotUser)
	}
	if gotPass != "s3cr3t" {
		t.Errorf("basic pass = %q", gotPass)
	}
	if gotPath != "/api/event" {
		t.Errorf("path = %q, want /api/event", gotPath)
	}
}

// F2: a stream that drops must reconnect, and the listener must not see the
// same durable event twice.
func TestStreamReconnectsAfterDrop(t *testing.T) {
	var mu sync.Mutex
	var hits int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		// Every connection replays evt_1, which is what a resume does.
		fmt.Fprint(w, `data: {"id":"evt_1","type":"session.created","data":{"sessionID":"ses_a"},`+
			`"durable":{"aggregateID":"ses_a","seq":1,"version":1}}`+"\n")
		if n == 1 {
			// First connection: one extra event, then hang up.
			fmt.Fprint(w, `data: {"id":"evt_2","type":"session.tool.called","data":{"sessionID":"ses_a","id":"c1"},`+
				`"durable":{"aggregateID":"ses_a","seq":2,"version":1}}`+"\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	s := &Stream{
		Endpoint:     Endpoint{URL: srv.URL, Password: "pw"},
		MinReconnect: time.Millisecond,
		MaxReconnect: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var mu2 sync.Mutex
	seen := map[string]int{}
	err := s.Run(ctx, func(e Event) {
		mu2.Lock()
		defer mu2.Unlock()
		if e.Durable != nil {
			seen[e.Durable.AggregateID+"/"+fmt.Sprint(e.Durable.Seq)]++
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
	mu2.Lock()
	defer mu2.Unlock()
	if seen["ses_a/1"] != 1 {
		t.Errorf("evt seq 1 delivered %d times, want exactly 1 (replayed on reconnect)", seen["ses_a/1"])
	}
	if seen["ses_a/2"] < 1 {
		t.Errorf("evt seq 2 never delivered")
	}
	mu.Lock()
	if hits < 2 {
		t.Errorf("server hits = %d, want at least 2 (a reconnect happened)", hits)
	}
	mu.Unlock()
}

func TestStreamRejectsNonLoopbackEndpoint(t *testing.T) {
	s := &Stream{Endpoint: Endpoint{URL: "http://example.com:4096", Password: "pw"}}
	err := s.Run(context.Background(), func(Event) {})
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, want ErrNoServer", err)
	}
}

func TestStreamRejectsEmptyPassword(t *testing.T) {
	s := &Stream{Endpoint: Endpoint{URL: "http://127.0.0.1:4096"}}
	err := s.Run(context.Background(), func(Event) {})
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, want ErrNoServer", err)
	}
}
