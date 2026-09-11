// Package oauth implements the browser authorization-code flow with PKCE
// (M9 #10): Claude Pro/Max and Codex log in this way, and the credential
// chain in internal/config already consumes the stored tokens.
//
// The flow never logs a code, verifier, or token: these are bearer
// credentials and one log line is a leak.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrDenied reports a user who closed the browser or clicked cancel.
var ErrDenied = errors.New("oauth: authorization denied by the user")

// Flow describes one provider's OAuth endpoints.
type Flow struct {
	AuthorizeURL string
	TokenURL     string
	ClientID     string
	// Scopes are requested verbatim.
	Scopes []string
	// RedirectPort pins the loopback callback port (0 = any free port).
	// Claude and Codex register specific redirect URIs, so this is data,
	// not a preference.
	RedirectPort int
	// ExtraAuthorizeParams carries provider-specific query (e.g. Codex's
	// code_challenge_method already set here; usercode flows add fields).
	ExtraAuthorizeParams map[string]string
	// ExtraTokenParams carries provider-specific form fields.
	ExtraTokenParams map[string]string
	// HTTPClient overrides the default token-exchange client (tests).
	HTTPClient *http.Client
	// CallbackTimeout bounds the browser round trip (default 5 minutes; a
	// human has to read a screen and click).
	CallbackTimeout time.Duration
}

// Tokens is a token-endpoint response, normalized.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"` // seconds
	Scope        string `json:"scope,omitempty"`
}

// ExpiresAtMillis converts the relative lifetime to an absolute stamp;
// zero means the server gave no expiry (treated as never-expiring).
func (t Tokens) ExpiresAtMillis(now time.Time) int64 {
	if t.ExpiresIn <= 0 {
		return 0
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second).UnixMilli()
}

// s256 is the PKCE challenge transform (RFC 7636 §4.2).
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newVerifier returns a RFC 7636 code_verifier (43–128 chars of the
// unreserved set, from crypto/rand).
func newVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// newState returns an unguessable CSRF token for the redirect.
func newState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthorizeURL builds the browser URL for one flow. Exported so tests pin
// the parameters, and so a caller can render the URL without running the
// whole flow.
func (f Flow) AuthorizeURLWith(verifier, state, redirectURI string) (string, error) {
	u, err := url.Parse(f.AuthorizeURL)
	if err != nil {
		return "", fmt.Errorf("oauth: authorize url: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", f.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", s256(verifier))
	q.Set("code_challenge_method", "S256")
	if len(f.Scopes) > 0 {
		q.Set("scope", strings.Join(f.Scopes, " "))
	}
	for k, v := range f.ExtraAuthorizeParams {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Result is a completed login.
type Result struct {
	Tokens      Tokens
	Account     string // email or subject, when the provider returned one
	RedirectURI string
}

// Run performs the loopback flow: bind a callback listener, hand the
// authorize URL to open (which must show or launch it), exchange the code,
// and return the tokens. ctx bounds the whole exchange.
func Run(ctx context.Context, f Flow, open func(url string) error) (*Result, error) {
	verifier, err := newVerifier()
	if err != nil {
		return nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, err
	}
	ln, port, err := listen(f.RedirectPort)
	if err != nil {
		return nil, err
	}
	defer ln.Close()

	redirect := fmt.Sprintf("http://localhost:%d/callback", port)
	authorize, err := f.AuthorizeURLWith(verifier, state, redirect)
	if err != nil {
		return nil, err
	}

	type codeResult struct {
		code string
		err  error
	}
	codes := make(chan codeResult, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if msg := q.Get("error"); msg != "" {
			writeDone(w, "Login "+msg+". You can close this tab.")
			codes <- codeResult{err: fmt.Errorf("%w: %s", ErrDenied, msg)}
			return
		}
		// State must match or the callback could be forged by another
		// local process or a confused-deputy redirect.
		if got := q.Get("state"); got != state {
			writeDone(w, "Login failed: state mismatch. Close this tab and retry.")
			codes <- codeResult{err: errors.New("oauth: state mismatch (possible CSRF); restart the login")}
			return
		}
		code := q.Get("code")
		if code == "" {
			writeDone(w, "Login failed: no code. Close this tab and retry.")
			codes <- codeResult{err: errors.New("oauth: callback carried no code")}
			return
		}
		writeDone(w, "Login complete. You can close this tab and return to xdev.")
		codes <- codeResult{code: code}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	if open != nil {
		if err := open(authorize); err != nil {
			return nil, fmt.Errorf("oauth: opening the browser: %w", err)
		}
	}

	timeout := f.CallbackTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case got := <-codes:
		if got.err != nil {
			return nil, got.err
		}
		tok, err := f.exchange(tctx, map[string]string{
			"grant_type":    "authorization_code",
			"code":          got.code,
			"code_verifier": verifier,
			"redirect_uri":  redirect,
		})
		if err != nil {
			return nil, err
		}
		return &Result{Tokens: tok, Account: accountFrom(tok), RedirectURI: redirect}, nil
	case <-tctx.Done():
		return nil, fmt.Errorf("oauth: no callback within %s (%w)", timeout, tctx.Err())
	}
}

// Refresh exchanges a refresh token (the credential chain calls this when a
// stored token is expired). A provider may rotate the refresh token, so
// both fields must be persisted from the result.
func (f Flow) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	return f.exchange(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     f.ClientID,
	})
}

func (f Flow) exchange(ctx context.Context, form map[string]string) (Tokens, error) {
	if strings.TrimSpace(f.TokenURL) == "" {
		return Tokens{}, errors.New("oauth: no token endpoint configured")
	}
	values := url.Values{}
	for k, v := range form {
		if v != "" {
			values.Set(k, v)
		}
	}
	for k, v := range f.ExtraTokenParams {
		values.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	hc := f.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	// Bounded read: a broken endpoint must not become an allocation.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Tokens{}, err
	}
	if resp.StatusCode != http.StatusOK {
		// The body may contain a token on some servers, so it is never
		// echoed: only the status and an error description.
		return Tokens{}, fmt.Errorf("oauth: token endpoint returned %d: %s", resp.StatusCode, redactBody(raw))
	}
	var tok Tokens
	if err := json.Unmarshal(raw, &tok); err != nil {
		return Tokens{}, fmt.Errorf("oauth: token response is not JSON (%d bytes)", len(raw))
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return Tokens{}, errors.New("oauth: token response carried no access_token")
	}
	return tok, nil
}

// accountFrom pulls a display identity out of the ID token without
// verifying it (this is a label, not an authorization decision).
func accountFrom(t Tokens) string {
	if t.IDToken == "" {
		return ""
	}
	parts := strings.Split(t.IDToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.Email != "" {
		return claims.Email
	}
	return claims.Sub
}

// redactBody keeps an error description while dropping anything that looks
// like a credential.
func redactBody(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	for _, key := range []string{"access_token", "refresh_token", "id_token", "token"} {
		if i := strings.Index(strings.ToLower(s), key+"\""); i >= 0 {
			s = s[:i] + `"redacted"`
			break
		}
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		s = "empty body"
	}
	return s
}

// listen binds a loopback listener on the pinned port, or any free port.
func listen(port int) (net.Listener, int, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, 0, fmt.Errorf("oauth: cannot bind %s (is another login in flight?): %w", addr, err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

func writeDone(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Escape nothing: msg is our own constant string, never user input.
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>xdev</title><body style=\"font-family:ui-monospace,monospace;padding:2rem\"><pre>%s</pre>", msg)
}
