package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const gatewayTestToken = "gateway-test-token"

// recordedUpstream is a stub provider endpoint that records what it received.
type recordedUpstream struct {
	srv    *httptest.Server
	status int
	body   string

	mu    sync.Mutex
	last  http.Header
	path  string
	calls int
}

func newRecordedUpstream(t *testing.T) *recordedUpstream {
	t.Helper()
	u := &recordedUpstream{status: http.StatusOK, body: `{"ok":true}`}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.last = r.Header.Clone()
		u.path = r.URL.Path
		u.calls++
		status, body := u.status, u.body
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *recordedUpstream) host() string { return strings.TrimPrefix(u.srv.URL, "http://") }

func (u *recordedUpstream) headers() http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

func (u *recordedUpstream) requestPath() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path
}

// newTestGateway wires a broker holding one credential in front of a stub
// upstream, and returns the proxy URL a client should use.
func newTestGateway(t *testing.T, dir string, upstream *recordedUpstream, allow ...Upstream) (*Gateway, *httptest.Server) {
	t.Helper()
	b := newTestBroker(t, dir)
	if rec := doReq(t, b.Handler(), http.MethodPut, "/credentials/openai", brokerTestToken, map[string]string{"value": "sk-broker-secret"}); rec.Code != http.StatusNoContent {
		t.Fatalf("seeding broker: %d", rec.Code)
	}
	if rec := doReq(t, b.Handler(), http.MethodPut, "/credentials/anthropic", brokerTestToken, map[string]string{"value": "ak-broker-secret"}); rec.Code != http.StatusNoContent {
		t.Fatalf("seeding broker: %d", rec.Code)
	}
	brokerSrv := httptest.NewServer(b.Handler())
	t.Cleanup(brokerSrv.Close)

	if len(allow) == 0 {
		allow = []Upstream{{Scheme: "http", Host: upstream.host(), Provider: "openai"}}
	}
	g, err := NewGateway(GatewayOptions{
		Options:     Options{DataDir: dir, Token: gatewayTestToken},
		BrokerURL:   brokerSrv.URL,
		BrokerToken: brokerTestToken,
		Allow:       allow,
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gwSrv := httptest.NewServer(g.Handler())
	t.Cleanup(gwSrv.Close)
	return g, gwSrv
}

// proxyClient is an HTTP client that sends every request through gwURL.
func proxyClient(t *testing.T, gwURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(gwURL)
	if err != nil {
		t.Fatalf("parsing gateway URL: %v", err)
	}
	return &http.Client{
		Transport:     &http.Transport{Proxy: http.ProxyURL(u)},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestGatewayInjectsBrokerCredentialAndStripsClientAuth(t *testing.T) {
	dir := t.TempDir()
	upstream := newRecordedUpstream(t)
	gw, gwSrv := newTestGateway(t, dir, upstream)
	client := proxyClient(t, gwSrv.URL)

	req, err := http.NewRequest(http.MethodPost, upstream.srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The client authenticates to the gateway and tries to smuggle its own
	// credentials along.
	req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	req.Header.Set("X-Api-Key", "client-own-key")
	req.Header.Set("Cookie", "session=client")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}

	got := upstream.headers()
	if want := "Bearer sk-broker-secret"; got.Get("Authorization") != want {
		t.Fatalf("upstream Authorization = %q, want the broker credential %q", got.Get("Authorization"), want)
	}
	if v := got.Get("X-Api-Key"); v != "" {
		t.Fatalf("client X-Api-Key reached the upstream: %q", v)
	}
	if v := got.Get("Cookie"); v != "" {
		t.Fatalf("client Cookie reached the upstream: %q", v)
	}
	if got := upstream.requestPath(); got != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", got)
	}

	// A client that authenticates to the proxy the standard way
	// (Proxy-Authorization) is authorized too, and its own Authorization
	// still does not survive. Exercised on the handler directly: Go's client
	// transport manages Proxy-Authorization itself and will not let a caller
	// set it for a plain-HTTP proxy.
	req = httptest.NewRequest(http.MethodGet, upstream.srv.URL+"/v1/models", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+gatewayTestToken)
	req.Header.Set("Authorization", "Bearer client-own-key")
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Proxy-Authorization request: %d %s", rec.Code, rec.Body.String())
	}
	if got := upstream.headers().Get("Authorization"); got != "Bearer sk-broker-secret" {
		t.Fatalf("upstream Authorization = %q, want the broker credential", got)
	}

	// The upstream's own status and headers are relayed, not replaced.
	upstream.mu.Lock()
	upstream.status, upstream.body = http.StatusTooManyRequests, `{"error":"slow down"}`
	upstream.mu.Unlock()
	req, err = http.NewRequest(http.MethodGet, upstream.srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "slow down") {
		t.Fatalf("status = %d body = %s, want the upstream 429 relayed", resp.StatusCode, body)
	}
}

func TestGatewayHostHeaderRouting(t *testing.T) {
	dir := t.TempDir()
	upstream := newRecordedUpstream(t)
	_, gwSrv := newTestGateway(t, dir, upstream)

	// A client whose base URL is the gateway's address names the upstream in
	// the Host header instead of an absolute request URI.
	req, err := http.NewRequest(http.MethodPost, gwSrv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = upstream.host()
	req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	resp, err := gwSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := upstream.headers().Get("Authorization"); got != "Bearer sk-broker-secret" {
		t.Fatalf("upstream Authorization = %q, want the injected credential", got)
	}
	if got := upstream.requestPath(); got != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", got)
	}
}

func TestGatewayRefusesUnlistedHostsAndBadTokens(t *testing.T) {
	dir := t.TempDir()
	upstream := newRecordedUpstream(t)
	_, gwSrv := newTestGateway(t, dir, upstream)
	client := proxyClient(t, gwSrv.URL)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unlisted host: %d, want 403", resp.StatusCode)
	}

	// A second gateway with an empty allowlist proxies nothing, even for the
	// host the first one allowed.
	empty, err := NewGateway(GatewayOptions{
		Options:   Options{DataDir: t.TempDir(), Token: gatewayTestToken},
		BrokerURL: gwSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	emptySrv := httptest.NewServer(empty.Handler())
	defer emptySrv.Close()
	req, err = http.NewRequest(http.MethodGet, upstream.srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	resp, err = proxyClient(t, emptySrv.URL).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("empty allowlist: %d, want 403", resp.StatusCode)
	}

	// Credentials are checked before anything is forwarded.
	for _, token := range []string{"", "wrong-token"} {
		req, err = http.NewRequest(http.MethodGet, upstream.srv.URL+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err = client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: %d, want 401", token, resp.StatusCode)
		}
	}
	if upstream.headers() != nil && upstream.requestPath() != "" {
		t.Fatalf("a refused request still reached the upstream: %s", upstream.requestPath())
	}

	// Liveness stays unauthenticated.
	resp, err = gwSrv.Client().Get(gwSrv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}

	// The healthz bypass belongs to this service, not to an upstream path:
	// an unauthenticated absolute-form request for the upstream's /healthz is
	// still 401.
	req, err = http.NewRequest(http.MethodGet, upstream.srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxied upstream healthz: %d, want 401", resp.StatusCode)
	}
}

func TestGatewayCredentialHeaderShaping(t *testing.T) {
	dir := t.TempDir()
	upstream := newRecordedUpstream(t)
	cases := []struct {
		name    string
		up      Upstream
		header  string
		want    string
		notSend string
	}{
		{"authorization", Upstream{Host: "", Provider: "openai", Scheme: "http"}, "Authorization", "Bearer sk-broker-secret", ""},
		{"x-api-key", Upstream{Host: "", Provider: "anthropic", Scheme: "http", Header: "x-api-key"}, "x-api-key", "ak-broker-secret", "Authorization"},
		{"x-goog-api-key", Upstream{Host: "", Provider: "anthropic", Scheme: "http", Header: "x-goog-api-key"}, "x-goog-api-key", "ak-broker-secret", "Authorization"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.up.Host = upstream.host()
			_, gwSrv := newTestGateway(t, dir, upstream, tc.up)
			client := proxyClient(t, gwSrv.URL)
			req, err := http.NewRequest(http.MethodPost, upstream.srv.URL+"/v1/messages", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
			req.Header.Set("X-Api-Key", "client-own-key")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			got := upstream.headers()
			if got.Get(tc.header) != tc.want {
				t.Fatalf("%s = %q, want %q", tc.header, got.Get(tc.header), tc.want)
			}
			if tc.notSend != "" && got.Get(tc.notSend) != "" {
				t.Fatalf("%s = %q, want it unset", tc.notSend, got.Get(tc.notSend))
			}
		})
	}
}

func TestGatewayBrokerFailures(t *testing.T) {
	dir := t.TempDir()
	upstream := newRecordedUpstream(t)

	// A provider the broker has nothing for is a 502 naming the provider.
	_, gwSrv := newTestGateway(t, dir, upstream, Upstream{Scheme: "http", Host: upstream.host(), Provider: "missing"})
	req, err := http.NewRequest(http.MethodGet, upstream.srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	resp, err := proxyClient(t, gwSrv.URL).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("missing credential: %d %s, want 502", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "missing") {
		t.Fatalf("error body does not name the provider: %s", body)
	}
	if upstream.headers() != nil {
		t.Fatal("a request without a credential still reached the upstream")
	}

	// A broker that is not running is a 502, not a panic or a hang.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	b, err := NewGateway(GatewayOptions{
		Options:     Options{DataDir: dir, Token: gatewayTestToken},
		BrokerURL:   deadURL,
		BrokerToken: brokerTestToken,
		Allow:       []Upstream{{Scheme: "http", Host: upstream.host(), Provider: "openai"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deadSrv := httptest.NewServer(b.Handler())
	defer deadSrv.Close()
	req, err = http.NewRequest(http.MethodGet, upstream.srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	resp, err = proxyClient(t, deadSrv.URL).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "broker") {
		t.Fatalf("dead broker: %d %s, want a 502 naming the broker", resp.StatusCode, body)
	}
}

func TestGatewayBoundsRequestBody(t *testing.T) {
	dir := t.TempDir()
	upstream := newRecordedUpstream(t)
	b, err := NewGateway(GatewayOptions{
		Options:     Options{DataDir: dir, Token: gatewayTestToken},
		BrokerURL:   "http://127.0.0.1:1",
		BrokerToken: "x",
		Allow:       []Upstream{{Scheme: "http", Host: upstream.host(), Provider: "openai"}},
		MaxBody:     16,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, upstream.srv.URL+"/v1/messages", strings.NewReader(strings.Repeat("x", 64)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	resp, err := proxyClient(t, srv.URL).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d, want 413", resp.StatusCode)
	}
}

func TestGatewayHealthzAndTokenComeFromOptions(t *testing.T) {
	dir := t.TempDir()
	g, err := NewGateway(GatewayOptions{Options: Options{DataDir: dir, Token: "explicit", Version: "9"}})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	rec := doReq(t, g.Handler(), http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	health := decodeBody[healthPayload](t, rec)
	if health.Service != serviceGateway || health.Version != "9" {
		t.Fatalf("health = %+v", health)
	}
	// No allowlist and no token: the gateway still refuses every proxy call.
	rec = doReq(t, g.Handler(), http.MethodGet, "http://example.com/x", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", rec.Code)
	}
}

func TestParseUpstream(t *testing.T) {
	ok := map[string]Upstream{
		"api.anthropic.com=anthropic": {
			Host: "api.anthropic.com", Scheme: "https", Provider: "anthropic", Header: "Authorization",
		},
		"http://127.0.0.1:9000=local:x-api-key": {
			Host: "127.0.0.1:9000", Scheme: "http", Provider: "local", Header: "x-api-key",
		},
		"api.example.com=openai:Authorization": {
			Host: "api.example.com", Scheme: "https", Provider: "openai", Header: "Authorization",
		},
	}
	for spec, want := range ok {
		got, err := parseUpstream(spec)
		if err != nil {
			t.Fatalf("parseUpstream(%q): %v", spec, err)
		}
		if got != want {
			t.Fatalf("parseUpstream(%q) = %+v, want %+v", spec, got, want)
		}
	}
	for _, spec := range []string{
		"",
		"api.anthropic.com",
		"=anthropic",
		"api.anthropic.com=",
		"*.anthropic.com=anthropic",
		"anthropic.com=bad provider",
		"anthropic.com=anthropic:x-custom",
		"ftp://anthropic.com=anthropic",
	} {
		if got, err := parseUpstream(spec); err == nil {
			t.Fatalf("parseUpstream(%q) = %+v, want an error", spec, got)
		}
	}
}
