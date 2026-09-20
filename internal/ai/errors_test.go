package ai

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai/sse"
)

func TestHTTPErrorFormat(t *testing.T) {
	e := &HTTPError{API: "openai-completions", Status: 401, Body: `{"error":{"type":"authentication_error","message":"invalid api key"}}`}
	want := `openai-completions: HTTP 401: {"error":{"type":"authentication_error","message":"invalid api key"}}`
	if e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrClass
	}{
		{"nil", nil, ClassUnknown},
		{"401 auth", &HTTPError{API: "a", Status: 401, Body: "invalid api key"}, ClassAuthFailed},
		{"403 auth", &HTTPError{API: "a", Status: 403, Body: "forbidden"}, ClassAuthFailed},
		// A 403 the gateway wraps when the UPSTREAM served an invalid
		// response — the upstream hiccupped, not xdev's request, so the
		// same turn retried may succeed.
		{"403 upstream server_error", &HTTPError{API: "openai-completions", Status: 403,
			Body: `{"error":{"message":"Error from provider (openai): Upstream request failed: [server_error] Upstream response was not valid JSON","type":"server_error"}}`}, ClassTransient},
		{"500 server", &HTTPError{API: "a", Status: 500, Body: "boom"}, ClassTransient},
		{"408 timeout", &HTTPError{API: "a", Status: 408, Body: "slow"}, ClassTransient},
		{"400 overflow body", &HTTPError{API: "a", Status: 400, Body: "This model's maximum context length is 128000 tokens"}, ClassContextOverflow},
		{"413 overflow body", &HTTPError{API: "a", Status: 413, Body: "prompt is too long: exceeds the limit of 12345"}, ClassContextOverflow},
		{"400 plain", &HTTPError{API: "a", Status: 400, Body: "bad parameter"}, ClassBadRequest},
		{"400 empty body", &HTTPError{API: "a", Status: 400, Body: ""}, ClassBadRequest},
		// The one this shipped for: a request the provider rejected on shape
		// (a Responses `input` item missing the required `output` field, which
		// an empty toolResult produced). Retried, the next turn rebuilds the
		// request from history with the placeholder text; left a bad request,
		// the run ended with the session unrecoverable.
		{"400 missing required field", &HTTPError{API: "openai-completions", Status: 400,
			Body: `{"error":{"code":"400","message":"Error","type":"invalid_request_error"}} ` + "`input[185]` missing required field `output`"}, ClassTransient},
		{"400 invalid_request_error without a field", &HTTPError{API: "a", Status: 400,
			Body: `{"error":{"type":"invalid_request_error"}}`}, ClassBadRequest},
		// A 404 is a dead route unless the body is a gateway relaying an
		// upstream verdict about the MODEL. Live 2026-09-18: `onegw/xdev`
		// left its upstream's catalog and every turn ended on
		// `HTTP 404 … "type":"upstream_error"` — the retry ladder never saw
		// a class it acts on, so the run ended instead of failing over.
		{"404 dead route", &HTTPError{API: "a", Status: 404, Body: "nope"}, ClassUnknown},
		{"404 empty body", &HTTPError{API: "a", Status: 404, Body: ""}, ClassUnknown},
		{"404 model_not_found", &HTTPError{API: "openai-completions", Status: 404,
			Body: `{"error":{"code":"model_not_found","message":"no provider for model xdev"}}`}, ClassTransient},
		{"404 upstream_error relaying a model verdict", &HTTPError{API: "openai-completions", Status: 404,
			Body: `{"error":{"code":"404","message":"Thank you for participating in the Stealth Union Alpha testing period. This model was Unbiased's Pareto. Use it now: https://openrouter.ai/unbiased/pareto","type":"upstream_error"}}`}, ClassTransient},
		// A 400 the gateway rejects because a tool name (from an MCP
		// server or extension binary) exceeds 64 characters. The
		// name comes from external sources xdev does not control;
		// retried, the next turn rebuilds the request from history
		// and only uses names the model already called, which never
		// exceed the limit. Left a bad request, the run ended with
		// no way back (the session-killing "name must be at most
		// 64 characters" failure).
		{"400 tool name too long", &HTTPError{API: "openai-completions", Status: 400,
			Body: `{"error":{"code":"400","message":"Error","type":"invalid_request_error"}} ` +
				"`name` must be at most 64 characters, got 73"}, ClassTransient},
		// A 400 the gateway relays when the UPSTREAM refused the request
		// ("Error from provider (Console Go): Upstream request could not
		// be processed") — the same turn served seconds later succeeds,
		// so the ladder must retry instead of ending the run.
		{"400 upstream refusal relayed by gateway", &HTTPError{API: "openai-completions", Status: 400,
			Body: `{"error":{"code":"400","message":"Error from provider (Console Go): Upstream request could not be processed","type":"invalid_request_error"}}`}, ClassTransient},
		{"400 upstream refusal (other provider)", &HTTPError{API: "openai-completions", Status: 400,
			Body: `{"error":{"message":"Error from provider (openai): Upstream request failed: Endpoint is unavailable.","type":"invalid_request_error"}}`}, ClassTransient},
		{"302 unknown", &HTTPError{API: "a", Status: 302, Body: "redir"}, ClassUnknown},
		{"transport overflow text", errors.New("agent: stream: prompt exceeds the context window of 1000000"), ClassContextOverflow},
		{"connection reset", errors.New("a: Post \"http://x\": read tcp: connection reset by peer"), ClassTransient},
		{"context deadline", errors.New("a: Post http://x: context deadline exceeded"), ClassTransient},
		{"stream stall", errors.New("stream stall detected after 30s"), ClassTransient},
		{"premature close", errors.New("stream closed before a terminal response event"), ClassTransient},
		// The adapters' own missing-terminal-event strings. These are the
		// exact messages emitted by openai_completions.go,
		// openai_responses.go, anthropic.go and the compaction/handoff
		// side requests; classified anything else they would fall out of
		// the retry ladder and end the session (the "stream ended without
		// finish_reason kills the run" bug).
		{"openai-completions ended", errors.New("agent: stream: openai-completions: stream ended without finish_reason"), ClassTransient},
		{"openai-responses ended", errors.New("agent: stream: openai-responses: stream ended without response.completed"), ClassTransient},
		{"anthropic ended", errors.New("agent: stream: anthropic-messages: stream ended without message_stop"), ClassTransient},
		{"compaction ended", errors.New("compaction: stream ended without done"), ClassTransient},
		{"handoff ended", errors.New("handoff: stream ended without done"), ClassTransient},
		// A body the adapters could not decode. The mangle usually comes
		// from a proxy between us and the provider; classed anything
		// else, one bad frame ended the session instead of being retried.
		{"malformed frame", malformedStream("anthropic-messages", "decode event", errors.New("invalid character 'o'")), ClassTransient},
		{"malformed frame wrapped by the agent", fmt.Errorf("agent: stream: %w", malformedStream("openai-completions", "decode chunk", errors.New("unexpected end of JSON input"))), ClassTransient},
		{"oversized SSE line", fmt.Errorf("anthropic-messages: read stream: %w", fmt.Errorf("%w: line exceeds %d bytes", sse.ErrMalformed, sse.MaxLineBytes)), ClassTransient},
		// Not a stream-ended failure: must stay unmatched so the ladder
		// cannot be widened into retrying arbitrary text.
		{"ended without (nonsense)", errors.New("the meeting ended without finish_reason being decided"), ClassUnknown},
		{"wrapped url error", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("no such host")}, ClassTransient},
		{"generic message", errors.New("weird internal thing"), ClassUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.err); got != c.want {
				t.Fatalf("Classify(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestRetriable(t *testing.T) {
	if !Retriable(&HTTPError{API: "a", Status: 500, Body: "x"}) {
		t.Fatal("500 should be retriable")
	}
	if Retriable(&HTTPError{API: "a", Status: 401, Body: "x"}) {
		t.Fatal("401 must not be retriable")
	}
	if Retriable(&HTTPError{API: "a", Status: 400, Body: "maximum context length exceeded"}) {
		t.Fatal("overflow must not be retried by the retry ladder")
	}
}

// TestMalformedStreamKeepsTheAdapterMessage pins the log/UI contract: the
// sentinel is a classification hook, not a replacement for the detail the
// adapters have always reported (api, stage, and the JSON error itself).
func TestMalformedStreamKeepsTheAdapterMessage(t *testing.T) {
	err := malformedStream("openai-completions", "decode chunk", errors.New("invalid character 'o'"))
	got := err.Error()
	for _, want := range []string{"openai-completions", "decode chunk", "invalid character 'o'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message %q must keep %q", got, want)
		}
	}
}
