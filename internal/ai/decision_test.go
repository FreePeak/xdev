package ai

import (
	"net/http"
	"testing"
)

// The header onegw actually sends on a combo fall-through, verbatim from a
// live gateway: the serving leg is a DIFFERENT provider/model than the one
// the client asked for, and fallback=true is the only field that says so.
func TestParseDecisionFallback(t *testing.T) {
	d, ok := ParseDecision("provider=opencode-free; account=free-003; model=big-pickle; attempts=1; request=execution; fallback=true")
	if !ok {
		t.Fatal("a well-formed decision header must parse")
	}
	if d.Provider != "opencode-free" || d.Model != "big-pickle" || d.Account != "free-003" {
		t.Fatalf("leg fields = %+v", d)
	}
	if d.Request != "execution" || !d.Fallback || d.Attempts != 1 {
		t.Fatalf("request/fallback/attempts = %q %v %d", d.Request, d.Fallback, d.Attempts)
	}
	if got := d.ServedLeg(); got != "opencode-free/big-pickle" {
		t.Fatalf("ServedLeg = %q", got)
	}
}

// The pre-request=/fallback= header shape (a gateway older than this change)
// must still parse, and must not claim a switch: a client cannot tell a
// same-target retry from a fall-through without the new field, and guessing
// would announce a switch on every retried request.
func TestParseDecisionLegacyHeaderClaimsNoFallback(t *testing.T) {
	d, ok := ParseDecision("provider=p1; account=acct1; model=m1; attempts=3")
	if !ok {
		t.Fatal("the legacy header must parse")
	}
	if d.Fallback {
		t.Fatal("a header with no fallback field must not claim a fallback")
	}
	if d.Request != "" {
		t.Fatalf("Request = %q, want empty", d.Request)
	}
}

// An unparseable or absent header is not an error: this is an annotation, and
// a client must never lose an answer over it.
func TestParseDecisionEmptyAndJunk(t *testing.T) {
	for _, v := range []string{"", "   ", "garbage", "provider"} {
		if _, ok := ParseDecision(v); ok {
			t.Fatalf("ParseDecision(%q) claimed a parse", v)
		}
	}
}

// servedByDecision is the gate every adapter uses: it reports the serving leg
// ONLY when the wire said a different one answered. Each silent case here is
// a false "your request was switched" the user would otherwise read.
func TestServedByDecision(t *testing.T) {
	mk := func(v string) *http.Response {
		h := http.Header{}
		if v != "" {
			h.Set(DecisionHeader, v)
		}
		return &http.Response{Header: h}
	}
	cases := []struct {
		name, header, requested, want string
	}{
		{"fallback reports the serving leg",
			"provider=p2; account=ok; model=m2; attempts=2; request=pair; fallback=true", "pair", "p2/m2"},
		{"same target retry stays silent",
			"provider=p1; account=acct1; model=m1; attempts=3; request=p1/m1; fallback=false", "p1/m1", ""},
		{"no header stays silent", "", "m1", ""},
		{"legacy header stays silent",
			"provider=p2; account=ok; model=m2; attempts=2", "pair", ""},
		{"a leg that spells the request stays silent",
			"provider=p1; account=acct1; model=m1; attempts=1; request=p1/m1; fallback=true", "p1/m1", ""},
	}
	for _, c := range cases {
		if got := servedByDecision(mk(c.header), c.requested); got != c.want {
			t.Fatalf("%s: servedByDecision = %q, want %q", c.name, got, c.want)
		}
	}
	if got := servedByDecision(nil, "m1"); got != "" {
		t.Fatalf("nil response = %q, want empty", got)
	}
}

// The notice the print worker writes to stderr and the Telegram bridge reads
// back out of its combined output: one wording, parsed round-trip, and silent
// when nothing switched.
func TestModelSwitchNoticeRoundTrip(t *testing.T) {
	n := ModelSwitchNotice("execution", "opencode-free/big-pickle")
	if n != "[model switched: execution → opencode-free/big-pickle]" {
		t.Fatalf("notice = %q", n)
	}
	req, served, ok := ParseModelSwitchNotice("thinking...\n" + n + "\nthe answer")
	if !ok || req != "execution" || served != "opencode-free/big-pickle" {
		t.Fatalf("round trip = %q %q %v", req, served, ok)
	}
	if got := ModelSwitchNotice("execution", "execution"); got != "" {
		t.Fatalf("same model must stay silent, got %q", got)
	}
	if got := ModelSwitchNotice("execution", ""); got != "" {
		t.Fatalf("no divergence must stay silent, got %q", got)
	}
	if _, _, ok := ParseModelSwitchNotice("no notice here"); ok {
		t.Fatal("text without a notice must not parse")
	}
}
