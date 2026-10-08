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

// wirePost builds and sends one streaming POST. It returns the 2x response,
// or a descriptive pre-flight error (status plus up to 4 KiB of body).
//
// The header phase is bounded HERE rather than on the transport. Two
// clients reach this function — the shared one (wireHTTPClient, which
// carries ResponseHeaderTimeout: 5m) and the one buildProvider constructs
// per provider (cmd/xdev/print.go), which had no timeout at all — and the
// live reproduction on 2026-09-29 found the gap: a gateway that accepts
// the connection and then never answers held a print run open indefinitely
// (killed at 210s and again at 195s, zero records written), because the
// 90s stream watchdog only starts AFTER this returns, and the per-provider
// client had nothing bound above it. One budget here covers both clients
// and lands the expiry at the stream watchdog's own FirstProgressTimeout,
// so "the host said nothing" becomes one 90s wait rather than a
// transport-dependent 90s or 5m or forever.
//
// The budget lives in wirePostWithTimeout, one function down, for a reason
// found the hard way: cancelHeader MUST NOT run once the headers are in,
// because the adapter reads resp.Body from that same context for the rest
// of the stream. A `defer cancelHeader()` here — the obvious shape — killed
// every healthy stream the moment its headers arrived: all the
// openai_*/google_*/responses_* happy-path tests read `context canceled`.
// The split is what lets go vet's lostcancel keep checking a function that
// DOES call its cancel on every path, rather than a per-callsite nolint
// that would teach the check to ignore a real leak.
//
// ponytail: not cancelling on success costs one live timer per long-lived
// stream, freed when the parent context (which every adapter cancels at
// stream end) goes away. The upgrade path, if that ever shows up, is a
// per-provider override rather than a larger constant here.
func wirePost(ctx context.Context, hc *http.Client, url string, headers map[string]string, body []byte, api string) (*http.Response, error) {
	return wirePostWithTimeout(ctx, hc, url, headers, body, api, FirstProgressTimeout)
}

// wirePostWithTimeout is wirePost with an explicit header budget, which is
// what lets a test shrink it without a 90s sleep. The budget is armed on
// the request's own context and released on every path this function owns
// except the success path, where the response body is read from that same
// context afterwards.
func wirePostWithTimeout(ctx context.Context, hc *http.Client, url string, headers map[string]string, body []byte, api string, headerBudget time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", api, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// The bound goes on a CLONED transport, not on a context, and that is the
	// only shape that works here. Two earlier attempts failed and both
	// failures are worth keeping in the record:
	//
	//  - `defer cancelHeader()` on a context.WithTimeout: the adapter reads
	//    resp.Body from that same context for the rest of the stream, so this
	//    killed every healthy stream the moment its headers arrived. All the
	//    openai_*/google_*/responses_* happy-path tests read
	//    `context canceled`.
	//  - not cancelling on success, to dodge that: go vet's lostcancel then
	//    fires (and would have to be silenced per-callsite, teaching the
	//    check to ignore a real leak).
	//
	// Transport.ResponseHeaderTimeout is the purpose-built mechanism: it
	// bounds the header wait and leaves the body unbounded, which is exactly
	// the split wanted here, with no cancel to own. The headers are set
	// BEFORE the clone, because arming first made the recorded Authorization
	// header come back EMPTY on the google/vertex and azure tests — the extra
	// context wrapper raced the transport's retry-on-a-closed-pooled-
	// connection path, which re-issues the request and found a header map
	// that no longer carried the token.
	budgeted, ok := withHeaderBudget(hc, headerBudget)
	if !ok {
		// A custom RoundTripper (tests, and any future caller) cannot be
		// cloned into a header timeout. The stream watchdog still bounds the
		// body; the header wait is this transport's own business. ponytail:
		// the ceiling is unchanged for that case, not a new hazard — upgrade
		// path is a wrapper RoundTripper if a real caller ever needs it.
		budgeted = hc
	}
	resp, err := budgeted.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", api, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, &HTTPError{API: api, Status: resp.StatusCode, Body: wireErrBody(resp.Body)}
	}
	return resp, nil
}

// withHeaderBudget returns a client whose transport bounds the response
// header wait to d, leaving the body read unbounded. It reports false when
// the transport is not an *http.Transport and cannot be cloned into that
// shape — the caller then uses the client unchanged.
//
// Clone() rather than mutating hc: the shared wireHTTPClient is package
// state used by every adapter, and a per-request mutation would be a data
// race between concurrent streams.
func withHeaderBudget(hc *http.Client, d time.Duration) (*http.Client, bool) {
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		if hc.Transport == nil {
			tr = nil // http.DefaultTransport underneath; clone nil as the zero value
		} else {
			return nil, false
		}
	}
	clone := tr.Clone()
	clone.ResponseHeaderTimeout = d
	out := *hc
	out.Transport = clone
	return &out, true
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

// reasoningEffort maps a thinking budget to the OpenAI reasoning-effort
// tier. The wire's own vocabulary stops at "high" (no OpenAI-compatible
// endpoint documents xhigh/max), so every rung at or above high's budget
// folds onto it: the top rungs of xdev's ladder are a wider budget, and the
// adapters that carry a NUMBER (Anthropic's budget_tokens) are the ones that
// express that. Folding rather than passing an unknown tier is deliberate —
// an endpoint that rejects an unrecognised effort would fail the whole turn
// over a rung the user asked for.
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
		return scrubToolArgsNUL(args)
	}
	logx.Errorf("%s: unparseable tool call %s arguments, sent as {}; the model must re-issue it: %s", api, id, raw)
	return json.RawMessage("{}")
}

// scrubToolArgsNUL removes NUL from every string inside a decoded tool-call
// argument object. NUL is legal inside a JSON string, so the strict parse
// above accepts it and no downstream decoder objects — but execve(2) rejects
// any argv string containing one with EINVAL, which surfaced live as
// `bash: start: fork/exec /bin/bash: invalid argument` and sent the model
// hunting a shell that was never broken (measured 2026-10-05 on
// onegw/opencode-space-bunny-free: 51 of 83,060 stored calls carried a U+0000).
//
// Every OTHER control character is legitimate payload and is left alone: \t
// \n \r are what a multi-line command, a `write` of a source file and an
// `edit` patch are made of. An argument left EMPTY by the scrub stays an empty
// string rather than losing its key — dropping the key would silently satisfy
// a `required` constraint the model never satisfied.
//
// ponytail: an argument scrubbed down to empty stays an empty string, so the
// tool's own "command is required" error is what the model reads — no schema
// walk and no per-tool opt-in here. Upgrade path: if a tool ever takes a
// legitimate binary payload, that tool decodes it from base64 in its own
// decoder rather than this scrub learning about it.
func scrubToolArgsNUL(args json.RawMessage) json.RawMessage {
	// A NUL in a JSON string arrives ESCAPED (\u0000), so scanning the raw
	// bytes for the 0x00 code unit misses every real case — the guard has to
	// look for the escape as well as the literal byte.
	if !bytes.ContainsRune(args, 0) && !bytes.Contains(args, []byte(`\u0000`)) {
		return args
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return args // not an object; the tool's own decoder owns that error
	}
	// Marshal skips nil, so a scrubbed-away value would DELETE its key and
	// satisfy a `required` constraint the model never met. {"path":"\x00"} must
	// reach the tool as {"path":""}, not {}.
	v = scrubValueNUL(v)
	out, err := json.Marshal(v)
	if err != nil {
		logx.Errorf("tool call arguments re-encoded to an empty object after the NUL scrub")
		return json.RawMessage("{}")
	}
	return out
}

// scrubValueNUL walks a decoded JSON value, dropping NUL from every string.
// Numbers and booleans are returned unchanged, so a numeric argument never
// round-trips through float formatting on its way back to JSON.
func scrubValueNUL(v any) any {
	switch t := v.(type) {
	case string:
		if strings.ContainsRune(t, 0) {
			return strings.ReplaceAll(t, "\x00", "")
		}
	case []any:
		for i := range t {
			t[i] = scrubValueNUL(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = scrubValueNUL(t[k])
		}
	}
	return v
}

// ScrubToolArgs and ScrubToolName are the exported half of the guard above, so
// the agent applies the same scrub to a tool call that never passed through
// this package's parser (a replayed session, an imported log, a synthesized
// block). Both are idempotent: scrubbing a clean value returns it unchanged.
func ScrubToolArgs(args json.RawMessage) json.RawMessage { return scrubToolArgsNUL(args) }

func ScrubToolName(name string) string { return scrubToolName(name) }

// scrubToolName is the same guard for the call's NAME. A spliced name reached
// the registry lookup verbatim and answered `unknown tool "\x00bash"`, which
// reads to the model as "this session has no bash tool" and to the user as a
// broken session. A name that was only the splice has nothing to salvage and
// stays empty, routing to the existing nameless-call error rather than
// inventing a tool.
func scrubToolName(name string) string {
	if !strings.ContainsRune(name, 0) {
		return name
	}
	return strings.ReplaceAll(name, "\x00", "")
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
