package serve

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doReq calls a handler with an optional bearer token and JSON body.
func doReq(t *testing.T, h http.Handler, method, target, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	return doRawReq(t, h, method, target, token, rdr)
}

// doRawReq calls a handler with a raw body reader.
func doRawReq(t *testing.T, h http.Handler, method, target, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeBody decodes a JSON response.
func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"":                 true,
		"127.0.0.1:8765":   true,
		"localhost:4000":   true,
		"[::1]:9223":       true,
		"127.0.0.53:80":    true,
		"0.0.0.0:8765":     false,
		":8765":            false,
		"[::]:8765":        false,
		"192.168.1.7:80":   false,
		"broker.tailnet:1": false,
	}
	for addr, want := range cases {
		if got := loopbackAddr(addr); got != want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestNonLoopbackBindRequiresExplicitToken(t *testing.T) {
	constructors := map[string]func(dir, listen, token string) error{
		serviceBroker: func(dir, listen, token string) error {
			_, err := NewBroker(BrokerOptions{Options: Options{DataDir: dir, Listen: listen, Token: token}})
			return err
		},
		serviceGateway: func(dir, listen, token string) error {
			_, err := NewGateway(GatewayOptions{Options: Options{DataDir: dir, Listen: listen, Token: token}})
			return err
		},
		serviceRelay: func(dir, listen, token string) error {
			_, err := NewRelay(RelayOptions{Options: Options{DataDir: dir, Listen: listen, Token: token}})
			return err
		},
	}
	for service, build := range constructors {
		for _, listen := range []string{"0.0.0.0:0", ":0", "192.168.1.7:0"} {
			err := build(t.TempDir(), listen, "")
			if err == nil {
				t.Fatalf("%s on %s: bound without a token", service, listen)
			}
			if !strings.Contains(err.Error(), "explicit token") {
				t.Fatalf("%s on %s: err = %v, want it to name the missing token", service, listen, err)
			}
			if err := build(t.TempDir(), listen, "operator-token"); err != nil {
				t.Fatalf("%s on %s with a token: %v", service, listen, err)
			}
		}
		if err := build(t.TempDir(), "127.0.0.1:0", ""); err != nil {
			t.Fatalf("%s on loopback: %v", service, err)
		}
	}
}

func TestGeneratedTokenFileIsPrivateAndStable(t *testing.T) {
	dir := t.TempDir()
	b, err := NewBroker(BrokerOptions{Options: Options{DataDir: dir}})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	tok := b.Token()
	if len(tok) < 32 {
		t.Fatalf("generated token %q is too short", tok)
	}
	path := filepath.Join(dir, "serve", serviceBroker+".token")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("serve dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("serve dir mode = %v, want 0700", di.Mode().Perm())
	}
	if got := readToken(path); got != tok {
		t.Fatalf("token file = %q, want %q", got, tok)
	}

	// A restart must reuse the token, or every paired client breaks.
	again, err := NewBroker(BrokerOptions{Options: Options{DataDir: dir}})
	if err != nil {
		t.Fatalf("second NewBroker: %v", err)
	}
	if again.Token() != tok {
		t.Fatalf("token changed across instances: %q then %q", tok, again.Token())
	}

	if rec := doReq(t, b.Handler(), http.MethodGet, "/usage", tok, nil); rec.Code != http.StatusOK {
		t.Fatalf("generated token rejected: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, b.Handler(), http.MethodGet, "/usage", "wrong-token", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", rec.Code)
	}
}

func TestTokenFromEnvOverridesGeneratedFile(t *testing.T) {
	t.Setenv(tokenEnv, "env-token")
	dir := t.TempDir()
	b, err := NewBroker(BrokerOptions{Options: Options{DataDir: dir}})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	if b.Token() != "env-token" {
		t.Fatalf("token = %q, want the environment value", b.Token())
	}
	if _, err := os.Stat(filepath.Join(dir, "serve", serviceBroker+".token")); err == nil {
		t.Fatal("an environment token must not also mint a token file")
	}
	if rec := doReq(t, b.Handler(), http.MethodGet, "/usage", "env-token", nil); rec.Code != http.StatusOK {
		t.Fatalf("env token rejected: %d", rec.Code)
	}
}

func TestTokenFromQueryAndProxyAuthorization(t *testing.T) {
	dir := t.TempDir()
	rl, err := NewRelay(RelayOptions{Options: Options{DataDir: dir, Token: "relay-token"}})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if got := tokenFromRequest(req); got != "" {
		t.Fatalf("bare request token = %q", got)
	}
	req = httptest.NewRequest(http.MethodGet, "/?token=relay-token", nil)
	if got := tokenFromRequest(req); got != "relay-token" {
		t.Fatalf("query token = %q", got)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Proxy-Authorization", "Bearer relay-token")
	if got := tokenFromRequest(req); got != "relay-token" {
		t.Fatalf("proxy-authorization token = %q", got)
	}
	// The relay gates on the token before it upgrades anything.
	if rec := doReq(t, rl.Handler(), http.MethodGet, "/", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("relay without a token: %d, want 401", rec.Code)
	}
}

func TestDispatchRejectsUnknownService(t *testing.T) {
	if code := Dispatch(nil, "test"); code != 2 {
		t.Fatalf("no service: code = %d, want 2", code)
	}
	if code := Dispatch([]string{"nope"}, "test"); code != 2 {
		t.Fatalf("unknown service: code = %d, want 2", code)
	}
	if code := Dispatch([]string{"help"}, "test"); code != 0 {
		t.Fatalf("help: code = %d, want 0", code)
	}
}
