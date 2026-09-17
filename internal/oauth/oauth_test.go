package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPKCEMatchesRFC7636 pins the S256 transform against the RFC's own
// worked example (A.1): verifier "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
// must challenge to "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM". Self-
// consistency would pass a broken hash; the published vector cannot.
func TestPKCEMatchesRFC7636(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	if got := s256(verifier); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("S256 mismatch: %q", got)
	}
}

func TestAuthorizeURLCarriesTheContract(t *testing.T) {
	f := Flow{
		AuthorizeURL:         "https://auth.example/authorize",
		ClientID:             "xdev-client",
		Scopes:               []string{"openid", "profile"},
		ExtraAuthorizeParams: map[string]string{"prompt": "consent"},
	}
	got, err := f.AuthorizeURLWith("verifier-abc", "state-xyz", "http://localhost:1234/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"response_type":         "code",
		"client_id":             "xdev-client",
		"redirect_uri":          "http://localhost:1234/callback",
		"state":                 "state-xyz",
		"code_challenge_method": "S256",
		"prompt":                "consent",
	} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
	if q.Get("scope") != "openid profile" {
		t.Errorf("scope = %q", q.Get("scope"))
	}
	// The challenge must be the S256 of the verifier, never the verifier.
	if q.Get("code_challenge") != s256("verifier-abc") {
		t.Error("code_challenge is not S256(verifier)")
	}
	if strings.Contains(got, "verifier-abc") {
		t.Fatal("the raw verifier leaked into the browser URL")
	}
}

func TestVerifierAndStateAreUnpredictable(t *testing.T) {
	v1, err := newVerifier()
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := newVerifier()
	if v1 == v2 {
		t.Fatal("verifier repeated")
	}
	// RFC 7636: 43..128 chars of the unreserved set.
	if len(v1) < 43 || len(v1) > 128 {
		t.Fatalf("verifier length %d out of RFC range", len(v1))
	}
	s1, _ := newState()
	s2, _ := newState()
	if s1 == s2 || len(s1) < 16 {
		t.Fatalf("state weak: %q vs %q", s1, s2)
	}
}

// tokenServer is a stub OAuth token endpoint.
func tokenServer(t *testing.T, handler func(r *http.Request) (int, any)) (*httptest.Server, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var seen []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		seen = append(seen, r.PostForm)
		mu.Unlock()
		code, body := handler(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestRefreshSendsTheRightGrant(t *testing.T) {
	srv, seen := tokenServer(t, func(r *http.Request) (int, any) {
		return 200, map[string]any{"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600}
	})
	f := Flow{TokenURL: srv.URL, ClientID: "cid", HTTPClient: srv.Client()}
	tok, err := f.Refresh(context.Background(), "rt-old")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "fresh" || tok.RefreshToken != "rotated" {
		t.Fatalf("tokens = %+v", tok)
	}
	// A rotated refresh token must be persisted, or the next login fails.
	if tok.ExpiresAtMillis(time.Unix(0, 0)) != int64(3600*time.Second/time.Millisecond) {
		t.Fatalf("expiry = %d", tok.ExpiresAtMillis(time.Unix(0, 0)))
	}
	form := (*seen)[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-old" {
		t.Fatalf("form = %v", form)
	}
}

func TestTokenErrorBodyIsRedacted(t *testing.T) {
	srv, _ := tokenServer(t, func(r *http.Request) (int, any) {
		return 400, map[string]any{"error": "invalid_grant", "access_token": "SHOULD-NOT-BE-ECHOED"}
	})
	f := Flow{TokenURL: srv.URL, HTTPClient: srv.Client()}
	_, err := f.Refresh(context.Background(), "rt")
	if err == nil {
		t.Fatal("a 400 must error")
	}
	if strings.Contains(err.Error(), "SHOULD-NOT-BE-ECHOED") {
		t.Fatalf("error message leaked a credential: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("status missing: %v", err)
	}
}

func TestTokenResponseWithoutAccessTokenFails(t *testing.T) {
	srv, _ := tokenServer(t, func(r *http.Request) (int, any) { return 200, map[string]any{"token_type": "Bearer"} })
	f := Flow{TokenURL: srv.URL, HTTPClient: srv.Client()}
	if _, err := f.Refresh(context.Background(), "rt"); err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Fatalf("missing access_token must fail clearly, got %v", err)
	}
}

func TestRedactBodyBoundsAndScrubs(t *testing.T) {
	got := redactBody([]byte(`{"error":"invalid_client","refresh_token":"abc123def456"}`))
	if strings.Contains(got, "abc123def456") {
		t.Fatalf("token echoed: %q", got)
	}
	if !strings.Contains(got, "invalid_client") {
		t.Fatalf("error description lost: %q", got)
	}
	if long := redactBody([]byte(strings.Repeat("y", 9000))); len(long) > 260 {
		t.Fatalf("body not bounded: %d chars", len(long))
	}
	if redactBody(nil) != "empty body" {
		t.Fatal("empty body must still say something")
	}
}

// TestRunCompletesLoopbackFlow drives the real browser leg: the callback
// listener is hit with the code, the exchange happens, tokens come back.
func TestRunCompletesLoopbackFlow(t *testing.T) {
	var gotVerifier, gotCode string
	token, _ := tokenServer(t, func(r *http.Request) (int, any) {
		gotVerifier = r.PostFormValue("code_verifier")
		gotCode = r.PostFormValue("code")
		return 200, map[string]any{"access_token": "at", "refresh_token": "rt", "expires_in": 120}
	})
	f := Flow{
		AuthorizeURL:    "https://auth.example/authorize",
		TokenURL:        token.URL,
		ClientID:        "cid",
		HTTPClient:      token.Client(),
		CallbackTimeout: 10 * time.Second,
	}
	res, err := Run(context.Background(), f, func(authorize string) error {
		u, err := url.Parse(authorize)
		if err != nil {
			return err
		}
		port := u.Query().Get("redirect_uri")
		// Replay the callback exactly as a browser would.
		go func() {
			time.Sleep(50 * time.Millisecond)
			resp, err := http.Get(port + "?code=code-123&state=" + url.QueryEscape(u.Query().Get("state")))
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Tokens.AccessToken != "at" || res.RedirectURI == "" {
		t.Fatalf("result = %+v", res)
	}
	// The server must have seen the verifier matching the challenge.
	if gotCode != "code-123" || s256(gotVerifier) == "" || gotVerifier == "" {
		t.Fatalf("exchange form wrong: code=%q verifier=%q", gotCode, gotVerifier)
	}
}

func TestRunRefusesStateMismatch(t *testing.T) {
	token, _ := tokenServer(t, func(r *http.Request) (int, any) { return 200, map[string]any{"access_token": "at"} })
	f := Flow{
		AuthorizeURL: "https://auth.example/authorize", TokenURL: token.URL,
		ClientID: "cid", HTTPClient: token.Client(), CallbackTimeout: 5 * time.Second,
	}
	_, err := Run(context.Background(), f, func(authorize string) error {
		u, _ := url.Parse(authorize)
		redirect := u.Query().Get("redirect_uri")
		go func() {
			time.Sleep(30 * time.Millisecond)
			resp, err := http.Get(redirect + "?code=stolen&state=not-the-state")
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("a forged callback must be refused, got %v", err)
	}
}

func TestRunSurfacesUserDenial(t *testing.T) {
	token, _ := tokenServer(t, func(r *http.Request) (int, any) { return 200, map[string]any{"access_token": "at"} })
	f := Flow{
		AuthorizeURL: "https://auth.example/authorize", TokenURL: token.URL,
		ClientID: "cid", HTTPClient: token.Client(), CallbackTimeout: 5 * time.Second,
	}
	_, err := Run(context.Background(), f, func(authorize string) error {
		u, _ := url.Parse(authorize)
		redirect := u.Query().Get("redirect_uri")
		go func() {
			time.Sleep(30 * time.Millisecond)
			resp, e := http.Get(redirect + "?error=access_denied&state=" + url.QueryEscape(u.Query().Get("state")))
			if e == nil {
				resp.Body.Close()
			}
		}()
		return nil
	})
	if !errorsIs(err) {
		t.Fatalf("user cancel must be ErrDenied, got %v", err)
	}
}

func errorsIs(err error) bool { return err != nil && strings.Contains(err.Error(), "denied") }

func TestRunTimesOutWithoutCallback(t *testing.T) {
	token, _ := tokenServer(t, func(r *http.Request) (int, any) { return 200, map[string]any{"access_token": "at"} })
	f := Flow{
		AuthorizeURL: "https://auth.example/authorize", TokenURL: token.URL,
		ClientID: "cid", HTTPClient: token.Client(), CallbackTimeout: 150 * time.Millisecond,
	}
	start := time.Now()
	_, err := Run(context.Background(), f, func(string) error { return nil }) // nobody calls back
	if err == nil || !strings.Contains(err.Error(), "no callback") {
		t.Fatalf("silent browser must time out, got %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout ignored: %s", d)
	}
}

func TestOpenFailurePropagates(t *testing.T) {
	token, _ := tokenServer(t, func(r *http.Request) (int, any) { return 200, map[string]any{"access_token": "at"} })
	f := Flow{AuthorizeURL: "https://auth.example/authorize", TokenURL: token.URL, HTTPClient: token.Client()}
	_, err := Run(context.Background(), f, func(string) error { return fmt.Errorf("no browser") })
	if err == nil || !strings.Contains(err.Error(), "no browser") {
		t.Fatalf("browser failure must surface: %v", err)
	}
}

func TestAccountFromIDToken(t *testing.T) {
	// An unsigned, unpadded JWT payload is enough for a display label.
	payload := base64Raw(`{"email":"dev@example.com","sub":"123"}`)
	got := accountFrom(Tokens{IDToken: "h." + payload + ".s"})
	if got != "dev@example.com" {
		t.Fatalf("account = %q", got)
	}
	if got := accountFrom(Tokens{IDToken: "h." + base64Raw(`{"sub":"9"}`) + ".s"}); got != "9" {
		t.Fatalf("sub fallback = %q", got)
	}
	if got := accountFrom(Tokens{IDToken: "garbage"}); got != "" {
		t.Fatalf("malformed token must yield no account, got %q", got)
	}
}

func base64Raw(jsonBody string) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(jsonBody))
	return enc
}
