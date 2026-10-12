package ai

import (
	"net/http"
	"strconv"
	"strings"
)

// DecisionHeader is the response header onegw stamps on every proxied reply
// to say which leg actually served it:
//
//	provider=opencode-free; account=free-003; model=big-pickle; attempts=1;
//	request=execution; fallback=true
//
// A gateway in front of the model is the only party that knows this: the
// client asked for `execution`, the gateway's combo refused on its first leg
// and a sibling answered instead. Nothing in the body says so — the SSE
// frames name the serving model at best, and the client would have to guess
// whether that was a switch or just a different id for the same thing.
//
// Only onegw sends it. Every other provider leaves the header absent, and
// then nothing here fires: the parse is one map lookup on a response the
// adapter already holds.
const DecisionHeader = "X-OneGW-Decision"

// Decision is a parsed DecisionHeader. Zero value = no header (every field
// empty, Fallback false), which is the shape a non-onegw provider answers.
type Decision struct {
	Provider string
	Account  string
	Model    string
	Attempts int
	// Request is the model string the CLIENT asked for. Absent from a
	// gateway older than the request=/fallback= pair, and from a request
	// whose client model string the gateway could not read.
	Request string
	// Fallback is true exactly when the serving leg is not the one the
	// client's model string resolves to first. A same-target retry is
	// attempts>1 with Fallback false — which is why a client must read this
	// field rather than counting attempts.
	Fallback bool
}

// ServedLeg renders the leg that served as "provider/model" (the form every
// xdev surface names a model in), "" when the header carried neither field.
func (d Decision) ServedLeg() string {
	switch {
	case d.Provider != "" && d.Model != "":
		return d.Provider + "/" + d.Model
	case d.Model != "":
		return d.Model
	case d.Provider != "":
		return d.Provider
	}
	return ""
}

// ParseDecision decodes a DecisionHeader value. ok is false for an empty or
// unparseable value: an unknown field is ignored (a future gateway may add
// one) and a malformed pair is skipped rather than failing the whole parse,
// because this header is an annotation — a client must never lose an answer
// over it.
func ParseDecision(v string) (Decision, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Decision{}, false
	}
	var d Decision
	seen := false
	for _, pair := range strings.Split(v, ";") {
		k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		k, val = strings.TrimSpace(k), strings.TrimSpace(val)
		switch k {
		case "provider":
			d.Provider, seen = val, true
		case "account":
			d.Account, seen = val, true
		case "model":
			d.Model, seen = val, true
		case "attempts":
			if n, err := strconv.Atoi(val); err == nil {
				d.Attempts, seen = n, true
			}
		case "request":
			d.Request, seen = val, true
		case "fallback":
			if b, err := strconv.ParseBool(val); err == nil {
				d.Fallback, seen = b, true
			}
		}
	}
	return d, seen
}

// servedByDecision reports the leg a response was served by, when the wire
// said it was NOT the model the caller asked for. "" means "no divergence
// reported" — a missing header, fallback=false, or a serving leg that
// happens to spell the requested model (an alias pointing at itself), all of
// which must stay silent rather than announce a switch that did not happen.
//
// It takes the response rather than a parsed Decision because the adapters
// hold exactly the response and the model they asked for.
func servedByDecision(resp *http.Response, requested string) string {
	if resp == nil {
		return ""
	}
	d, ok := ParseDecision(resp.Header.Get(DecisionHeader))
	if !ok || !d.Fallback {
		return ""
	}
	leg := d.ServedLeg()
	if leg == "" || leg == requested {
		return ""
	}
	return leg
}

// SwitchPhrase is the human wording of a switch, shared by every surface that
// reports one: the TUI toast, the Telegram caption, the stderr line. One
// wording, so a user who sees two of them cannot think they are two events.
func SwitchPhrase(requested, served string) string {
	return switchPrefix + requested + " → " + served
}

// switchPrefix is the fixed opening of the phrase, named once so the printer
// and the parser cannot drift apart on a typo.
const switchPrefix = "model switched: "

// modelSwitchNotice is SwitchPhrase in the bracketed form print mode writes
// to stderr — bracketed because stdout is the model's answer, and a bare
// sentence there would be read as part of it. The Telegram bridge parses
// this exact line back out of the worker's combined output.
func modelSwitchNotice(requested, served string) string {
	return "[" + SwitchPhrase(requested, served) + "]"
}

// ModelSwitchNotice renders the user-facing line for a served-by-someone-else
// turn: "" when the two names are the same, so a caller can print it
// unconditionally.
func ModelSwitchNotice(requested, served string) string {
	if served == "" || served == requested {
		return ""
	}
	return modelSwitchNotice(requested, served)
}

// ParseModelSwitchNotice recovers (requested, served) from a ModelSwitchNotice
// line found anywhere in text — the Telegram bridge reads the worker's
// combined output, where the notice sits among the answer and any other
// stderr narration. ok is false when no notice is present.
func ParseModelSwitchNotice(text string) (requested, served string, ok bool) {
	i := strings.Index(text, "["+switchPrefix)
	if i < 0 {
		return "", "", false
	}
	rest := text[i+len("["+switchPrefix):]
	j := strings.Index(rest, "]")
	if j < 0 {
		return "", "", false
	}
	req, srv, found := strings.Cut(rest[:j], " → ")
	if !found {
		return "", "", false
	}
	req, srv = strings.TrimSpace(req), strings.TrimSpace(srv)
	if req == "" || srv == "" {
		return "", "", false
	}
	return req, srv, true
}
