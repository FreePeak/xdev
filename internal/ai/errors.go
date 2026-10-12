package ai

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"syscall"

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
	// serverErrorRe matches a 403 the gateway wraps when the UPSTREAM
	// served an invalid / non-JSON response. Live bodies look like:
	//
	//   Upstream request failed: [server_error] Upstream response was not valid JSON
	//   {"error":{"code":"server_error","message":"Upstream response was not valid JSON","type":"server_error"}}
	//
	// The upstream hiccupped, not xdev's request, so the same turn
	// retried may succeed. A plain JSON 403 (access denied) stays auth.
	serverErrorRe = regexp.MustCompile(`(?i)(upstream (request failed.*)?(response )?was not valid json|\[server_error\].*not valid json)`)
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

// upstreamRefusalRe matches a 400 the gateway wraps when the UPSTREAM
// refused the request, not one the gateway itself rejected on shape.
// Two phrasings of the same relay, both live:
//
//	HTTP 400 {"error":{"code":"400","message":"Error from provider (Console Go): Upstream request could not be processed","type":"invalid_request_error"}}
//	HTTP 400 {"error":{"code":"400","message":"Upstream request failed: [invalid_request_error] invalid request","type":"invalid_request_error"}}
//
// The second names no provider and still relays an upstream verdict: the
// gateway got a non-2xx from upstream and wrapped it with its own 400
// shape. Left ClassBadRequest it took only the bounded escalation rounds
// (two quick retries) and ended the run, because there is nothing in the
// body a rebuild could fix — the same turn served seconds later succeeds.
// ClassTransient gives it the full ladder instead: backoff in place,
// failover to the next chain target, then escalation rounds (see the
// 404 routing verdicts in modelVerdictRe). A gateway 400 that names no
// upstream at all (bare invalid_request_error) stays ClassBadRequest and
// fails fast.
//
// ponytail: a body regex is a deliberate shortcut with a ceiling — it
// trusts any body carrying this vocabulary, so a proxy whose error page
// happens to include it retries before surfacing the same 400. The
// upgrade path is a provider-reported error code on HTTPError instead of
// body sniffing.
var upstreamRefusalRe = regexp.MustCompile(`(?i)upstream (request (failed|could not be processed)|response was not valid json)`)

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
		case he.Status == 401:
			return ClassAuthFailed
		case he.Status == 403 && serverErrorRe.MatchString(he.Body):
			// A 403 whose body says the upstream's response was not
			// valid JSON / server_error: the gateway/proxy could not
			// parse the provider's answer, so the provider never
			// rejected the request itself — retrying may succeed.
			// A JSON-carrying 403 falls through to ClassAuthFailed.
			return ClassTransient
		case he.Status == 403:
			return ClassAuthFailed
		case he.Status == 400 || he.Status == 413 || he.Status == 422:
			if bodyIndicatesOverflow(he.Body) {
				return ClassContextOverflow
			}
			if malformedRequestRe.MatchString(he.Body) || toolNameTooLongRe.MatchString(he.Body) || upstreamRefusalRe.MatchString(he.Body) {
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

// outageRe matches the transport failures that mean "the request never
// reached the provider": the machine has no route to the network at all.
// `no such host` is deliberately NOT here even though connectionResetRe
// carries it — that one is the DNS answer for a single dead hostname, and
// the live shape it appears in is a gateway relaying a provider's own
// name-resolution failure, which is not the local link being down.
var outageRe = regexp.MustCompile(`(?i)(network is unreachable|no route to host|network is down|host is down|temporary failure in name resolution)`)

// IsNetworkOutage reports whether err is a local connectivity failure — the
// transport never opened a connection to the provider, so every target and
// every retry will fail identically until the machine is back on the
// network. That is a different thing from a transient provider blip, and
// the recovery ladder treats it differently: an outage may not end a turn
// (agent.oneTurnWithRecovery), because the request that failed is still a
// perfectly valid request and the user's only action would be to resend it.
//
// Deliberately narrow. `connection refused` is excluded on purpose: that is
// what a DEAD PROVIDER answers, and the ladder must still bound it (a
// gateway on loopback that is not running is the case the existing
// escalation bound was written for). So is a timeout: a wedged host and a
// missing link look the same at the socket, and only the kernel's routing
// verdict is unambiguous.
func IsNetworkOutage(err error) bool {
	if err == nil {
		return false
	}
	for _, se := range []error{
		syscall.ENETDOWN, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETRESET,
	} {
		if errors.Is(err, se) {
			return true
		}
	}
	// A DNS failure is an outage only when the resolver itself could not be
	// reached or answered "try again"; NXDOMAIN for one name is a host that
	// does not exist, which is a provider problem, not a link.
	var d *net.DNSError
	if errors.As(err, &d) && (d.IsTemporary || d.IsTimeout || d.Server != "") {
		return true
	}
	return outageRe.MatchString(err.Error())
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
