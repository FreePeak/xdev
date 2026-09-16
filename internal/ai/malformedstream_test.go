package ai

import (
	"context"
	"testing"
)

// TestAdaptersClassifyMangledFramesAsTransient pins the contract every wire
// adapter owes the agent ladder (M5 recovery): a frame the adapter cannot
// decode is delivered as an EventError that Classify calls transient, so the
// ladder backs off and retries the request instead of ending the session on
// one damaged body. The damage is real — a gateway between us and the
// provider rewrites the stream — and one bad frame used to cost the turn.
func TestAdaptersClassifyMangledFramesAsTransient(t *testing.T) {
	const garbage = `{not json at all`
	req := StreamRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}}}

	cases := []struct {
		adapter string
		stream  func(t *testing.T) []Event
	}{
		{"anthropic-messages", func(t *testing.T) []Event {
			srv, _ := newStreamServer(t, [2]string{"", garbage})
			p := NewAnthropicProvider("claude", srv.URL, "k", nil, nil)
			return drainProvider(t, p, req)
		}},
		{"openai-completions", func(t *testing.T) []Event {
			srv, _ := newStreamServer(t, [2]string{"", garbage})
			p := NewOpenAICompletionsProvider("router", srv.URL, "k", nil, nil)
			return drainProvider(t, p, req)
		}},
		{"openai-responses", func(t *testing.T) []Event {
			srv, _ := newStreamServer(t, [2]string{"", garbage})
			p := NewOpenAIResponsesProvider("codex", srv.URL, "k", nil, nil)
			return drainProvider(t, p, req)
		}},
		{"google-generative-ai", func(t *testing.T) []Event {
			srv, _ := newGoogleServer(t, []string{garbage})
			p := NewGoogleGenAIProvider("gemini", srv.URL, "k", nil, nil)
			return collectGoogle(t, p, req)
		}},
	}

	for _, c := range cases {
		t.Run(c.adapter, func(t *testing.T) {
			evs := c.stream(t)
			last := evs[len(evs)-1]
			if last.Type != EventError {
				t.Fatalf("last event = %q, want an error event (its error: %v)", last.Type, last.Err)
			}
			if got := Classify(last.Err); got != ClassTransient {
				t.Fatalf("Classify(%v) = %v, want ClassTransient — a mangled frame must reach the retry ladder, not end the turn", last.Err, got)
			}
		})
	}
}

// drainProvider starts a stream and collects it to its terminal event.
func drainProvider(t *testing.T, p Provider, req StreamRequest) []Event {
	t.Helper()
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return collectEvents(t, ch)
}
