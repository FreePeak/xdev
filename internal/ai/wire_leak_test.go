package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// leakClient disables keep-alives so idle httptest transport goroutines do
// not pollute the count: what remains are the adapter's own stream loops.
func leakClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

// TestAdaptersNoGoroutineLeaks pins the contract that every stream goroutine
// exits after the terminal event, on context cancel, and on hard transport
// failure, for each of the three wire adapters.
func TestAdaptersNoGoroutineLeaks(t *testing.T) {
	before := runtime.NumGoroutine()

	// 1. Happy path on all three adapters.
	happy := map[string][2]string{
		"anthropic":   {"message_stop", `{"type":"message_stop"}`},
		"completions": {"", `[DONE]`},
		"responses":   {"response.completed", `{"type":"response.completed","response":{"id":"r","status":"completed","usage":{}}}`},
	}
	var servers []*httptest.Server
	for name, frame := range happy {
		srv, _ := newStreamServer(t, frame)
		servers = append(servers, srv)
		switch name {
		case "anthropic":
			p := NewAnthropicProvider("p", srv.URL, "", nil, leakClient())
			ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
			if err != nil {
				t.Fatalf("%s: Stream: %v", name, err)
			}
			collectEvents(t, ch)
		case "completions":
			p := NewOpenAICompletionsProvider("p", srv.URL, "", nil, leakClient())
			ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
			if err != nil {
				t.Fatalf("%s: Stream: %v", name, err)
			}
			collectEvents(t, ch)
		case "responses":
			p := NewOpenAIResponsesProvider("p", srv.URL, "", nil, leakClient())
			ch, err := p.Stream(context.Background(), StreamRequest{Model: "m"})
			if err != nil {
				t.Fatalf("%s: Stream: %v", name, err)
			}
			collectEvents(t, ch)
		}
	}

	// 2. Context cancel mid-stream on a handler that would otherwise hang.
	block := make(chan struct{})
	hsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-block
	}))
	servers = append(servers, hsrv)
	for name, prov := range map[string]Provider{
		"anthropic":   NewAnthropicProvider("p", hsrv.URL, "", nil, leakClient()),
		"completions": NewOpenAICompletionsProvider("p", hsrv.URL, "", nil, leakClient()),
		"responses":   NewOpenAIResponsesProvider("p", hsrv.URL, "", nil, leakClient()),
	} {
		ctx, cancel := context.WithCancel(context.Background())
		c, err := prov.Stream(ctx, StreamRequest{Model: "m"})
		if err != nil {
			t.Fatalf("%s: Stream: %v", name, err)
		}
		cancel()
		Drain(c)
	}

	// 3. Server closes the connection abruptly (transport error mid-frame).
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\nevent: message_start\r\n")
		conn.Close()
	}))
	servers = append(servers, esrv)
	for name, prov := range map[string]Provider{
		"anthropic":   NewAnthropicProvider("p", esrv.URL, "", nil, leakClient()),
		"completions": NewOpenAICompletionsProvider("p", esrv.URL, "", nil, leakClient()),
		"responses":   NewOpenAIResponsesProvider("p", esrv.URL, "", nil, leakClient()),
	} {
		c, err := prov.Stream(context.Background(), StreamRequest{Model: "m"})
		if err != nil {
			t.Fatalf("%s: Stream: %v", name, err)
		}
		Drain(c)
	}

	// Tear down every server (including the hanging handler) before
	// counting: t.Cleanup would otherwise run after the count.
	close(block)
	for _, s := range servers {
		s.CloseClientConnections()
		s.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutine leak: before = %d, after = %d\n%s", before, after, buf[:n])
	}
}
