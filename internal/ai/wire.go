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
