package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gateway is the credential-injecting forward proxy (issue #71).
//
// A client sends an unauthenticated request for an allowlisted upstream host;
// the gateway attaches the credential the broker holds for that host's
// provider, strips every credential the client may have attached, and
// forwards. The client never sees the secret, and a secret is only ever sent
// to a host the operator listed.
//
// Two request shapes reach an upstream:
//
//	absolute form   GET http://api.anthropic.com/v1/messages — what a client
//	                using HTTP_PROXY sends; r.URL.Host names the upstream.
//	Host header     r.URL.Host is empty, so the Host header names the
//	                upstream. This is how a client whose base URL points at
//	                http://127.0.0.1:4000 tells the gateway where to go.
//
// Anything else — an unlisted host, a client that just talks to the gateway's
// own address — is a 403, because a loopback proxy with an injected provider
// credential is an exfiltration tool if it accepts arbitrary destinations.
type Gateway struct {
	token       string
	brokerToken string
	version     string
	options     Options
	brokerURL   string
	allow       []Upstream
	timeout     time.Duration
	maxBody     int64
	client      *http.Client
	h           http.Handler
	logf        func(string, ...any)
}

// Defaults for the gateway.
const (
	defaultBrokerURL      = "http://127.0.0.1:8765"
	defaultGatewayTimeout = 5 * time.Minute
	// gatewayMaxBody bounds a proxied request body: a chat payload with a
	// transcript, not a file upload.
	gatewayMaxBody = 32 << 20
)

// Upstream maps one allowlisted host to a broker credential.
type Upstream struct {
	// Host is the upstream host[:port] as the client writes it.
	Host string
	// Scheme is "http" or "https"; empty means https.
	Scheme string
	// Provider is the broker credential key injected for this host.
	Provider string
	// Header selects the credential header: "" or "authorization" sends
	// "Authorization: Bearer <value>"; "x-api-key" and "x-goog-api-key" send
	// the value verbatim under that header.
	Header string
}

// GatewayOptions configures the proxy.
type GatewayOptions struct {
	Options
	// BrokerURL is the credential source (default http://127.0.0.1:8765).
	BrokerURL string
	// BrokerToken authenticates to the broker. Empty falls back to the
	// broker's stored token under the same data dir, which is the co-located
	// case this service is built for.
	BrokerToken string
	// Allow is the upstream allowlist. An empty list proxies nothing.
	Allow []Upstream
	// MaxBody bounds a proxied request body (default 32 MiB).
	MaxBody int64
	// Timeout bounds one proxied request end to end (default 5 minutes: a
	// long completion is slow, not hung).
	Timeout time.Duration
	// HTTPClient overrides the upstream client (tests).
	HTTPClient *http.Client
}

// NewGateway builds the proxy.
func NewGateway(opts GatewayOptions) (*Gateway, error) {
	token, err := resolveToken(serviceGateway, opts.Options)
	if err != nil {
		return nil, err
	}
	allow := make([]Upstream, 0, len(opts.Allow))
	for _, u := range opts.Allow {
		normalized, err := u.normalize()
		if err != nil {
			return nil, fmt.Errorf("gateway: %w", err)
		}
		allow = append(allow, normalized)
	}
	brokerToken := opts.BrokerToken
	if brokerToken == "" {
		brokerToken = loadToken(serviceBroker, opts.Options)
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			// A proxy must never chain through another proxy (HTTP_PROXY in
			// the environment would loop back into it) and must not follow
			// redirects: a redirect is the upstream's answer, not ours.
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          16,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	brokerURL := opts.BrokerURL
	if brokerURL == "" {
		brokerURL = defaultBrokerURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultGatewayTimeout
	}
	if opts.MaxBody <= 0 {
		opts.MaxBody = gatewayMaxBody
	}
	g := &Gateway{
		token:       token,
		brokerToken: brokerToken,
		version:     opts.Version,
		options:     opts.Options,
		brokerURL:   strings.TrimRight(brokerURL, "/"),
		allow:       allow,
		timeout:     timeout,
		maxBody:     opts.MaxBody,
		client:      client,
		logf:        opts.Options.logf,
	}
	g.h = requireToken(token, http.HandlerFunc(g.serve))
	return g, nil
}

// Handler serves the proxy.
func (g *Gateway) Handler() http.Handler { return g.h }

// Run serves until ctx is done.
func (g *Gateway) Run(ctx context.Context) error {
	g.logf("%s: %d allowlisted upstream(s), broker %s", serviceGateway, len(g.allow), g.brokerURL)
	return serveHTTP(ctx, serviceGateway, g.options.addr(defaultGatewayListen), g.h, g.logf)
}

// serve is the proxy entry point.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if isHealthz(r) {
		writeJSON(w, http.StatusOK, healthPayload{Status: "ok", Service: serviceGateway, Version: g.version})
		return
	}
	host := r.URL.Host
	if host == "" {
		host = r.Host
	}
	rule, ok := g.match(host)
	if !ok {
		httpError(w, http.StatusForbidden, "host %q is not an allowlisted upstream", host)
		return
	}
	if r.ContentLength > g.maxBody {
		httpError(w, http.StatusRequestEntityTooLarge, "request body exceeds %d bytes", g.maxBody)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.timeout)
	defer cancel()

	secret, err := g.credential(ctx, rule.Provider)
	if err != nil {
		httpError(w, http.StatusBadGateway, "%v", err)
		return
	}

	target := *r.URL
	target.Scheme = rule.Scheme
	target.Host = rule.Host
	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), http.MaxBytesReader(w, r.Body, g.maxBody))
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad upstream request: %v", err)
		return
	}
	copyProxyHeaders(req.Header, r.Header)
	stripAuth(req.Header)
	attachCredential(req.Header, rule, secret)
	req.ContentLength = r.ContentLength

	resp, err := g.client.Do(req)
	if err != nil {
		httpError(w, http.StatusBadGateway, "upstream %s: %v", rule.Host, err)
		return
	}
	defer resp.Body.Close()
	copyProxyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	streamBody(w, resp.Body)
}

// match resolves a request host to an allowlisted upstream. A rule written
// without a port also matches the host's default-port form.
func (g *Gateway) match(host string) (Upstream, bool) {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, u := range g.allow {
		rh := strings.ToLower(strings.TrimSuffix(u.Host, "."))
		if rh == h {
			return u, true
		}
		if !strings.Contains(rh, ":") && (h == rh+":443" || h == rh+":80") {
			return u, true
		}
	}
	return Upstream{}, false
}

// normalize validates one allowlist entry and fills its defaults.
func (u Upstream) normalize() (Upstream, error) {
	if u.Host == "" {
		return u, fmt.Errorf("upstream with an empty host")
	}
	if u.Host == "*" {
		// A wildcard would attach a provider credential to every host.
		return u, fmt.Errorf("upstream host %q: wildcards are not allowlistable", u.Host)
	}
	if strings.ContainsAny(u.Host, "/ \t*") {
		return u, fmt.Errorf("upstream host %q: want host[:port]", u.Host)
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return u, fmt.Errorf("upstream %s: scheme %q is not http or https", u.Host, u.Scheme)
	}
	if !validProvider(u.Provider) {
		return u, fmt.Errorf("upstream %s: invalid provider %q", u.Host, u.Provider)
	}
	switch strings.ToLower(u.Header) {
	case "", "authorization":
		u.Header = "Authorization"
	case "x-api-key":
		u.Header = "x-api-key"
	case "x-goog-api-key":
		u.Header = "x-goog-api-key"
	default:
		return u, fmt.Errorf("upstream %s: header %q is not supported", u.Host, u.Header)
	}
	return u, nil
}

// attachCredential writes the broker-held secret under the rule's header.
func attachCredential(h http.Header, u Upstream, secret string) {
	if u.Header == "Authorization" {
		h.Set("Authorization", "Bearer "+secret)
		return
	}
	h.Set(u.Header, secret)
}

// parseUpstream parses a --upstream spec: [scheme://]host=provider[:header].
func parseUpstream(spec string) (Upstream, error) {
	u := Upstream{}
	body := spec
	if i := strings.Index(body, "://"); i >= 0 {
		u.Scheme, body = body[:i], body[i+3:]
	}
	host, rest, ok := strings.Cut(body, "=")
	if !ok || host == "" || rest == "" {
		return Upstream{}, fmt.Errorf("--upstream %q: want [scheme://]host=provider[:header]", spec)
	}
	provider := rest
	if i := strings.Index(rest, ":"); i >= 0 {
		provider, u.Header = rest[:i], rest[i+1:]
	}
	u.Host, u.Provider = host, provider
	return u.normalize()
}

// credential fetches one provider's secret from the broker.
//
// ponytail: one loopback round trip per proxied request, no cache — a rotation
// on the broker takes effect on the next request. Add a short TTL if the
// broker ever moves off-host, where that round trip starts to cost.
func (g *Gateway) credential(ctx context.Context, provider string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.brokerURL+"/credentials/"+url.PathEscape(provider), nil)
	if err != nil {
		return "", err
	}
	if g.brokerToken != "" {
		req.Header.Set("Authorization", "Bearer "+g.brokerToken)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("broker unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := readErrorMessage(resp.Body)
		if resp.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("no credential stored for provider %q", provider)
		}
		return "", fmt.Errorf("broker returned %s: %s", resp.Status, msg)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxValueBytes+1024)).Decode(&out); err != nil {
		return "", fmt.Errorf("broker response: %w", err)
	}
	if out.Value == "" {
		return "", fmt.Errorf("broker returned an empty credential for %q", provider)
	}
	return out.Value, nil
}

// readErrorMessage extracts the {"error":...} body a service writes, bounded:
// an error body is small, and a hostile one must not be read into memory.
func readErrorMessage(body io.Reader) string {
	var out struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 8<<10)).Decode(&out); err != nil {
		return "(unreadable error body)"
	}
	return out.Error
}

// stripAuth removes every credential a client could have attached: the
// standard Authorization header, its proxy variant, API-key headers, and
// cookies. The client's own identity must not reach a provider on a request
// the gateway is about to authenticate with someone else's secret.
func stripAuth(h http.Header) {
	for _, name := range []string{
		"Authorization",
		"Proxy-Authorization",
		"X-Api-Key",
		"X-Goog-Api-Key",
		"Cookie",
		"Cookie2",
	} {
		h.Del(name)
	}
}

// hopHeaders are connection-scoped, owned by each hop rather than forwarded.
var hopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// copyProxyHeaders copies end-to-end headers, dropping hop-by-hop ones and
// the compression negotiation: the transport transparently decompresses what
// it asked for, and forwarding the client's Accept-Encoding would hand back a
// body this proxy cannot inspect the length of.
func copyProxyHeaders(dst, src http.Header) {
	for name, vals := range src {
		if hopHeaders[strings.ToLower(name)] || strings.EqualFold(name, "Accept-Encoding") {
			continue
		}
		for _, v := range vals {
			dst.Add(name, v)
		}
	}
}

// streamBody relays an upstream body, flushing after every chunk: an SSE
// completion has to reach the client as it arrives, not when it ends.
func streamBody(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
