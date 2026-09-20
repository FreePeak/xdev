package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/logx"
)

// wireHTTPClient is the shared transport for every wire adapter: no overall
// timeout (streams legitimately run for minutes), but bounded dial, TLS
// handshake (30s) and response-header (5m) phases so dead endpoints fail.
var wireHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
	},
}

// wirePost builds and sends one streaming POST. It returns the 2xx response,
// or a descriptive pre-flight error (status plus up to 4 KiB of body).
func wirePost(ctx context.Context, hc *http.Client, url string, headers map[string]string, body []byte, api string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", api, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", api, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, &HTTPError{API: api, Status: resp.StatusCode, Body: wireErrBody(resp.Body)}
	}
	return resp, nil
}

// wireErrBody reads up to 4 KiB of an error body and flattens it to one line.
func wireErrBody(r io.Reader) string {
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(r, buf)
	return strings.Join(strings.Fields(string(buf[:n])), " ")
}

// malformedStream wraps a wire-decode failure in ErrMalformedStream. The
// rendered text keeps the "<api>: <stage>: <json error>" shape the adapters
// have always reported, with the sentinel's own text in the middle so a log
// reader can tell a mangled body from a rejected request.
func malformedStream(api, stage string, err error) error {
	return fmt.Errorf("%s: %w: %s: %v", api, ErrMalformedStream, stage, err)
}

// reasoningEffort maps a thinking budget to the OpenAI reasoning-effort tier.
func reasoningEffort(tokens int) string {
	switch {
	case tokens <= 2048:
		return "low"
	case tokens <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// emptyJSONObjectArgs normalizes empty tool-call arguments to "{}": providers
// reject an empty string in the arguments field.
func emptyJSONObjectArgs(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

// parseToolArgs turns a streamed tool-call argument blob into strict JSON.
// The strict parse is authoritative. When it fails the call is answered with
// {} rather than a guess, and the turn lives: a sloppy model (GLM-family in
// the field, 2026-09-14: a missing comma before an unquoted key) or a
// response cut off by max_tokens used to raise a fatal stream error that the
// retry ladder could not classify, killing the run and leaving an unpaired
// tool call. Salvaging the parseable prefix is tempting but wrong: the
// repairer stops at the first pair it cannot read, so
// {"path":"/x" content:"..."} would become a write of an empty file — the
// tool's own required-argument error is honest by comparison, and the model
// sees it and re-issues the call.
//
// ponytail: a tool whose arguments are all optional executes on defaults
// instead of refusing (only harmless scans fall in that set today). Upgrade
// path: mark the block and have agent.runOneTool answer it with an error
// result without executing.
func parseToolArgs(api, id, raw string) json.RawMessage {
	if strings.TrimSpace(raw) == "" {
		return json.RawMessage("{}")
	}
	var args json.RawMessage
	if err := json.Unmarshal([]byte(raw), &args); err == nil {
		return args
	}
	logx.Errorf("%s: unparseable tool call %s arguments, sent as {}; the model must re-issue it: %s", api, id, raw)
	return json.RawMessage("{}")
}

// modelsProbeURL is the catalog endpoint a HealthCheck probes: {base}/models,
// or {base}/v1/models only when the base URL carries no version segment of
// its own. The chat wire builds {base}/chat/completions from the same base,
// so a base that already ends in /v1 (onegw, and every OpenAI-shaped gateway)
// must not be handed a second one: /v1/v1/models 404s on a healthy host, and
// the probe then reports a live gateway as down. This is the same rule
// internal/config's discovery applies, so a probe never asks for a path the
// provider is not already serving.
func modelsProbeURL(baseURL string) string {
	b := strings.TrimSuffix(baseURL, "/")
	if strings.Contains(b, "/v1") { // also matches /v1beta
		return b + "/models"
	}
	return b + "/v1/models"
}

// healthCheckOneGet runs a single GET against the provider's catalog
// endpoint. It is the cheap liveness probe every provider's HealthCheck
// delegates to: no body, no auth header (the agent's key rides on the
// httpClient), and a short timeout so a dead host fails fast instead of
// burning the escalation ladder.
//
// Only a transport failure is an error. The probe asks one question — is the
// host answering? — and any HTTP status answers it: a 200, or a 401 from a
// gateway that wants the key this probe deliberately does not send (onegw's
// catalog), or even a 404 from a mirror serving no catalog at all. Reading
// those as "down" is what made a live gateway unusable: the turn was refused
// before a single request was spent, and the real fault was never reported by
// the request that would have failed on it. A host that is genuinely down
// accepts no connection at all — that, and only that, is the error below.
func healthCheckOneGet(ctx context.Context, hc *http.Client, url string, headers map[string]string, api string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%s: health check: build request: %w", api, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: health check: %w", api, err)
	}
	defer resp.Body.Close()
	return nil
}


// cleanUTF8 keeps only valid UTF-8 runes and drops U+FFFD. A vendor that
// splices invalid bytes into its SSE JSON (or already substitutes U+FFFD
// before framing) would otherwise land mojibake in the thinking box and
// the session JSONL. English, Mandarin, Vietnamese and every other real
// script pass through unchanged — they are valid UTF-8 without U+FFFD.
//
// Used at every text/thinking emit so a single call site cannot forget.
func CleanUTF8(s string) string {
	if s == "" {
		return s
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if !strings.ContainsRune(s, '\uFFFD') {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\uFFFD' {
			return -1
		}
		return r
	}, s)
}
