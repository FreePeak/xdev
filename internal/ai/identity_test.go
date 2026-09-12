package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// #102: install-id minted a file that no request attached. The wire layer now
// carries it — Claude's metadata.user_id device blob and the Responses/Codex
// `user` field — and sends nothing when no identity was installed.

func captureTransport(t *testing.T, got *[]byte) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		*got, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeRequestCarriesDeviceID(t *testing.T) {
	SetInstallIdentity("11111111-2222-4333-8444-555555555555")
	t.Cleanup(func() { SetInstallIdentity("") })
	var body []byte
	p := NewAnthropicProvider("claude", "https://example.invalid/v1", "k", nil, captureTransport(t, &body))
	drainStream(t, p)

	var wr struct {
		Metadata *struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &wr); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	if wr.Metadata == nil || wr.Metadata.UserID == "" {
		t.Fatalf("no metadata.user_id attached: %s", body)
	}
	var blob map[string]string
	if err := json.Unmarshal([]byte(wr.Metadata.UserID), &blob); err != nil {
		t.Fatalf("user_id must be the omp JSON blob, got %q: %v", wr.Metadata.UserID, err)
	}
	if blob["device_id"] == "" {
		t.Fatalf("device_id missing: %s", wr.Metadata.UserID)
	}
}

func TestResponsesCarryUserOnlyForCodex(t *testing.T) {
	SetInstallIdentity("aaaaaaaabbbb4cccc8dddddddddddd")
	t.Cleanup(func() { SetInstallIdentity("") })

	var plain []byte
	op := NewOpenAIResponsesProvider("openai", "https://example.invalid/v1", "k", nil, captureTransport(t, &plain))
	drainStream(t, op)
	var plainBody struct {
		User string `json:"user"`
	}
	_ = json.Unmarshal(plain, &plainBody)
	if plainBody.User != "" {
		t.Fatalf("plain responses must not attach the id: %s", plain)
	}

	var cx []byte
	cp := NewOpenAICodexResponsesProvider("codex", "https://example.invalid/v1", "k", nil, captureTransport(t, &cx))
	drainStream(t, cp)
	var codexBody struct {
		User string `json:"user"`
	}
	if err := json.Unmarshal(cx, &codexBody); err != nil {
		t.Fatalf("codex body: %v (%s)", err, cx)
	}
	if codexBody.User == "" {
		t.Fatalf("codex request never attached the install id: %s", cx)
	}
}

// An unset identity must omit the fields entirely, not send empty ones.
func TestIdentityUnsetOmitsFields(t *testing.T) {
	SetInstallIdentity("")
	var body []byte
	p := NewAnthropicProvider("claude", "https://example.invalid/v1", "k", nil, captureTransport(t, &body))
	drainStream(t, p)
	if strings.Contains(string(body), `"metadata"`) {
		t.Fatalf("empty identity still emitted metadata: %s", body)
	}
}

// drainStream issues one request and consumes the event stream until the
// provider closes it; the captured body is what the test asserts on.
func drainStream(t *testing.T, p Provider) {
	t.Helper()
	ch, err := p.Stream(context.Background(), StreamRequest{
		Model:    "m",
		System:   "sys",
		Messages: []Message{{Role: RoleUser, Content: []Block{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for range ch {
	}
}
