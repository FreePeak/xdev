package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The effort LEVEL reaches the provider as itself.
//
// xdev's ladder has five real rungs above "off" (low, medium, high, xhigh,
// max), and the wire used to collapse them into three token-derived tiers:
// every budget above 8,192 became "high", so "xhigh" and "max" reached
// OpenAI as the same string the user had already paid for "high".
//
// A model that DECLARES the capability (the model registry's `effort:`)
// now gets the rung the user asked for. A model that does not declare it
// gets exactly the request it got before the level existed — byte for byte,
// because a provider that rejects an effort parameter it never agreed to
// accept is a failed turn.

// effortRequest is one request as it went out: the body bytes and the
// headers the adapter sent.
type effortRequest struct {
	body []byte
	head http.Header
}

// newEffortServer answers every request with an empty event stream and
// keeps the request it was given, so a test can assert on the exact bytes
// and the exact headers.
func newEffortServer(t *testing.T) (*httptest.Server, chan effortRequest) {
	t.Helper()
	got := make(chan effortRequest, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		got <- effortRequest{body: buf, head: r.Header.Clone()}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// captureEffort streams one request through the provider and returns what it
// sent. The answer is deliberately empty: the assertion is about the request.
func captureEffort(t *testing.T, p Provider, req StreamRequest, got chan effortRequest) effortRequest {
	t.Helper()
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch { // drain: the provider closes the channel when it is done
	}
	select {
	case r := <-got:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no request captured")
	}
	return effortRequest{}
}

// effortJSON decodes a request body into a generic map.
func effortJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	return m
}

// effortTurn is the smallest conversation every wire accepts.
func effortTurn() StreamRequest {
	return StreamRequest{
		Model:    "gpt-6",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
	}
}

// The OpenAI responses wire carries the level the user asked for.
func TestOpenAIResponsesSendsTheEffortLevel(t *testing.T) {
	srv, got := newEffortServer(t)
	p := NewOpenAIResponsesProvider("openai", srv.URL, "sk-test", nil, nil)
	req := effortTurn()
	req.Thinking = &ThinkingBudget{
		Tokens:    32768,
		Level:     "xhigh",
		Supported: []string{"low", "medium", "high", "xhigh", "max"},
	}
	body := effortJSON(t, captureEffort(t, p, req, got).body)
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "xhigh" {
		t.Fatalf("reasoning = %v, want effort xhigh", body["reasoning"])
	}
}

// The chat/completions wire carries it too — it is the wire a gateway
// speaks, and it is the one that had the three-bucket fold.
func TestOpenAICompletionsSendsTheEffortLevel(t *testing.T) {
	srv, got := newEffortServer(t)
	p := NewOpenAICompletionsProvider("gw", srv.URL, "sk-test", nil, nil)
	req := effortTurn()
	req.Thinking = &ThinkingBudget{
		Tokens:    49152,
		Level:     "max",
		Supported: []string{"low", "medium", "high", "xhigh", "max"},
	}
	body := effortJSON(t, captureEffort(t, p, req, got).body)
	if body["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %v, want max", body["reasoning_effort"])
	}
}

// Anthropic gets output_config.effort AND the beta header that goes with
// it (omp sends the two together — anthropic.ts:55, :3281-3290).
//
// And the budget rides along with it, on purpose. omp's Anthropic body
// carries `thinking` and `output_config` in the SAME request
// (anthropic.ts:3312 and :3320), and the thinking block that goes with an
// effort is the budget one — the `{type:"enabled", budget_tokens}` shape is
// the model mode omp calls "anthropic-budget-effort" (anthropic.ts:3216-3226:
// the effort is set right after the budget block, under exactly that mode).
// The two are therefore not mutually exclusive: a model in that mode takes
// both, and a model in no mode takes neither. xdev's Anthropic wire has
// always carried the budget, so xdev's both-params request IS omp's
// budget-effort shape.
func TestAnthropicSendsOutputConfigEffortAndBeta(t *testing.T) {
	srv, got := newEffortServer(t)
	p := NewAnthropicProvider("anthropic", srv.URL, "sk-test", nil, nil)
	req := effortTurn()
	req.Model = "claude-opus-5"
	req.Thinking = &ThinkingBudget{
		Tokens:    16384,
		Level:     "high",
		Supported: []string{"low", "medium", "high"},
	}
	sent := captureEffort(t, p, req, got)
	body := effortJSON(t, sent.body)
	out, _ := body["output_config"].(map[string]any)
	if out == nil || out["effort"] != "high" {
		t.Fatalf("output_config = %v, want effort high", body["output_config"])
	}
	if beta := sent.head.Get("anthropic-beta"); beta != anthropicEffortBeta {
		t.Fatalf("anthropic-beta = %q, want %q", beta, anthropicEffortBeta)
	}
	// The budget still rides along: the level is an addition, not a swap.
	thinking, _ := body["thinking"].(map[string]any)
	if thinking == nil || thinking["budget_tokens"] != float64(16384) {
		t.Fatalf("thinking = %v, want budget_tokens 16384", body["thinking"])
	}
}

// Gemini names the rung itself: thinkingLevel, and the budget only when the
// model does not take a level (omp sends one or the other — google-shared.ts:554-563).
func TestGoogleSendsTheThinkingLevel(t *testing.T) {
	srv, got := newEffortServer(t)
	p := NewGoogleGenAIProvider("google", srv.URL, "key", nil, nil)
	req := effortTurn()
	req.Model = "gemini-3.8-flash"
	req.Thinking = &ThinkingBudget{
		Tokens:    8192,
		Level:     "medium",
		Supported: []string{"low", "medium", "high"},
	}
	body := effortJSON(t, captureEffort(t, p, req, got).body)
	gen, _ := body["generationConfig"].(map[string]any)
	thinking, _ := gen["thinkingConfig"].(map[string]any)
	if thinking == nil || thinking["thinkingLevel"] != "medium" {
		t.Fatalf("thinkingConfig = %v, want thinkingLevel medium", gen["thinkingConfig"])
	}
	if _, ok := thinking["thinkingBudget"]; ok {
		t.Fatalf("thinkingConfig carries a budget next to the level: %v", thinking)
	}
}

// A model that does not declare the capability gets the request it always
// got — byte for byte. This is the promise the default makes: the effort
// level is an addition, never a change to what an undeclared model sees.
func TestModelWithoutTheEffortCapabilityIsByteIdentical(t *testing.T) {
	srv, got := newEffortServer(t)

	t.Run("openai-responses", func(t *testing.T) {
		p := NewOpenAIResponsesProvider("openai", srv.URL, "sk-test", nil, nil)
		before := effortTurn()
		before.Thinking = &ThinkingBudget{Tokens: 32768}
		after := effortTurn()
		// The level is set, but the model declares nothing: the answer is
		// the same request as before.
		after.Thinking = &ThinkingBudget{Tokens: 32768, Level: "xhigh"}
		a := captureEffort(t, p, before, got)
		b := captureEffort(t, p, after, got)
		if !bytes.Equal(a.body, b.body) {
			t.Fatalf("body changed for an undeclared model:\n%s\n%s", a.body, b.body)
		}
		body := effortJSON(t, a.body)
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning == nil || reasoning["effort"] != "high" {
			t.Fatalf("reasoning = %v, want the token-derived tier high", body["reasoning"])
		}
	})

	t.Run("openai-completions", func(t *testing.T) {
		p := NewOpenAICompletionsProvider("gw", srv.URL, "sk-test", nil, nil)
		before := effortTurn()
		before.Thinking = &ThinkingBudget{Tokens: 32768}
		after := effortTurn()
		after.Thinking = &ThinkingBudget{Tokens: 32768, Level: "xhigh"}
		a := captureEffort(t, p, before, got)
		b := captureEffort(t, p, after, got)
		if !bytes.Equal(a.body, b.body) {
			t.Fatalf("body changed for an undeclared model:\n%s\n%s", a.body, b.body)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		p := NewAnthropicProvider("anthropic", srv.URL, "sk-test", nil, nil)
		before := effortTurn()
		before.Model = "claude-opus-5"
		before.Thinking = &ThinkingBudget{Tokens: 16384}
		after := effortTurn()
		after.Model = "claude-opus-5"
		after.Thinking = &ThinkingBudget{Tokens: 16384, Level: "high"}
		a := captureEffort(t, p, before, got)
		b := captureEffort(t, p, after, got)
		if !bytes.Equal(a.body, b.body) {
			t.Fatalf("body changed for an undeclared model:\n%s\n%s", a.body, b.body)
		}
		if _, ok := effortJSON(t, a.body)["output_config"]; ok {
			t.Fatalf("output_config rode along for an undeclared model: %s", a.body)
		}
		if beta := a.head.Get("anthropic-beta"); beta != "" {
			t.Fatalf("anthropic-beta = %q, want no header for an undeclared model", beta)
		}
	})

	t.Run("google", func(t *testing.T) {
		p := NewGoogleGenAIProvider("google", srv.URL, "key", nil, nil)
		before := effortTurn()
		before.Model = "gemini-3.8-flash"
		before.Thinking = &ThinkingBudget{Tokens: 8192}
		after := effortTurn()
		after.Model = "gemini-3.8-flash"
		after.Thinking = &ThinkingBudget{Tokens: 8192, Level: "medium"}
		a := captureEffort(t, p, before, got)
		b := captureEffort(t, p, after, got)
		if !bytes.Equal(a.body, b.body) {
			t.Fatalf("body changed for an undeclared model:\n%s\n%s", a.body, b.body)
		}
	})
}

// A level the model does not accept is DROPPED, never folded onto a
// different one: silently sending "high" for a request that said "max" is a
// lie about what was asked.
//
// Each provider is pinned by BOTH halves: the same model, the same request,
// one level it accepts and one it does not. The accepted level must go out
// and the rejected one must not — a wire that sent neither (the behaviour
// before the effort level existed) fails the first half.
func TestUnsupportedEffortLevelIsDropped(t *testing.T) {
	srv, got := newEffortServer(t)

	t.Run("openai-responses", func(t *testing.T) {
		p := NewOpenAIResponsesProvider("openai", srv.URL, "sk-test", nil, nil)
		accepted := effortTurn()
		accepted.Thinking = &ThinkingBudget{
			Tokens:    32768,
			Level:     "high",
			Supported: []string{"low", "medium", "high"},
		}
		rejected := effortTurn()
		rejected.Thinking = &ThinkingBudget{
			Tokens:    32768,
			Level:     "xhigh",
			Supported: []string{"low", "medium", "high"},
		}
		a := effortJSON(t, captureEffort(t, p, accepted, got).body)
		reasoning, _ := a["reasoning"].(map[string]any)
		if reasoning == nil || reasoning["effort"] != "high" {
			t.Fatalf("the accepted level must ride along: reasoning = %v", a["reasoning"])
		}
		b := effortJSON(t, captureEffort(t, p, rejected, got).body)
		if _, ok := b["reasoning"]; ok {
			t.Fatalf("reasoning rode along for an unsupported level: %v", b["reasoning"])
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		p := NewAnthropicProvider("anthropic", srv.URL, "sk-test", nil, nil)
		accepted := effortTurn()
		accepted.Model = "claude-opus-5"
		accepted.Thinking = &ThinkingBudget{
			Tokens:    16384,
			Level:     "high",
			Supported: []string{"low", "medium", "high"},
		}
		rejected := effortTurn()
		rejected.Model = "claude-opus-5"
		rejected.Thinking = &ThinkingBudget{
			Tokens:    16384,
			Level:     "xhigh",
			Supported: []string{"low", "medium", "high"},
		}
		a := captureEffort(t, p, accepted, got)
		out, _ := effortJSON(t, a.body)["output_config"].(map[string]any)
		if out == nil || out["effort"] != "high" {
			t.Fatalf("the accepted level must ride along: output_config = %v", out)
		}
		if beta := a.head.Get("anthropic-beta"); beta != anthropicEffortBeta {
			t.Fatalf("anthropic-beta = %q, want %q", beta, anthropicEffortBeta)
		}
		b := captureEffort(t, p, rejected, got)
		if _, ok := effortJSON(t, b.body)["output_config"]; ok {
			t.Fatalf("output_config rode along for an unsupported level")
		}
		if beta := b.head.Get("anthropic-beta"); beta != "" {
			t.Fatalf("anthropic-beta = %q, want no header for an unsupported level", beta)
		}
	})

	t.Run("google", func(t *testing.T) {
		p := NewGoogleGenAIProvider("google", srv.URL, "key", nil, nil)
		accepted := effortTurn()
		accepted.Model = "gemini-3.8-flash"
		accepted.Thinking = &ThinkingBudget{
			Tokens:    8192,
			Level:     "high",
			Supported: []string{"low", "medium", "high"},
		}
		rejected := effortTurn()
		rejected.Model = "gemini-3.8-flash"
		rejected.Thinking = &ThinkingBudget{
			Tokens:    8192,
			Level:     "xhigh",
			Supported: []string{"low", "medium", "high"},
		}
		a := effortJSON(t, captureEffort(t, p, accepted, got).body)
		gen, _ := a["generationConfig"].(map[string]any)
		thinking, _ := gen["thinkingConfig"].(map[string]any)
		if thinking == nil || thinking["thinkingLevel"] != "high" {
			t.Fatalf("the accepted level must ride along: thinkingConfig = %v", gen["thinkingConfig"])
		}
		b := effortJSON(t, captureEffort(t, p, rejected, got).body)
		gen, _ = b["generationConfig"].(map[string]any)
		thinking, _ = gen["thinkingConfig"].(map[string]any)
		if thinking == nil || thinking["thinkingBudget"] != float64(8192) {
			t.Fatalf("an unsupported level falls back to the budget: thinkingConfig = %v", gen["thinkingConfig"])
		}
		if _, ok := thinking["thinkingLevel"]; ok {
			t.Fatalf("the unsupported level rode along: thinkingConfig = %v", thinking)
		}
	})
}
