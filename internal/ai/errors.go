package ai

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/FreePeak/xdev/internal/ai/sse"
)

// HTTPError is a typed non-2xx response from a wire adapter. Raised at the
// source (wirePost) so classification never regexes formatted strings.
// Error() renders the legacy "<api>: HTTP <code>: <body>" format verbatim.
type HTTPError struct {
	API    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.API, e.Status, e.Body)
}

// ErrMalformedStream marks a streaming response body the adapter could not
// decode: an SSE frame whose JSON is not JSON (a proxy or gateway between us
// and the provider rewriting the stream is the usual cause). It is not a
// request problem — the same request sent again may well be served intact —
// so it classifies transient: the M5 ladder (agent.oneTurnWithRecovery)
// backoff-retries it in place, fails over once the ladder drains, and
// retains-and-continues when the mangled frame arrived after visible
// content. A stream that dies on one bad frame is resumed, not surfaced.
var ErrMalformedStream = errors.New("malformed stream response")

// ErrClass classifies a provider error for the retry/compaction engines
// (omp pi-ai Flag taxonomy, subset xdev acts on).
type ErrClass uint8

const (
	// ClassUnknown: no recovery machinery applies; surface as-is.
	ClassUnknown ErrClass = iota
	// ClassTransient: retriable with backoff — 408/409/429/5xx, network
	// failures, stream stall / premature close.
	ClassTransient
	// ClassAuthFailed: 401/403 — fail fast, retrying cannot succeed.
	ClassAuthFailed
	// ClassContextOverflow: the request exceeded the model's context
	// window; the compaction engine owns recovery.
	ClassContextOverflow
	// ClassEmptyTurn: the model produced no answer and no tool call
	// (a reasoning-only turn, or nothing at all). The session
	// layer rebuilds context from the persisted history and retries.
	ClassEmptyTurn
	// ClassBadRequest: 400/413/422 without overflow markers — fail fast.
	ClassBadRequest
)

// retriablableStatus: 408 request timeout, 409 conflict/retry, 429 rate limit.
func retriablableStatus(s int) bool {
	return s == 408 || s == 409 || s == 429 || s >= 500
}

// contextOverflowRe patterns match provider-specific overflow bodies
// (omp pi-ai flags.ts:24 overflow regexes, subset + generic).
var contextOverflowRe = []*regexp.Regexp{
	regexp.MustCompile(`(?i)exceeds the context window`),
	regexp.MustCompile(`(?i)context length.*(exceed|maximum|too long)`),
	regexp.MustCompile(`(?i)maximum context length`),
	regexp.MustCompile(`(?i)prompt is too long`),
	regexp.MustCompile(`(?i)input is too long`),
	regexp.MustCompile(`(?i)too many (input )?tokens`),
	regexp.MustCompile(`(?i)context_length_exceeded`),
	regexp.MustCompile(`(?i)exceeded model token limit`),
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),
}

// streamStallRe matches mid-stream transport death classifiers
// (omp turn-recovery.ts:76-84).
var (
	streamStallRe      = regexp.MustCompile(`(?i)stream stall`)
	http2StreamResetRe = regexp.MustCompile(`(?i)(nghttp2|http2).*(internal_error|refused_stream)|stream error received`)
	// prematureCloseRe also matches the adapters' own "clean EOF but no
	// terminal event" failures — the strings they actually emit
	// (openai-completions .../finish_reason, openai-responses .../
	// response.completed, anthropic-messages .../message_stop, and the
	// compaction/handoff side requests .../done).
	prematureCloseRe  = regexp.MustCompile(`(?i)stream closed before a (finish_reason|terminal response event)|(unexpected|premature) EOF|body closed before|stream ended without (finish_reason|response\.completed|message_stop|done)`)
	connectionResetRe = regexp.MustCompile(`(?i)(connection reset|connection refused|broken pipe|no such host|i/o timeout|context deadline exceeded|tls: handshake failure)`)
)

// malformedRequestRe matches a 400 whose body names a missing REQUIRED FIELD:
// a body the provider rejected on shape, not a request the model asked for. The
// one this shipped for is openai-responses' `input` item shape —
//
//	`input[185]` missing required field `output`
//
// — where a toolResult with no text serialized to a function_call_output that
// omitted the required `output` field. Left a bad request, the retry ladder
// ends the run; retried, the next turn rebuilds the request from history and
// the agent loop's placeholder (ai.EnsureToolOutput) makes it valid.
//
// ponytail: a body regex is a deliberate shortcut with a ceiling — it retries
// any missing-field 400, including one a fixed history cannot satisfy, which
// burns the ladder's attempts before surfacing the same error. The upgrade path
// is a provider-reported error code on HTTPError instead of body sniffing.
var malformedRequestRe = regexp.MustCompile(`(?i)missing required field`)

// modelVerdictRe matches a 404 whose body is a gateway relaying an upstream
// verdict about the MODEL rather than a dead route. Live 2026-09-18 through
// onegw (`defaultModel: onegw/xdev`): the pinned model had left the upstream's
// catalog, and every turn ended with
//
//	HTTP 404 {"error":{"code":"404","message":"… This model was Unbiased's
//	Pareto. Use it now: …","type":"upstream_error"}}
//
// A bare 404 is a dead route and stays terminal (wrong base URL, wrong path —
// retrying only re-sends the same request). A 404 that NAMES a model verdict
// indicts THIS target alone: the same request served by the next chain target
// works, which is what onegw's own router concludes (`types.APIError.
// ModelScoped`: it benches the (provider, model) pair and falls through
// instead of rotating the account pool). The vocabulary is the gateway's:
// onegw's `model_not_found` / `no provider for model` routing verdicts and the
// `upstream_error` type it wraps an upstream 404 in.
//
// ponytail: a body regex is a deliberate shortcut with a ceiling — it trusts
// any 404 that mentions these words, so a proxy whose error page happens to
// carry them retries the ladder before surfacing the same 404. The upgrade
// path is a provider-reported error code on HTTPError instead of body sniffing.
var modelVerdictRe = regexp.MustCompile(`(?i)model_not_found|model not found|no provider for model|upstream_error|404 page not found`)

// toolNameTooLongRe matches a 400 the gateway rejects because a tool
// name exceeds the provider's 64-character ceiling. Names come from
// external sources (MCP servers, extension binaries) that xdev does
// not control, so the right fix at this layer is to retry: the next
// turn rebuilds the request from history, and names the model already
// called (a persisted assistant message) never exceed the limit. Left
// a bad request, the run ended with no way back.
var toolNameTooLongRe = regexp.MustCompile(`(?i)name.*at most 64`)

// classify by status + body; raw transport errors by their message.
func Classify(err error) ErrClass {
	if err == nil {
		return ClassUnknown
	}
	// Empty turn (#389, #331): a model that produced nothing
	// is not a transport failure.
	if strings.Contains(err.Error(), "empty-turn") {
		return ClassEmptyTurn
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status == 401 || he.Status == 403:
			return ClassAuthFailed
		case he.Status == 400 || he.Status == 413 || he.Status == 422:
			if bodyIndicatesOverflow(he.Body) {
				return ClassContextOverflow
			}
			if malformedRequestRe.MatchString(he.Body) || toolNameTooLongRe.MatchString(he.Body) {
				return ClassTransient
			}
			return ClassBadRequest
		case he.Status == 404 && modelVerdictRe.MatchString(he.Body):
			// The model left the catalog; the ROUTE is fine. Retriable so
			// the ladder fails over to the next chain target (see
			// modelVerdictRe). In-place retries are bounded by MaxRetries,
			// and a bare 404 (no verdict in the body) stays terminal.
			return ClassTransient
		case retriablableStatus(he.Status):
			return ClassTransient
		default:
			return ClassUnknown
		}
	}
	// Transport-level failures: classify from the message.
	msg := err.Error()
	switch {
	case errors.Is(err, ErrWatchdogAborted):
		// Watchdog expiry cancels the STREAM's context only; the agent's
		// own ctx is untouched, so the M5 ladder (retry → failover,
		// retain-and-continue for partials) owns recovery.
		return ClassTransient
	case errors.Is(err, ErrMalformedStream), errors.Is(err, sse.ErrMalformed):
		// A body we could not decode — a frame whose JSON is not JSON, or a
		// line past the shared reader's 1 MiB cap — is something the
		// transport delivered damaged, not a request the provider rejected:
		// retry it (see ErrMalformedStream).
		return ClassTransient
	case bodyIndicatesOverflow(msg):
		return ClassContextOverflow
	case streamStallRe.MatchString(msg),
		http2StreamResetRe.MatchString(msg),
		prematureCloseRe.MatchString(msg),
		connectionResetRe.MatchString(msg):
		return ClassTransient
	}
	// net.Error timeouts and url.Errors (proxy/DNS) are transient.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassTransient
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ClassTransient
	}
	return ClassUnknown
}

// Retriable reports whether backoff-and-retry can plausibly succeed.
func Retriable(err error) bool {
	return Classify(err) == ClassTransient
}

func bodyIndicatesOverflow(body string) bool {
	if body == "" {
		return false
	}
	for _, re := range contextOverflowRe {
		if re.MatchString(body) {
			return true
		}
	}
	return false
}

// TruncateBody flattens s to one line and clamps it for error messages.
func TruncateBody(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
