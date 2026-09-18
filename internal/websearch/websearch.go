// Package websearch implements xdev's web_search engine (M13 #48,
// parity-tools-providers §D): a parsed Google-style query over an ORDERED
// provider chain with sequential fallback, a per-provider timeout, and
// constraint relaxation instead of an empty answer. Three adapters (Tavily,
// Brave, DuckDuckGo's no-JS endpoint) are the exercisable subset of omp's
// 22-provider zoo; a new one is a small type implementing webProvider plus a
// case in (*Searcher).provider.
//
// The engine lives in its own leaf package because internal/config imports
// internal/tool (through internal/agent): if the Settings type lived in
// config, config could not be imported here, and if it lived in tool, the
// wiring in cmd/xdev would need field-by-field mapping instead of handing
// over settings.WebSearch whole. This package imports nothing from xdev.
//
// Two invariants shape the code:
//   - search results are the least trustworthy text injected into the
//     model's context, so provider HTML never survives: tags are stripped,
//     entities decoded, whitespace collapsed, snippets capped, and URLs
//     restricted to http(s);
//   - every provider hop is bounded (per-attempt deadline, response-body
//     cap, result cap), so a hanging provider cannot eat the whole call.
package websearch

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Settings is the layered webSearch config block. Zero values mean "use the
// default", so a layer that mentions one key never resets the others. It
// lives here rather than in internal/config so both the config schema and
// this engine can name one type without an import cycle.
type Settings struct {
	// Providers is the ordered fallback chain; empty → DefaultProviders.
	Providers []string `yaml:"providers"`
	// APIKeys maps provider → key. A provider's environment variable
	// (TAVILY_API_KEY, BRAVE_API_KEY) wins when set, so a rotated key can
	// be exported without editing config.
	APIKeys map[string]string `yaml:"apiKeys"`
	// Timeout bounds ONE provider attempt as a duration string ("10s",
	// "500ms"); empty → DefaultTimeout. The chain as a whole may therefore
	// take len(providers) × timeout: a hung provider must not eat the
	// budget of the providers behind it.
	Timeout string `yaml:"timeout"`
	// MaxResults caps the rows handed to the model; 0 → DefaultMaxResults.
	MaxResults int `yaml:"maxResults"`
}

// DefaultProviders is the schema-default chain order — the
// providers listed when nothing is configured. It is NOT the
// runtime default: chain() skips keyed providers (tavily/brave)
// without a credential, so an unconfigured install actually
// runs the DuckDuckGo no-JS endpoint (runtimeDefaultProviders).
var DefaultProviders = []string{"tavily", "brave", "duckduckgo"}

// runtimeDefaultProviders is the chain the engine actually runs
// when nothing is configured. chain() skips keyed providers
// without a credential, leaving only duckduckgo here.
var runtimeDefaultProviders = []string{"duckduckgo"}

const (
	// DefaultTimeout bounds ONE provider attempt. Per-provider (not per-call)
	// is the point: the chain's tail must still get its turn when the head
	// hangs.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxResults bounds how many rows reach the model.
	DefaultMaxResults = 10
	// MaxResultsCeiling is the hard cap (Brave refuses count > 20).
	MaxResultsCeiling = 20
	// maxSnippet caps one sanitized snippet.
	maxSnippet = 400
	// maxResponse caps one provider response body.
	maxResponse = 4 << 20
)

// envKeys maps a provider to the env var holding its key.
var envKeys = map[string]string{
	"tavily": "TAVILY_API_KEY",
	"brave":  "BRAVE_API_KEY",
}

// Searcher runs queries over the configured chain. The zero value is a
// usable keyless searcher (DuckDuckGo only).
type Searcher struct {
	Providers  []string
	APIKeys    map[string]string
	Timeout    time.Duration
	MaxResults int
	// HTTPClient overrides the transport; nil → one client per attempt with
	// Timeout applied, so a body read cannot hang past the deadline either.
	HTTPClient *http.Client

	// Endpoints are test seams; empty → the provider's real URL. Fields, not
	// package vars: a var would leak between parallel tests.
	tavilyURL, braveURL, ddgURL string
}

// Answer is one completed search run: the model-facing text, the structured
// details persisted with the tool result, and whether every provider failed
// (as opposed to a legitimately empty result).
type Answer struct {
	Text    string
	Details map[string]any
	Failed  bool
}

// New builds a Searcher from a settings block.
func New(s Settings) *Searcher {
	se := &Searcher{
		Providers:  append([]string(nil), s.Providers...),
		APIKeys:    s.APIKeys,
		MaxResults: s.MaxResults,
		// A malformed duration cannot reach here through config (merge
		// validates it); on any other path the default is the safe fallback.
	}
	if d, err := time.ParseDuration(strings.TrimSpace(s.Timeout)); err == nil {
		se.Timeout = d
	}
	return se
}

// Search runs one query: constraints are honored where a provider supports
// them, relaxed in the documented order when nothing comes back, and the
// chain falls through to the next provider on failure or a short answer.
func (s *Searcher) Search(ctx context.Context, raw string, maxResults int) Answer {
	q := parseQuery(raw)
	if !q.hasText() {
		return Answer{
			Text:   "web_search: query is required (a site:/date: filter alone leaves nothing to search for)",
			Failed: true,
		}
	}
	limit := s.limit(maxResults)
	providers, notes := s.chain()

	var (
		rows     []webResult
		used     []string
		relaxed  []string
		answered int
	)
	for _, p := range providers {
		res, dropped, err := searchProvider(ctx, p, q, limit-len(rows))
		if err != nil {
			notes = append(notes, p.name()+": "+err.Error())
			continue
		}
		answered++ // the provider answered, even if it answered "nothing"
		if len(res) == 0 {
			notes = append(notes, p.name()+": no results")
			continue
		}
		used = append(used, p.name())
		if len(dropped) > 0 && len(relaxed) == 0 {
			relaxed = dropped
		}
		rows = append(rows, res...)
		// An answer short of the limit falls through to the next provider:
		// "empty OR partial" is the fallback trigger.
		if len(rows) >= limit {
			break
		}
	}
	return s.report(raw, q, rows, used, relaxed, notes, answered)
}

// limit resolves the row cap: argument → settings → default, ceilinged.
func (s *Searcher) limit(arg int) int {
	n := arg
	if n <= 0 {
		n = s.MaxResults
	}
	if n <= 0 {
		n = DefaultMaxResults
	}
	if n > MaxResultsCeiling {
		n = MaxResultsCeiling
	}
	return n
}

// apiKey resolves one provider's key: env first, then settings.
func (s *Searcher) apiKey(name string) string {
	if env := envKeys[name]; env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(s.APIKeys[name])
}

// chain resolves the ordered adapter list. Unusable entries (unknown name,
// missing key) are dropped with a note rather than failing the call — the
// whole point of a chain is to survive a provider that cannot run.
func (s *Searcher) chain() ([]webProvider, []string) {
	names := s.Providers
	if len(names) == 0 {
		names = runtimeDefaultProviders // keyed providers without a key are skipped below
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var (
		out   []webProvider
		notes []string
	)
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		p, err := s.provider(name, timeout)
		if err != nil {
			notes = append(notes, name+": "+err.Error())
			continue
		}
		out = append(out, p)
	}
	return out, notes
}

// provider builds one adapter. A keyed provider without a key is skipped: the
// request could only fail, and DuckDuckGo behind it needs no credential.
func (s *Searcher) provider(name string, timeout time.Duration) (webProvider, error) {
	base := func(url string) webBase {
		client := s.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: timeout}
		}
		return webBase{url: url, client: client, timeout: timeout}
	}
	switch name {
	case "tavily":
		key := s.apiKey("tavily")
		if key == "" {
			return nil, errors.New("skipped: no API key (set TAVILY_API_KEY or webSearch.apiKeys.tavily)")
		}
		return &tavilyProvider{webBase: base(orDefault(s.tavilyURL, tavilySearchURL)), key: key}, nil
	case "brave":
		key := s.apiKey("brave")
		if key == "" {
			return nil, errors.New("skipped: no API key (set BRAVE_API_KEY or webSearch.apiKeys.brave)")
		}
		return &braveProvider{webBase: base(orDefault(s.braveURL, braveSearchURL)), key: key}, nil
	case "duckduckgo", "ddg":
		return &ddgProvider{webBase: base(orDefault(s.ddgURL, ddgSearchURL))}, nil
	default:
		return nil, fmt.Errorf("skipped: unknown provider %q (want tavily|brave|duckduckgo)", name)
	}
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// webResult is one search hit after sanitization.
type webResult struct {
	Title   string
	URL     string
	Snippet string
}

// searchProvider runs one provider through the relaxation ladder: the full
// constraint set first, then one rung per relaxable class in relaxOrder,
// stopping at the first rung that returns anything. Each rung is one retry —
// a query using all four classes costs at most five requests, and only when
// the narrower query found nothing at all. The returned slice names the
// classes the winning rung dropped.
func searchProvider(ctx context.Context, p webProvider, q query, limit int) ([]webResult, []string, error) {
	for _, active := range q.ladder() {
		req := buildRequest(p, q, active, limit)
		if strings.TrimSpace(req.text) == "" {
			// Every constraint was relaxed away and no free text is left
			// (the query was a bare quoted phrase): nothing to ask for.
			return nil, nil, nil
		}
		res, err := attempt(ctx, p, req)
		if err != nil {
			return nil, nil, err
		}
		if res = sanitizeResults(res, limit); len(res) == 0 {
			continue
		}
		return res, q.dropped(active), nil
	}
	return nil, nil, nil
}

// attempt runs one provider request under its own deadline, so a hung
// provider cannot eat the next provider's budget. Failures are flattened to
// the short phrase the footer shows: a *url.Error's full text is 200 chars
// of transport noise for the model to read.
func attempt(ctx context.Context, p webProvider, req webRequest) ([]webResult, error) {
	timeout := p.deadline()
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := p.search(ctx, req)
	if err != nil {
		return nil, errors.New(providerErrText(err, ctx, timeout))
	}
	return res, nil
}

func providerErrText(err error, ctx context.Context, timeout time.Duration) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(),
		errors.Is(err, context.DeadlineExceeded):
		return "timeout after " + timeout.String()
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return "canceled"
	default:
		return err.Error()
	}
}

// report renders the answer plus the chain's bookkeeping. The footer is part
// of Text on purpose: a thin answer must not look like a complete one, so a
// skipped provider or a relaxed constraint is stated, not hidden in details.
func (s *Searcher) report(raw string, q query, rows []webResult, used, relaxed, notes []string, answered int) Answer {
	var b strings.Builder
	fmt.Fprintf(&b, "web_search: %d result(s) for %q", len(rows), raw)
	if len(used) > 0 {
		b.WriteString(" — " + strings.Join(used, ", "))
	}
	if len(relaxed) > 0 {
		b.WriteString(" (relaxed: " + strings.Join(relaxed, ", ") + ")")
	}
	for i, r := range rows {
		fmt.Fprintf(&b, "\n%d. %s\n   %s", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			b.WriteString("\n   " + r.Snippet)
		}
	}
	if len(notes) > 0 {
		b.WriteString("\nnotes:")
		for _, n := range notes {
			b.WriteString("\n- " + n)
		}
	}
	return Answer{
		Text: b.String(),
		Details: map[string]any{
			"query":     raw,
			"parsed":    q.details(),
			"providers": orEmpty(used),
			"results":   rowDetails(rows),
			"relaxed":   orEmpty(relaxed),
			"notes":     orEmpty(notes),
		},
		// No provider ever answered = a failure; a provider that answered
		// "nothing" is a legitimate empty result.
		Failed: len(rows) == 0 && answered == 0,
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func rowDetails(rows []webResult) []map[string]string {
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]string{"title": r.Title, "url": r.URL, "snippet": r.Snippet})
	}
	return out
}

// --- query parsing -------------------------------------------------------

// query is a parsed Google-style query.
type query struct {
	terms    []string // free text and -negatives: never relaxed
	exact    []string // "quoted phrases"
	sites    []string // site:x
	exclude  []string // -site:x
	after    string   // after:YYYY-MM-DD
	before   string   // before:YYYY-MM-DD
	filetype string   // filetype:pdf
}

// parseQuery splits a Google-style query into relaxable constraints and
// pass-through terms. Operators xdev does not model (OR, inurl:, …) stay in
// terms deliberately: the model typed them, and a backend that ignores one is
// a smaller lie than a harness that silently deletes it.
func parseQuery(raw string) query {
	var q query
	for _, tok := range tokenize(raw) {
		switch tok.kind {
		case tokExact:
			q.exact = append(q.exact, tok.value)
		case tokSite:
			q.sites = append(q.sites, tok.value)
		case tokExcludeSite:
			q.exclude = append(q.exclude, tok.value)
		case tokAfter:
			q.after = tok.value
		case tokBefore:
			q.before = tok.value
		case tokFiletype:
			q.filetype = tok.value
		default:
			q.terms = append(q.terms, tok.value)
		}
	}
	return q
}

// tokenKind classifies one query token.
type tokenKind int

const (
	tokTerm tokenKind = iota
	tokExact
	tokSite
	tokExcludeSite
	tokAfter
	tokBefore
	tokFiletype
)

type token struct {
	kind  tokenKind
	value string
}

// operators is checked in order, so "-site:" wins over "site:".
var operators = []struct {
	prefix string
	kind   tokenKind
}{
	{"-site:", tokExcludeSite},
	{"site:", tokSite},
	{"after:", tokAfter},
	{"before:", tokBefore},
	{"filetype:", tokFiletype},
}

// classify maps one token to its kind. A quoted span is an exact phrase
// unless the whole span is an operator ("site:example.com"), which keeps
// `-site:"quoted host"` from being read as an exact phrase plus noise.
func classify(tok string, quoted bool) token {
	low := strings.ToLower(tok)
	for _, op := range operators {
		if !strings.HasPrefix(low, op.prefix) {
			continue
		}
		if v := strings.TrimSpace(tok[len(op.prefix):]); v != "" {
			return token{kind: op.kind, value: v}
		}
		break // bare "site:" is just a word
	}
	if quoted {
		return token{kind: tokExact, value: tok}
	}
	return token{kind: tokTerm, value: tok}
}

// tokenize splits on whitespace, keeping quoted spans whole. An unterminated
// quote runs to the end of the input: a typo is not worth an error when the
// fix is obvious.
func tokenize(raw string) []token {
	var out []token
	rs := []rune(raw)
	for i := 0; i < len(rs); {
		if unicode.IsSpace(rs[i]) {
			i++
			continue
		}
		if rs[i] == '"' {
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			if v := strings.TrimSpace(string(rs[i+1 : j])); v != "" {
				out = append(out, classify(v, true))
			}
			i = j + 1
			continue
		}
		j := i
		for j < len(rs) && !unicode.IsSpace(rs[j]) {
			j++
		}
		if v := strings.TrimSpace(string(rs[i:j])); v != "" {
			out = append(out, classify(v, false))
		}
		i = j
	}
	return out
}

// constraintClass is one relaxable group of parsed constraints.
type constraintClass int

const (
	clsExact constraintClass = iota
	clsFiletype
	clsDate
	clsSite
)

func (c constraintClass) String() string {
	switch c {
	case clsExact:
		return "exact"
	case clsFiletype:
		return "filetype"
	case clsDate:
		return "date"
	default:
		return "site"
	}
}

// relaxOrder is the documented drop order: the exact-phrase wrapper is the
// cheapest to lose (the same words are still searched), then the filetype
// narrow, then the date window, and last the site filter — the strongest
// statement of intent, so the last thing abandoned.
var relaxOrder = []constraintClass{clsExact, clsFiletype, clsDate, clsSite}

// constraintSet is the set of classes an attempt still applies.
type constraintSet map[constraintClass]bool

func (s constraintSet) clone() constraintSet {
	out := make(constraintSet, len(s))
	for k := range s {
		out[k] = true
	}
	return out
}

// hasText reports whether anything searchable remains once every constraint
// is stripped — a filter-only query has no text to search for.
func (q query) hasText() bool { return len(q.terms) > 0 || len(q.exact) > 0 }

// present returns the classes this query actually uses.
func (q query) present() constraintSet {
	s := constraintSet{}
	if len(q.exact) > 0 {
		s[clsExact] = true
	}
	if q.filetype != "" {
		s[clsFiletype] = true
	}
	if q.after != "" || q.before != "" {
		s[clsDate] = true
	}
	if len(q.sites) > 0 || len(q.exclude) > 0 {
		s[clsSite] = true
	}
	return s
}

// ladder is the attempt sequence: full constraints, then one cumulative rung
// per class the query uses, in relaxOrder.
func (q query) ladder() []constraintSet {
	cur := q.present()
	out := []constraintSet{cur.clone()}
	for _, c := range relaxOrder {
		if !cur[c] {
			continue
		}
		next := cur.clone()
		delete(next, c)
		out = append(out, next)
		cur = next
	}
	return out
}

// dropped names the classes inactive in an attempt (relaxOrder order, so the
// report is deterministic).
func (q query) dropped(active constraintSet) []string {
	full := q.present()
	var out []string
	for _, c := range relaxOrder {
		if full[c] && !active[c] {
			out = append(out, c.String())
		}
	}
	return out
}

// text builds the provider query for one attempt: free terms plus every
// active constraint the provider cannot express as a structured field.
func (q query) text(active, native constraintSet) string {
	parts := append([]string(nil), q.terms...)
	if active[clsExact] {
		for _, e := range q.exact {
			parts = append(parts, `"`+e+`"`)
		}
	}
	if active[clsSite] && !native[clsSite] {
		for _, s := range q.sites {
			parts = append(parts, "site:"+s)
		}
		for _, s := range q.exclude {
			parts = append(parts, "-site:"+s)
		}
	}
	if active[clsDate] {
		if q.after != "" {
			parts = append(parts, "after:"+q.after)
		}
		if q.before != "" {
			parts = append(parts, "before:"+q.before)
		}
	}
	if active[clsFiletype] && q.filetype != "" {
		parts = append(parts, "filetype:"+q.filetype)
	}
	return strings.Join(parts, " ")
}

// details is the parsed query in machine-readable form.
func (q query) details() map[string]any {
	d := map[string]any{}
	if len(q.terms) > 0 {
		d["terms"] = q.terms
	}
	if len(q.exact) > 0 {
		d["exact"] = q.exact
	}
	if len(q.sites) > 0 {
		d["sites"] = q.sites
	}
	if len(q.exclude) > 0 {
		d["excludeSites"] = q.exclude
	}
	if q.after != "" {
		d["after"] = q.after
	}
	if q.before != "" {
		d["before"] = q.before
	}
	if q.filetype != "" {
		d["filetype"] = q.filetype
	}
	return d
}

// --- sanitization --------------------------------------------------------

var (
	htmlTagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	whitespaceRe = regexp.MustCompile(`\s+`)
)

// sanitizeSnippet makes provider text safe to paste into the model context.
// Tags are stripped FIRST so an entity like &lt;img&gt; cannot be decoded
// into markup after the stripper ran; then entities are decoded, whitespace
// collapsed, control runes dropped, and the result capped at a word boundary
// (a mid-rune cut would inject invalid UTF-8).
func sanitizeSnippet(s string) string {
	if s == "" {
		return ""
	}
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	// \s is ASCII-only in Go, and DuckDuckGo snippets are full of &nbsp;:
	// collapse those too or the transcript gets stray non-breaking spaces.
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " "))
	s = stripControl(s)
	if len(s) <= maxSnippet {
		return s
	}
	cut := s[:maxSnippet]
	if i := strings.LastIndexByte(cut, ' '); i > maxSnippet/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// stripControl drops C0/C1 control runes that would garble the transcript.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// sanitizeURL keeps only something the model can safely open: an absolute
// http(s) URL. Providers occasionally emit javascript:/data: redirect shells.
func sanitizeURL(raw string) string {
	s := strings.TrimSpace(html.UnescapeString(htmlTagRe.ReplaceAllString(raw, "")))
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return ""
	}
	return s
}

// sanitizeResults strips every row of provider markup, drops rows whose URL
// is not fetchable, and caps the row count.
func sanitizeResults(in []webResult, limit int) []webResult {
	if limit < 0 {
		limit = 0
	}
	out := make([]webResult, 0, min(len(in), limit))
	for _, r := range in {
		u := sanitizeURL(r.URL)
		if u == "" {
			continue
		}
		title := sanitizeSnippet(r.Title)
		if title == "" {
			title = u
		}
		out = append(out, webResult{Title: title, URL: u, Snippet: sanitizeSnippet(r.Snippet)})
		if len(out) >= limit {
			break
		}
	}
	return out
}
