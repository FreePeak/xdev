package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// countMarkers walks a decoded request body and reports every cache_control it
// finds, in the three places markers can ride: the system block array, the tool
// array, and message content.
func countMarkers(t *testing.T, body []byte) int {
	t.Helper()
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	n := 0
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if _, ok := x["cache_control"]; ok {
				n++
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	return n
}

func TestAnthropicCacheBreakpoints(t *testing.T) {
	srv, body := newStreamServer(t, anthropicFrames...)
	p := NewAnthropicProvider("claude", srv.URL, "sk-test", nil, nil)
	msgs := []Message{
		{Role: RoleUser, Content: []Block{TextBlock{Text: "one"}}},
		{Role: RoleAssistant, Content: []Block{ToolCallBlock{ID: "t1", Name: "read", Arguments: json.RawMessage(`{}`)}}},
		{Role: RoleToolResult, ToolCallID: "t1", Content: []Block{TextBlock{Text: "result"}}},
		{Role: RoleUser, Content: []Block{TextBlock{Text: "two"}}},
	}
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model: "claude-sonnet", System: "be brief", Messages: msgs,
		Tools: []ToolDef{
			{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "bash", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		Cache: CacheOpts{Key: "sess_1"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collectEvents(t, ch)
	b := body.get(t)
	req := decodeJSON(t, b)

	// System rides as blocks so it can end the tools+system span.
	if jstr(t, req, "system", 0, "text") != "be brief" {
		t.Fatalf("system = %v", req["system"])
	}
	if jstr(t, req, "system", 0, "cache_control", "type") != "ephemeral" {
		t.Fatalf("system marker = %v", jpath(req, "system", 0, "cache_control"))
	}
	// The LAST tool carries the schema marker; earlier ones do not.
	if jpath(req, "tools", 0, "cache_control") != nil {
		t.Fatalf("tools[0] should be unmarked: %v", jpath(req, "tools", 0))
	}
	if jstr(t, req, "tools", 1, "cache_control", "type") != "ephemeral" {
		t.Fatalf("tools[1] marker = %v", jpath(req, "tools", 1, "cache_control"))
	}
	// Messages: the tool_result merge makes 4 wire messages (user, assistant,
	// user[tool_result], user); the tail gets the budget left after two markers.
	// Two of them is the whole point of the budget: never more than four total.
	if n := countMarkers(t, b); n > cacheMaxBreakpoints {
		t.Fatalf("markers = %d, want <= %d", n, cacheMaxBreakpoints)
	}
	// The newest turn is the marker that makes a growing conversation cheap.
	if jpath(req, "messages", 3, "content", 0, "cache_control") == nil {
		t.Fatalf("newest message unmarked: %v", jpath(req, "messages", 3))
	}
}

func TestAnthropicNoCacheIdentitySendsPreCacheShape(t *testing.T) {
	srv, body := newStreamServer(t, anthropicFrames...)
	p := NewAnthropicProvider("claude", srv.URL, "sk-test", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model: "claude-sonnet", System: "be brief",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
		Tools:    []ToolDef{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collectEvents(t, ch)
	b := body.get(t)
	if got := jstr(t, decodeJSON(t, b), "system"); got != "be brief" {
		t.Fatalf("system = %q, want the plain string", got)
	}
	if n := countMarkers(t, b); n != 0 {
		t.Fatalf("markers = %d, want 0 without a cache key", n)
	}
}

func TestAnthropicCacheRetentionTTL(t *testing.T) {
	t.Setenv("XDEV_CACHE_RETENTION", "long")
	srv, body := newStreamServer(t, anthropicFrames...)
	p := NewAnthropicProvider("claude", srv.URL, "sk-test", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model: "m", System: "s", Cache: CacheOpts{Key: "sess"},
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collectEvents(t, ch)
	if got := jstr(t, decodeJSON(t, body.get(t)), "system", 0, "cache_control", "ttl"); got != "1h" {
		t.Fatalf("ttl = %q, want 1h", got)
	}
}

func TestAnthropicCacheRetentionNone(t *testing.T) {
	t.Setenv("PI_CACHE_RETENTION", "none")
	srv, body := newStreamServer(t, anthropicFrames...)
	p := NewAnthropicProvider("claude", srv.URL, "sk-test", nil, nil)
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model: "m", System: "s", Cache: CacheOpts{Key: "sess"},
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collectEvents(t, ch)
	if n := countMarkers(t, body.get(t)); n != 0 {
		t.Fatalf("markers = %d, want 0 with retention none", n)
	}
}

// TestAnthropicCacheMarkerRejectedFallback covers the gateway case: a 400 that
// names cache_control must cost one turn, not the session.
func TestAnthropicCacheMarkerRejectedFallback(t *testing.T) {
	t.Cleanup(func() { cacheMarkersDisabled.Store(false) })
	cacheMarkersDisabled.Store(false)

	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"body.system[0].cache_control: unknown field"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range anthropicFrames {
			if f[0] != "" {
				w.Write([]byte("event: " + f[0] + "\n"))
			}
			w.Write([]byte("data: " + f[1] + "\n\n"))
		}
	}))
	t.Cleanup(srv.Close)

	p := NewAnthropicProvider("claude", srv.URL, "sk-test", nil, nil)
	req := StreamRequest{
		Model: "m", System: "s", Cache: CacheOpts{Key: "sess"},
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
	}
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := collectEvents(t, ch)
	if len(evs) == 0 || evs[len(evs)-1].Type != EventDone {
		t.Fatalf("events = %v, want a successful stream after the fallback", eventTypes(evs))
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want the rejected one plus the retry", len(bodies))
	}
	if n := countMarkers(t, bodies[0]); n == 0 {
		t.Fatal("first request sent no markers, so the 400 proves nothing")
	}
	if n := countMarkers(t, bodies[1]); n != 0 {
		t.Fatalf("retry markers = %d, want 0", n)
	}
	if got := jstr(t, decodeJSON(t, bodies[1]), "system"); got != "s" {
		t.Fatalf("retry system = %q, want the plain string", got)
	}
	// The latch is the point: a later turn must not re-probe the endpoint.
	ch, err = p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	for range ch {
	}
	if len(bodies) != 3 || countMarkers(t, bodies[2]) != 0 {
		t.Fatalf("post-latch request markers = %d (requests %d), want 0", countMarkers(t, bodies[2]), len(bodies))
	}
}

func TestCacheAffinityKey(t *testing.T) {
	t.Setenv("XDEV_CACHE_RETENTION", "")
	if got := cacheAffinityKey(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := cacheAffinityKey(" sess-42 "); got != "sess-42" {
		t.Fatalf("short = %q", got)
	}
	long := strings.Repeat("x", 65)
	got := cacheAffinityKey(long)
	if !strings.HasPrefix(got, "pc_") || len(got) > 64 {
		t.Fatalf("long = %q, want a pc_-prefixed key within 64 chars", got)
	}
	if got != cacheAffinityKey(long) {
		t.Fatal("hash is not stable")
	}
	if got == cacheAffinityKey(long+"y") {
		t.Fatal("distinct ids must not collide")
	}
}

func TestPromptCacheKeyHostGate(t *testing.T) {
	if got := promptCacheKey("https://api.openai.com/v1", "sess"); got != "sess" {
		t.Fatalf("first-party = %q", got)
	}
	for _, base := range []string{"https://onegw.example.com/v1", "http://127.0.0.1:11434/v1", "not a url"} {
		if got := promptCacheKey(base, "sess"); got != "" {
			t.Fatalf("%s = %q, want no key on a non-first-party host", base, got)
		}
	}
}

func TestCacheBreakpointBudget(t *testing.T) {
	msgs := make([]anthropicWireMessage, 0, 40)
	for i := 0; i < 40; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, anthropicWireMessage{Role: role, Content: []anthropicWireBlock{{Type: "text", Text: "m"}}})
	}
	applyAnthropicBreakpoints(msgs, cacheMaxBreakpoints)
	n := 0
	last := -1
	for i, m := range msgs {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				n++
				last = i
			}
		}
	}
	if n != cacheMaxBreakpoints {
		t.Fatalf("markers = %d, want %d", n, cacheMaxBreakpoints)
	}
	if last != len(msgs)-1 {
		t.Fatalf("newest message %d unmarked (last marker at %d)", len(msgs)-1, last)
	}
	// A deep marker must survive: the tail rewrites every turn, an early
	// deep one is the only reason a long session still reads something.
	if msgs[0].Content[0].CacheControl == nil && msgs[24].Content[0].CacheControl == nil {
		t.Fatal("no deep marker placed")
	}
}

func TestCacheSkipsThinkingOnlyMessage(t *testing.T) {
	msgs := []anthropicWireMessage{
		{Role: "assistant", Content: []anthropicWireBlock{{Type: "thinking", Thinking: "hmm"}}},
	}
	applyAnthropicBreakpoints(msgs, cacheMaxBreakpoints)
	if msgs[0].Content[0].CacheControl != nil {
		t.Fatal("a thinking block must not carry a marker: replaying an unsigned one is a 400")
	}
}

// TestOpenAIWiresPromptCacheKey pins the affinity field on both OpenAI wires at
// the body each one produces. The endpoint a test can dial is never
// api.openai.com, so the base URL is only a value here: buildRequest decides the
// shape, and that is the part that must not regress.
func TestOpenAIWiresPromptCacheKey(t *testing.T) {
	req := StreamRequest{Model: "gpt", System: "s", Cache: CacheOpts{Key: "sess-1"},
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}}}
	for _, tc := range []struct{ name, baseURL string }{
		{"first-party", "https://api.openai.com/v1"},
		{"proxy", "https://onegw.example.com/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewOpenAICompletionsProvider("oai", tc.baseURL, "k", nil, nil).buildRequest(req)
			if err != nil {
				t.Fatalf("completions buildRequest: %v", err)
			}
			r, err := NewOpenAIResponsesProvider("oai", tc.baseURL, "k", nil, nil).buildRequest(req)
			if err != nil {
				t.Fatalf("responses buildRequest: %v", err)
			}
			want := any("sess-1")
			if tc.name == "proxy" {
				want = nil // a proxy 400s on an unknown top-level field
			}
			for _, b := range [][]byte{c, r} {
				if got := decodeJSON(t, b)["prompt_cache_key"]; got != want {
					t.Fatalf("prompt_cache_key = %v, want %v", got, want)
				}
			}
		})
	}
}
