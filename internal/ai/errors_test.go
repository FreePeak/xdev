package ai

import (
	"errors"
	"net/url"
	"testing"
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
		{"429 rate limit", &HTTPError{API: "a", Status: 429, Body: "rate limited"}, ClassTransient},
		{"500 server", &HTTPError{API: "a", Status: 500, Body: "boom"}, ClassTransient},
		{"408 timeout", &HTTPError{API: "a", Status: 408, Body: "slow"}, ClassTransient},
		{"400 overflow body", &HTTPError{API: "a", Status: 400, Body: "This model's maximum context length is 128000 tokens"}, ClassContextOverflow},
		{"413 overflow body", &HTTPError{API: "a", Status: 413, Body: "prompt is too long: exceeds the limit of 12345"}, ClassContextOverflow},
		{"400 plain", &HTTPError{API: "a", Status: 400, Body: "bad parameter"}, ClassBadRequest},
		{"400 empty body", &HTTPError{API: "a", Status: 400, Body: ""}, ClassBadRequest},
		{"404 unknown", &HTTPError{API: "a", Status: 404, Body: "nope"}, ClassUnknown},
		{"302 unknown", &HTTPError{API: "a", Status: 302, Body: "redir"}, ClassUnknown},
		{"transport overflow text", errors.New("agent: stream: prompt exceeds the context window of 1000000"), ClassContextOverflow},
		{"connection reset", errors.New("a: Post \"http://x\": read tcp: connection reset by peer"), ClassTransient},
		{"context deadline", errors.New("a: Post http://x: context deadline exceeded"), ClassTransient},
		{"stream stall", errors.New("stream stall detected after 30s"), ClassTransient},
		{"premature close", errors.New("stream closed before a terminal response event"), ClassTransient},
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
