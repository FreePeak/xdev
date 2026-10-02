package ai

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// The header phase of a streaming POST was unbounded for any provider
// configured through buildProvider: the client it constructs carries no
// ResponseHeaderTimeout, and the 90s stream watchdog only starts once
// wirePost has already returned. A gateway that accepts the connection and
// then says nothing therefore held a run open forever — reproduced live on
// 2026-09-29 (killed at 210s and again at 195s, zero records written).
//
// The test drives the same shape as a real run: a raw listener that reads
// the request and never answers, and the EXACT client buildProvider builds
// (cmd/xdev/print.go) rather than the shared transport, because the bug is
// that one client's absence of a bound.
func TestWirePostHeaderPhaseIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				time.Sleep(10 * time.Minute) // accepted, and then silent
			}(c)
		}
	}()

	// buildProvider's client, verbatim: no ResponseHeaderTimeout.
	hc := &http.Client{Transport: &http.Transport{
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}}
	// Shrink the budget so the test does not spend 90s. The production value
	// is FirstProgressTimeout; the wiring under test is the WithTimeout
	// itself, and a var keeps both the shrink and the assertion honest.
	orig := FirstProgressTimeout
	FirstProgressTimeout = 300 * time.Millisecond
	t.Cleanup(func() { FirstProgressTimeout = orig })

	start := time.Now()
	_, err = wirePost(context.Background(), hc, "http://"+ln.Addr().String()+"/v1/chat/completions",
		map[string]string{"Authorization": "Bearer k"}, []byte(`{"model":"m"}`), "test")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a host that never sends response headers must produce an error, not a hang")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("wirePost took %s: the header phase is unbounded", elapsed.Round(time.Millisecond))
	}
	t.Logf("bounded after %s: %v", elapsed.Round(time.Millisecond), err)
}

// The budget must bound the HEADER phase only. A healthy stream that answers
// and then trickles for longer than FirstProgressTimeout must survive: the
// 90s watchdog above the body is the layer meant to judge that, and a
// header deadline that leaked into the body would kill every slow-but-
// healthy model. (Every openai_* happy-path test failed with
// `context canceled` when cancelHeader was deferred here — this is the
// test that says why that is wrong.)
func TestWirePostHeaderBudgetDoesNotCapTheBody(t *testing.T) {
	orig := FirstProgressTimeout
	FirstProgressTimeout = 200 * time.Millisecond
	t.Cleanup(func() { FirstProgressTimeout = orig })

	srv, _ := newStreamServer(t,
		[2]string{"", `{"choices":[{"delta":{"content":"he"}}]}`},
		[2]string{"", `{"choices":[{"delta":{"content":"llo"}}]}`},
		[2]string{"", "[DONE]"},
	)
	p := NewOpenAICompletionsProvider("t", srv.URL, "k", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := p.Stream(ctx, StreamRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	var text string
	for _, ev := range evs {
		if ev.Type == EventTextDelta {
			text += ev.Delta
		}
	}
	if text != "hello" {
		t.Fatalf("stream text = %q, want %q: the header budget leaked into the body", text, "hello")
	}
}
