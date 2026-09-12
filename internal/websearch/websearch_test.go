package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// serve starts a test server and returns it with every recorded request
// captured through a channel-free slice (handlers run sequentially here).
func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// searcherFor wires a Searcher whose whole chain points at test servers.
func searcherFor(t *testing.T, timeout time.Duration, routes map[string]string) *Searcher {
	t.Helper()
	s := &Searcher{
		Providers: []string{"tavily", "brave", "duckduckgo"},
		APIKeys:   map[string]string{"tavily": "tvly-test", "brave": "brave-test"},
		Timeout:   timeout,
	}
	for name, url := range routes {
		switch name {
		case "tavily":
			s.tavilyURL = url
		case "brave":
			s.braveURL = url
		case "duckduckgo":
			s.ddgURL = url
		}
	}
	return s
}

// scan sets up a server for each named provider, handing the handler the
// request recorder so tests can assert on outbound requests.
func routes(t *testing.T, handlers map[string]http.HandlerFunc) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, h := range handlers {
		srv := serve(t, h)
		out[name] = srv.URL
	}
	return out
}

func TestQueryParsesGoogleOperators(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want query
	}{
		{
			name: "all classes",
			in:   `golang "generics" site:go.dev -site:blog.example after:2024-01-01 before:2024-06-01 filetype:pdf -bad OR foo`,
			want: query{
				terms:    []string{"golang", "-bad", "OR", "foo"},
				exact:    []string{"generics"},
				sites:    []string{"go.dev"},
				exclude:  []string{"blog.example"},
				after:    "2024-01-01",
				before:   "2024-06-01",
				filetype: "pdf",
			},
		},
		{
			name: "quoted operator is still an operator",
			in:   `"site:go.dev" cache`,
			want: query{terms: []string{"cache"}, sites: []string{"go.dev"}},
		},
		{
			name: "bare operator is a word",
			in:   `site:`,
			want: query{terms: []string{"site:"}},
		},
		{
			name: "unterminated quote runs to the end",
			in:   `unterminated "phrase`,
			want: query{terms: []string{"unterminated"}, exact: []string{"phrase"}},
		},
		{
			name: "unmodeled operators pass through",
			in:   `inurl:docs golang OR rust`,
			want: query{terms: []string{"inurl:docs", "golang", "OR", "rust"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseQuery(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseQuery(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestRelaxationLadderOrder pins the documented drop order by observing the
// sequence of queries a zero-result provider receives: exact → filetype →
// date → site.
func TestRelaxationLadderOrder(t *testing.T) {
	var got []string
	urls := routes(t, map[string]http.HandlerFunc{
		"duckduckgo": func(w http.ResponseWriter, r *http.Request) {
			q, _ := io.ReadAll(r.Body)
			got = append(got, formValue(string(q), "q"))
			io.WriteString(w, "<html><body>no results</body></html>")
		},
	})
	s := &Searcher{Providers: []string{"duckduckgo"}, Timeout: time.Second, ddgURL: urls["duckduckgo"]}

	ans := s.Search(context.Background(), `golang "generics" site:go.dev after:2024-01-01 filetype:pdf -bad`, 0)

	want := []string{
		`golang -bad "generics" site:go.dev after:2024-01-01 filetype:pdf`,
		`golang -bad site:go.dev after:2024-01-01 filetype:pdf`,
		`golang -bad site:go.dev after:2024-01-01`,
		`golang -bad site:go.dev`,
		`golang -bad`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("relaxation ladder\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(ans.Text, "duckduckgo: no results") {
		t.Fatalf("empty result must say so: %q", ans.Text)
	}
	if ans.Failed {
		t.Fatal("a provider that answered 'nothing' is not an infrastructure failure")
	}
	if relaxed, _ := ans.Details["relaxed"].([]string); len(relaxed) != 0 {
		t.Fatalf("nothing was relaxed away from a full query that stayed empty: %v", relaxed)
	}
}

// TestRelaxationIsReported: a rung that succeeds records exactly the classes
// dropped to get there.
func TestRelaxationIsReported(t *testing.T) {
	var n int
	urls := routes(t, map[string]http.HandlerFunc{
		"duckduckgo": func(w http.ResponseWriter, r *http.Request) {
			n++
			if n < 3 {
				io.WriteString(w, "<html><body>nothing</body></html>")
				return
			}
			io.WriteString(w, ddgPage("Result", "https://go.dev/x", "found it"))
		},
	})
	s := &Searcher{Providers: []string{"duckduckgo"}, Timeout: time.Second, ddgURL: urls["duckduckgo"]}

	ans := s.Search(context.Background(), `golang "generics" site:go.dev filetype:pdf`, 0)

	if !strings.Contains(ans.Text, "(relaxed: exact, filetype)") {
		t.Fatalf("relaxation missing from the answer: %q", ans.Text)
	}
	if relaxed, _ := ans.Details["relaxed"].([]string); !reflect.DeepEqual(relaxed, []string{"exact", "filetype"}) {
		t.Fatalf("details.relaxed = %v", ans.Details["relaxed"])
	}
	if !strings.Contains(ans.Text, "https://go.dev/x") {
		t.Fatalf("result row missing: %q", ans.Text)
	}
}

// TestTavilyAdapterParsesResponseAndSendsNativeSiteFilters checks the real
// request shape (Bearer auth, include/exclude_domains, site: removed from
// the query) and the real response shape.
func TestTavilyAdapterParsesResponseAndSendsNativeSiteFilters(t *testing.T) {
	var (
		auth string
		body map[string]any
	)
	urls := routes(t, map[string]http.HandlerFunc{
		"tavily": func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			json.NewDecoder(r.Body).Decode(&body)
			io.WriteString(w, `{"results":[
				{"title":"Go <b>docs</b>","url":"https://go.dev/doc","content":"text &amp; more"},
				{"title":"","url":"javascript:alert(1)","content":"dropped"}
			]}`)
		},
	})
	s := searcherFor(t, time.Second, urls)
	s.Providers = []string{"tavily"} // only the adapter under test: no real endpoints

	ans := s.Search(context.Background(), `golang "generics" site:go.dev -site:blog.example -bad`, 0)

	if auth != "Bearer tvly-test" {
		t.Fatalf("auth header = %q", auth)
	}
	if body["query"] != `golang -bad "generics"` {
		t.Fatalf("site: must not be folded into the query when sent natively: %v", body["query"])
	}
	if !reflect.DeepEqual(body["include_domains"], []any{"go.dev"}) {
		t.Fatalf("include_domains = %v", body["include_domains"])
	}
	if !reflect.DeepEqual(body["exclude_domains"], []any{"blog.example"}) {
		t.Fatalf("exclude_domains = %v", body["exclude_domains"])
	}
	if !strings.Contains(ans.Text, "1. Go docs\n   https://go.dev/doc\n   text & more") {
		t.Fatalf("row not sanitized/rendered: %q", ans.Text)
	}
	if strings.Contains(ans.Text, "javascript:") {
		t.Fatalf("unfetchable URL reached the model: %q", ans.Text)
	}
	if providers, _ := ans.Details["providers"].([]string); !reflect.DeepEqual(providers, []string{"tavily"}) {
		t.Fatalf("providers = %v", ans.Details["providers"])
	}
}

func TestBraveAdapterParsesResponse(t *testing.T) {
	var (
		token  string
		params map[string][]string
	)
	urls := routes(t, map[string]http.HandlerFunc{
		"brave": func(w http.ResponseWriter, r *http.Request) {
			token = r.Header.Get("X-Subscription-Token")
			params = r.URL.Query()
			io.WriteString(w, `{"web":{"results":[{"title":"T","url":"https://e.com","description":"a <b>b</b> c"}]}}`)
		},
	})
	s := searcherFor(t, time.Second, urls)
	s.Providers = []string{"brave"} // only the adapter under test: no real endpoints

	ans := s.Search(context.Background(), `golang site:go.dev`, 0)

	if token != "brave-test" {
		t.Fatalf("subscription token = %q", token)
	}
	if got := params["q"]; len(got) != 1 || got[0] != "golang site:go.dev" {
		t.Fatalf("q = %v (brave takes operators in the query)", params["q"])
	}
	if got := params["count"]; len(got) != 1 || got[0] != "10" {
		t.Fatalf("count = %v", params["count"])
	}
	if !strings.Contains(ans.Text, "\n   a b c") {
		t.Fatalf("markup survived the sanitizer: %q", ans.Text)
	}
}

// TestDDGAdapterParsesHTML: the no-JS page's real markup, the uddg redirect
// wrapper, entities — and the rule that no HTML reaches the model.
func TestDDGAdapterParsesHTML(t *testing.T) {
	var form string
	urls := routes(t, map[string]http.HandlerFunc{
		"duckduckgo": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			form = string(raw)
			io.WriteString(w, `<html><body>
				<div class="result">
					<h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%3Fa%3D1%26b%3D2&amp;rut=zz">Go &amp; Docs</a></h2>
					<a class="result__snippet" href="//duckduckgo.com/l/?uddg=x">A <b>great</b>&nbsp;language &#x27;guide&#x27;</a>
				</div>
			</body></html>`)
		},
	})
	s := searcherFor(t, time.Second, urls)
	s.Providers = []string{"duckduckgo"} // only the adapter under test: no real endpoints

	ans := s.Search(context.Background(), `golang "generics"`, 0)

	if got := formValue(form, "q"); got != `golang "generics"` {
		t.Fatalf("form q = %q", got)
	}
	if !strings.Contains(ans.Text, "1. Go & Docs\n   https://go.dev/doc?a=1&b=2\n   A great language 'guide'") {
		t.Fatalf("ddg row not decoded/rendered: %q", ans.Text)
	}
	if strings.Contains(ans.Text, "<") || strings.Contains(ans.Text, "&nbsp;") {
		t.Fatalf("HTML leaked into the model context: %q", ans.Text)
	}
	if providers, _ := ans.Details["providers"].([]string); !reflect.DeepEqual(providers, []string{"duckduckgo"}) {
		t.Fatalf("providers = %v", ans.Details["providers"])
	}
}

// TestChainFallsThroughOn500AndTimeout: a failing provider must not end the
// run — the failure is recorded and the next provider answers.
func TestChainFallsThroughOn500AndTimeout(t *testing.T) {
	urls := routes(t, map[string]http.HandlerFunc{
		"tavily": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"detail":"upstream exploded"}`)
		},
		"brave": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		},
		"duckduckgo": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, ddgPage("Late", "https://example.com/late", "still here"))
		},
	})
	s := searcherFor(t, 50*time.Millisecond, urls)

	ans := s.Search(context.Background(), "golang", 0)

	if !strings.Contains(ans.Text, "https://example.com/late") {
		t.Fatalf("chain did not fall through to the last provider: %q", ans.Text)
	}
	for _, want := range []string{
		`tavily: HTTP 500 Internal Server Error: {"detail":"upstream exploded"}`,
		"brave: timeout after 50ms",
	} {
		if !strings.Contains(ans.Text, want) {
			t.Fatalf("note %q missing from the footer: %q", want, ans.Text)
		}
	}
	if providers, _ := ans.Details["providers"].([]string); !reflect.DeepEqual(providers, []string{"duckduckgo"}) {
		t.Fatalf("providers = %v", ans.Details["providers"])
	}
}

// TestProviderWithoutKeyIsSkipped: no credential means no attempt at all.
func TestProviderWithoutKeyIsSkipped(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("BRAVE_API_KEY", "")
	var tavilyHits int
	urls := routes(t, map[string]http.HandlerFunc{
		"tavily": func(w http.ResponseWriter, r *http.Request) {
			tavilyHits++
			io.WriteString(w, `{"results":[]}`)
		},
		"duckduckgo": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, ddgPage("Keyless", "https://example.com/keyless", "ddg answered"))
		},
	})
	s := searcherFor(t, time.Second, urls)
	s.APIKeys = nil // no settings keys either

	ans := s.Search(context.Background(), "golang", 0)

	if tavilyHits != 0 {
		t.Fatalf("keyless provider was queried %d time(s)", tavilyHits)
	}
	if !strings.Contains(ans.Text, "tavily: skipped: no API key") {
		t.Fatalf("skip note missing: %q", ans.Text)
	}
	if !strings.Contains(ans.Text, "https://example.com/keyless") {
		t.Fatalf("keyless provider behind the skip did not answer: %q", ans.Text)
	}
}

// TestUnknownProviderIsSkipped: a typo in settings degrades to a note, not a
// dead tool.
func TestUnknownProviderIsSkipped(t *testing.T) {
	urls := routes(t, map[string]http.HandlerFunc{
		"duckduckgo": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, ddgPage("T", "https://e.com", "s"))
		},
	})
	s := &Searcher{Providers: []string{"perplexity", "duckduckgo"}, Timeout: time.Second, ddgURL: urls["duckduckgo"]}

	ans := s.Search(context.Background(), "golang", 0)

	if !strings.Contains(ans.Text, `perplexity: skipped: unknown provider "perplexity"`) {
		t.Fatalf("unknown provider note missing: %q", ans.Text)
	}
	if !strings.Contains(ans.Text, "https://e.com") {
		t.Fatalf("chain died on the unknown provider: %q", ans.Text)
	}
}

// TestAllProvidersFailIsAnError: nothing answered at all = failure, so the
// caller does not read "0 results" as "the web has nothing".
func TestAllProvidersFailIsAnError(t *testing.T) {
	urls := routes(t, map[string]http.HandlerFunc{
		"tavily": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"detail":{"error":"invalid api key"}}`)
		},
	})
	s := searcherFor(t, time.Second, urls)
	s.Providers = []string{"tavily"}

	ans := s.Search(context.Background(), "golang", 0)
	if !ans.Failed {
		t.Fatalf("expected failure, got %q", ans.Text)
	}
	if !strings.Contains(ans.Text, "401") {
		t.Fatalf("status missing: %q", ans.Text)
	}
}

// TestDDGBotChallengeIsAFailure: parsing the challenge page would report a
// silent zero, which is exactly the wrong answer.
func TestDDGBotChallengeIsAFailure(t *testing.T) {
	urls := routes(t, map[string]http.HandlerFunc{
		"duckduckgo": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `<html><body><div class="anomaly-modal">Unfortunately, bots use DuckDuckGo too.</div></body></html>`)
		},
	})
	s := &Searcher{Providers: []string{"duckduckgo"}, Timeout: time.Second, ddgURL: urls["duckduckgo"]}

	ans := s.Search(context.Background(), "golang", 0)

	if !ans.Failed || !strings.Contains(ans.Text, "bot challenge") {
		t.Fatalf("challenge page not reported as a failure: %q", ans.Text)
	}
}

// TestPartialAnswerFallsThroughToTheNextProvider: fewer rows than asked for
// is the other fallback trigger in the issue.
func TestPartialAnswerFallsThroughToTheNextProvider(t *testing.T) {
	urls := routes(t, map[string]http.HandlerFunc{
		"brave": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"web":{"results":[{"title":"One","url":"https://one.example","description":"first"}]}}`)
		},
		"duckduckgo": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, ddgPage("Two", "https://two.example", "second"))
		},
	})
	s := searcherFor(t, time.Second, urls)
	s.Providers = []string{"brave", "duckduckgo"}

	ans := s.Search(context.Background(), "golang", 2)

	providers, _ := ans.Details["providers"].([]string)
	if !reflect.DeepEqual(providers, []string{"brave", "duckduckgo"}) {
		t.Fatalf("a short answer must ask the next provider: %v", providers)
	}
	if !strings.Contains(ans.Text, "2 result(s)") {
		t.Fatalf("both rows expected: %q", ans.Text)
	}
}

func TestLimitAndTimeoutResolution(t *testing.T) {
	// The documented defaults: three-provider chain, 10s per provider, 10 rows.
	if !reflect.DeepEqual(DefaultProviders, []string{"tavily", "brave", "duckduckgo"}) {
		t.Fatalf("default chain = %v", DefaultProviders)
	}
	if DefaultTimeout != 10*time.Second || DefaultMaxResults != 10 {
		t.Fatalf("defaults = %v / %d", DefaultTimeout, DefaultMaxResults)
	}
	zero := New(Settings{})
	if got := zero.limit(0); got != DefaultMaxResults {
		t.Fatalf("default limit = %d", got)
	}
	if got := zero.Timeout; got != 0 {
		t.Fatalf("unset timeout must stay zero (chain applies DefaultTimeout): %v", got)
	}
	capped := New(Settings{Timeout: "250ms", MaxResults: 99})
	if capped.Timeout != 250*time.Millisecond {
		t.Fatalf("timeout parse = %v", capped.Timeout)
	}
	if got := capped.limit(0); got != MaxResultsCeiling {
		t.Fatalf("ceiling = %d", got)
	}
	if got := capped.limit(3); got != 3 {
		t.Fatalf("explicit limit ignored: %d", got)
	}
	// A malformed duration cannot arrive through config (merge validates it),
	// but must still not produce a timed-out-everything searcher.
	if bad := New(Settings{Timeout: "ten seconds"}); bad.Timeout != 0 {
		t.Fatalf("malformed duration accepted: %v", bad.Timeout)
	}
}

func TestSanitizeSnippet(t *testing.T) {
	long := strings.Repeat("word ", 200)
	cases := []struct{ name, in, want string }{
		{"tags", "a <b>bold</b> c", "a bold c"},
		{"entities", "a &amp; b &#x27;q&#x27;", "a & b 'q'"},
		{"escaped markup stays text", "&lt;script&gt;", "<script>"},
		{"whitespace", "line\nbreak\t x", "line break x"},
		{"nbsp", "a\u00a0b", "a b"},
		{"control runes", "a\x00b\x07c", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeSnippet(tc.in); got != tc.want {
				t.Fatalf("sanitizeSnippet(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	capped := sanitizeSnippet(long)
	if len(capped) > maxSnippet+len("…") {
		t.Fatalf("snippet not capped: %d bytes", len(capped))
	}
	if !strings.HasSuffix(capped, "…") || strings.HasSuffix(capped, " …") {
		t.Fatalf("cap must fall on a word boundary: %q", capped[len(capped)-20:])
	}
	if !utf8.ValidString(capped) {
		t.Fatal("cap produced invalid UTF-8")
	}
}

func TestSanitizeResultsDropsUnfetchableURLsAndCapsRows(t *testing.T) {
	in := []webResult{
		{Title: "bad", URL: "javascript:alert(1)"},
		{Title: "relative", URL: "/nope"},
		{Title: "ok", URL: "https://e.com/?a=1&amp;b=2", Snippet: "x"},
		{Title: "extra", URL: "https://f.com"},
	}
	got := sanitizeResults(in, 2)
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].URL != "https://e.com/?a=1&b=2" {
		t.Fatalf("url not cleaned: %q", got[0].URL)
	}
	if got[1].Title != "extra" {
		t.Fatalf("cap kept the wrong rows: %+v", got)
	}
	if blank := sanitizeResults([]webResult{{Title: "", URL: "https://e.com"}}, 5); blank[0].Title != "https://e.com" {
		t.Fatalf("untitled row must fall back to its URL: %+v", blank)
	}
}

// TestSearchRejectsFilterOnlyQuery: there is nothing to search for once every
// constraint is stripped.
func TestSearchRejectsFilterOnlyQuery(t *testing.T) {
	s := New(Settings{})
	if ans := s.Search(context.Background(), "site:go.dev", 0); !ans.Failed {
		t.Fatalf("filter-only query accepted: %q", ans.Text)
	}
	if ans := s.Search(context.Background(), "   ", 0); !ans.Failed {
		t.Fatalf("blank query accepted: %q", ans.Text)
	}
}

// --- helpers -------------------------------------------------------------

func ddgPage(title, url, snippet string) string {
	return fmt.Sprintf(`<html><body><div class="result"><h2 class="result__title">`+
		`<a rel="nofollow" class="result__a" href="%s">%s</a></h2>`+
		`<a class="result__snippet" href="x">%s</a></div></body></html>`, url, title, snippet)
}

func formValue(body, key string) string {
	for _, kv := range strings.Split(body, "&") {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == key {
			if dec, err := url.QueryUnescape(v); err == nil {
				return dec
			}
			return v
		}
	}
	return ""
}
