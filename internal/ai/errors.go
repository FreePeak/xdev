package ai

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
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
	prematureCloseRe   = regexp.MustCompile(`(?i)stream closed before a (finish_reason|terminal response event)|(unexpected|premature) EOF|body closed before`)
	connectionResetRe  = regexp.MustCompile(`(?i)(connection reset|connection refused|broken pipe|no such host|i/o timeout|context deadline exceeded|tls: handshake failure)`)
)

// Classify maps an error to its recovery class. Wire HTTPError instances
// classify by status + body; raw transport errors by their message.
func Classify(err error) ErrClass {
	if err == nil {
		return ClassUnknown
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
			return ClassBadRequest
		case retriablableStatus(he.Status):
			return ClassTransient
		default:
			return ClassUnknown
		}
	}
	// Transport-level failures: classify from the message.
	msg := err.Error()
	switch {
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
