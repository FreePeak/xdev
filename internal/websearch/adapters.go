package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The three adapters behind web_search. Each one is a request builder plus a
// decoder for that provider's real response shape; the chain, timeouts,
// relaxation, and sanitization live in websearch.go and are shared.

// Provider endpoints. Tests override the Searcher's endpoint fields, never
// these constants: a package-level var would leak between parallel tests.
const (
	tavilySearchURL = "https://api.tavily.com/search"
	braveSearchURL  = "https://api.search.brave.com/res/v1/web/search"
	ddgSearchURL    = "https://html.duckduckgo.com/html/"
)

// ddgUserAgent: the no-JS endpoint answers a browser UA and 403s Go's
// default. Nothing is forged beyond a user-agent string — no cookies, no JS
// execution, no relay.
const ddgUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// webRequest is one attempt handed to a provider.
type webRequest struct {
	// text is the query: free terms plus every active constraint the
	// provider cannot express as a structured field.
	text string
	// q carries the parsed query so a provider can read the constraints it
	// maps to native fields.
	q query
	// active is the constraint set this rung of the ladder still applies.
	active constraintSet
	limit  int
}

// webProvider is one search backend in the chain.
type webProvider interface {
	name() string
	// native names the constraint classes this provider expresses as
	// structured request fields; the rest are folded into the query text.
	native() constraintSet
	// deadline bounds one attempt (per-provider timeout).
	deadline() time.Duration
	search(ctx context.Context, req webRequest) ([]webResult, error)
}

// webBase is the shared transport plumbing of the HTTP adapters.
type webBase struct {
	url     string
	client  *http.Client
	timeout time.Duration
}

func (b webBase) deadline() time.Duration { return b.timeout }

// buildRequest renders one attempt for one provider.
func buildRequest(p webProvider, q query, active constraintSet, limit int) webRequest {
	return webRequest{text: q.text(active, p.native()), q: q, active: active, limit: limit}
}

// tavilyProvider queries api.tavily.com/search. Site filters are sent as
// structured include/exclude domain lists (Tavily's native form), so they are
// not folded into the query text.
type tavilyProvider struct {
	webBase
	key string
}

func (p *tavilyProvider) name() string { return "tavily" }

func (p *tavilyProvider) native() constraintSet { return constraintSet{clsSite: true} }

func (p *tavilyProvider) search(ctx context.Context, req webRequest) ([]webResult, error) {
	body := map[string]any{
		"query":               req.text,
		"max_results":         req.limit,
		"search_depth":        "basic",
		"include_answer":      false,
		"include_raw_content": false,
	}
	if req.active[clsSite] {
		if len(req.q.sites) > 0 {
			body["include_domains"] = req.q.sites
		}
		if len(req.q.exclude) > 0 {
			body["exclude_domains"] = req.q.exclude
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	// Bearer auth: the query-string form is deprecated and leaks keys into
	// proxy logs.
	hreq.Header.Set("Authorization", "Bearer "+p.key)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, httpStatusError(resp)
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	results := make([]webResult, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, webResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return results, nil
}

// braveProvider queries the Brave Search API. Brave takes Google-style
// operators in `q` but exposes no absolute-date field (freshness is relative
// only), so every constraint travels in the query text.
type braveProvider struct {
	webBase
	key string
}

func (p *braveProvider) name() string { return "brave" }

func (p *braveProvider) native() constraintSet { return constraintSet{} }

func (p *braveProvider) search(ctx context.Context, req webRequest) ([]webResult, error) {
	u, err := url.Parse(p.url)
	if err != nil {
		return nil, err
	}
	qs := u.Query()
	qs.Set("q", req.text)
	qs.Set("count", strconv.Itoa(min(req.limit, MaxResultsCeiling)))
	// Decorations wrap matched terms in <b>; the sanitizer would strip them
	// anyway, so ask for the plain text.
	qs.Set("text_decorations", "false")
	u.RawQuery = qs.Encode()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("X-Subscription-Token", p.key)
	hreq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, httpStatusError(resp)
	}
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	results := make([]webResult, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		results = append(results, webResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return results, nil
}

// ddgProvider scrapes the no-JS DuckDuckGo endpoint — the keyless floor of
// the chain, so web_search still answers when no API key is configured.
// It is HTML, hence the two rules here: decode the `uddg=` redirect wrapper
// into the real target, and never let markup reach the caller (the chain
// sanitizes; this parser only extracts).
type ddgProvider struct {
	webBase
}

func (p *ddgProvider) name() string { return "duckduckgo" }

func (p *ddgProvider) native() constraintSet { return constraintSet{} }

func (p *ddgProvider) search(ctx context.Context, req webRequest) ([]webResult, error) {
	form := url.Values{"q": {req.text}, "kl": {"wt-wt"}}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hreq.Header.Set("User-Agent", ddgUserAgent)
	hreq.Header.Set("Accept", "text/html")

	resp, err := p.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, httpStatusError(resp)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if bytes.Contains(page, []byte("anomaly-modal")) || bytes.Contains(page, []byte("bots use DuckDuckGo")) {
		// The endpoint's bot challenge (the live service returns HTTP 202 with
		// this page to non-browser clients): a real answer is impossible, and
		// parsing the challenge page would silently report zero results. Name
		// the fix, since this is the keyless provider.
		return nil, fmt.Errorf("blocked by bot challenge (DuckDuckGo rejects this host; set TAVILY_API_KEY or BRAVE_API_KEY for a keyed provider)")
	}
	return parseDDGHTML(string(page)), nil
}

// parseDDGHTML extracts results from the no-JS result page. The markup is
// stable: each hit is an <a class="result__a" href=…>title</a> followed by an
// optional <a class="result__snippet" …>snippet</a>.
func parseDDGHTML(page string) []webResult {
	var out []webResult
	chunks := strings.Split(page, `class="result__a"`)
	for _, chunk := range chunks[1:] {
		target := ddgTarget(attrValue(chunk, "href"))
		if target == "" {
			continue
		}
		title := textUntil(chunk, "</a>")
		snippet := ""
		if i := strings.Index(chunk, `class="result__snippet"`); i >= 0 {
			snippet = textUntil(chunk[i:], "</a>")
		}
		out = append(out, webResult{Title: title, URL: target, Snippet: snippet})
	}
	return out
}

// ddgTarget unwraps the /l/?uddg= redirect and keeps only http(s) targets.
func ddgTarget(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if i := strings.Index(strings.ToLower(href), "uddg="); i >= 0 {
		raw := href[i+len("uddg="):]
		if j := strings.IndexByte(raw, '&'); j >= 0 {
			raw = raw[:j]
		}
		if dec, err := url.QueryUnescape(raw); err == nil && dec != "" {
			href = dec
		}
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	if !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
		return ""
	}
	return href
}

// attrValue returns the first name="value" attribute value in s.
func attrValue(s, name string) string {
	i := strings.Index(s, name+"=\"")
	if i < 0 {
		return ""
	}
	rest := s[i+len(name)+2:]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// textUntil returns the text between the first '>' and the first end marker
// after it ("" when either is missing).
func textUntil(s, end string) string {
	i := strings.IndexByte(s, '>')
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	if j := strings.Index(rest, end); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// httpStatusError turns a non-2xx response into an error carrying a bounded
// excerpt of the body: providers explain 401/402/429 there, and "HTTP 401" on
// its own sends the user hunting for the wrong problem. The provider name is
// NOT included — the chain prefixes every note with it, and "tavily: tavily:
// HTTP 500" reads like a bug. The body is read fully so the connection can be
// reused.
func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := sanitizeSnippet(string(body))
	if len(msg) > 160 {
		msg = msg[:160]
	}
	if msg == "" {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return fmt.Errorf("HTTP %s: %s", resp.Status, msg)
}
