package serve

import (
	"bytes"
	"encoding/json"
	"github.com/FreePeak/xdev/internal/config"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const brokerTestToken = "vault-test-token"

func newTestBroker(t *testing.T, dir string) *Broker {
	t.Helper()
	// The broker no longer mints its own install id (#102) — it shares
	// config.InstallID — so a test that wants the id under `dir` must point
	// the install dir there, and must clear the process cache the previous
	// test filled.
	t.Setenv("XDEV_AGENT_DIR", dir)
	config.ResetInstallIDCacheForTests()
	t.Cleanup(config.ResetInstallIDCacheForTests)
	b, err := NewBroker(BrokerOptions{Options: Options{DataDir: dir, Token: brokerTestToken, Version: "test"}})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	return b
}

func TestBrokerCredentialCRUD(t *testing.T) {
	dir := t.TempDir()
	b := newTestBroker(t, dir)
	h := b.Handler()

	// Unauthenticated access never reaches a credential.
	if rec := doReq(t, h, http.MethodGet, "/credentials/openai", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read: %d, want 401", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/credentials/openai", "wrong", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/credentials/openai", brokerTestToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing credential: %d, want 404", rec.Code)
	}

	rec := doReq(t, h, http.MethodPut, "/credentials/openai", brokerTestToken, map[string]string{"value": "sk-live-abc123"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d %s, want 204", rec.Code, rec.Body.String())
	}

	rec = doReq(t, h, http.MethodGet, "/credentials/openai", brokerTestToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeBody[credentialResponse](t, rec)
	if got.Provider != "openai" || got.Value != "sk-live-abc123" {
		t.Fatalf("GET returned %+v", got)
	}
	if _, err := time.Parse(time.RFC3339, got.UpdatedAt); err != nil {
		t.Fatalf("updated_at %q: %v", got.UpdatedAt, err)
	}

	// A replace is a replace, not an append.
	doReq(t, h, http.MethodPut, "/credentials/openai", brokerTestToken, map[string]string{"value": "sk-live-rotated"})
	if rec := doReq(t, h, http.MethodGet, "/credentials/openai", brokerTestToken, nil); decodeBody[credentialResponse](t, rec).Value != "sk-live-rotated" {
		t.Fatal("PUT did not replace the stored value")
	}

	if rec := doReq(t, h, http.MethodDelete, "/credentials/openai", brokerTestToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d, want 204", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/credentials/openai", brokerTestToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("after DELETE: %d, want 404", rec.Code)
	}
	if rec := doReq(t, h, http.MethodDelete, "/credentials/openai", brokerTestToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second DELETE: %d, want 404", rec.Code)
	}
}

func TestBrokerRejectsBadRequests(t *testing.T) {
	dir := t.TempDir()
	b := newTestBroker(t, dir)
	h := b.Handler()

	cases := []struct {
		name   string
		method string
		target string
		body   string
		want   int
	}{
		{"provider with a space", http.MethodPut, "/credentials/bad%20name", `{"value":"x"}`, http.StatusBadRequest},
		{"provider with a slash", http.MethodGet, "/credentials/a%2Fb", "", http.StatusBadRequest},
		{"empty value", http.MethodPut, "/credentials/openai", `{"value":"   "}`, http.StatusBadRequest},
		{"no value field", http.MethodPut, "/credentials/openai", `{}`, http.StatusBadRequest},
		{"trailing json", http.MethodPut, "/credentials/openai", `{"value":"a"}{"value":"b"}`, http.StatusBadRequest},
		{"not json", http.MethodPut, "/credentials/openai", `sk-plaintext`, http.StatusBadRequest},
		{"unknown method", http.MethodPatch, "/credentials/openai", `{}`, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRawReq(t, h, tc.method, tc.target, brokerTestToken, strings.NewReader(tc.body))
			if rec.Code != tc.want {
				t.Fatalf("%s %s: %d %s, want %d", tc.method, tc.target, rec.Code, rec.Body.String(), tc.want)
			}
		})
	}

	// A value over the per-value cap is refused, not truncated.
	big := map[string]string{"value": strings.Repeat("k", maxValueBytes+1)}
	if rec := doReq(t, h, http.MethodPut, "/credentials/openai", brokerTestToken, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized value: %d, want 413", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/credentials/openai", brokerTestToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("oversized value was stored: %d", rec.Code)
	}
}

func TestBrokerBoundRequestBody(t *testing.T) {
	dir := t.TempDir()
	b, err := NewBroker(BrokerOptions{
		Options: Options{DataDir: dir, Token: brokerTestToken},
		MaxBody: 128,
	})
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	rec := doRawReq(t, b.Handler(), http.MethodPut, "/credentials/openai", brokerTestToken,
		strings.NewReader(`{"value":"`+strings.Repeat("x", 512)+`"}`))
	if rec.Code < 400 {
		t.Fatalf("oversized body: %d, want a client error", rec.Code)
	}
	if rec := doReq(t, b.Handler(), http.MethodGet, "/credentials/openai", brokerTestToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("oversized body was stored: %d", rec.Code)
	}
}

func TestBrokerEncryptsValuesAtRest(t *testing.T) {
	dir := t.TempDir()
	b := newTestBroker(t, dir)
	const secret = "sk-live-must-not-appear-9f3a"
	if rec := doReq(t, b.Handler(), http.MethodPut, "/credentials/openai", brokerTestToken, map[string]string{"value": secret}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", rec.Code)
	}

	vaultPath := filepath.Join(dir, "serve", serviceBroker+".vault.json")
	keyPath := filepath.Join(dir, "serve", serviceBroker+".key")
	raw, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatalf("reading vault: %v", err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("the vault file contains the plaintext secret")
	}
	if !bytes.Contains(raw, []byte("openai")) {
		t.Fatalf("vault file lost the provider key: %s", raw)
	}
	for _, path := range []string{vaultPath, keyPath} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", path, fi.Mode().Perm())
		}
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading key: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key file is %d bytes, want a 32-byte AES-256 key", len(key))
	}

	// The key is at rest, not in the process: a second broker over the same
	// data dir reads the same value back.
	b2 := newTestBroker(t, dir)
	rec := doReq(t, b2.Handler(), http.MethodGet, "/credentials/openai", brokerTestToken, nil)
	if rec.Code != http.StatusOK || decodeBody[credentialResponse](t, rec).Value != secret {
		t.Fatalf("second instance: %d %s", rec.Code, rec.Body.String())
	}

	// The provider name is authenticated data: a ciphertext moved into
	// another provider's slot must not decrypt there.
	var doc vaultDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing vault: %v", err)
	}
	doc.Credentials["other"] = doc.Credentials["openai"]
	delete(doc.Credentials, "openai")
	moved, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultPath, moved, 0o600); err != nil {
		t.Fatal(err)
	}
	b3 := newTestBroker(t, dir)
	rec = doReq(t, b3.Handler(), http.MethodGet, "/credentials/other", brokerTestToken, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("moved ciphertext: %d, want 500", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(secret)) {
		t.Fatal("the failed open leaked the plaintext")
	}
}

func TestBrokerUsageReport(t *testing.T) {
	dir := t.TempDir()
	b := newTestBroker(t, dir)
	h := b.Handler()

	rec := doReq(t, h, http.MethodGet, "/usage", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated usage: %d, want 401", rec.Code)
	}

	entries := []map[string]any{
		{"provider": "openai", "model": "gpt-5", "input_tokens": 1200, "output_tokens": 300},
		{"provider": "anthropic", "model": "claude-sonnet-4", "input_tokens": 900, "output_tokens": 150},
	}
	for _, e := range entries {
		if rec := doReq(t, h, http.MethodPost, "/usage/observed", brokerTestToken, e); rec.Code != http.StatusAccepted {
			t.Fatalf("POST /usage/observed: %d %s, want 202", rec.Code, rec.Body.String())
		}
	}

	bad := []map[string]any{
		{"provider": "no such provider", "input_tokens": 1},
		{"provider": "", "input_tokens": 1},
		{"provider": "openai", "input_tokens": -1},
		{"provider": "openai", "output_tokens": -5},
		{"provider": "openai", "model": strings.Repeat("m", 129), "input_tokens": 1},
	}
	for i, e := range bad {
		if rec := doReq(t, h, http.MethodPost, "/usage/observed", brokerTestToken, e); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad observation %d (%v): %d, want 400", i, e, rec.Code)
		}
	}

	rec = doReq(t, h, http.MethodGet, "/usage", brokerTestToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /usage: %d %s", rec.Code, rec.Body.String())
	}
	report := decodeBody[UsageReport](t, rec)
	if !uuidRe.MatchString(report.InstallID) {
		t.Fatalf("install_id = %q, want a UUID", report.InstallID)
	}
	host, _ := os.Hostname()
	if report.Hostname != host {
		t.Fatalf("hostname = %q, want %q", report.Hostname, host)
	}
	if _, err := time.Parse(time.RFC3339, report.GeneratedAt); err != nil {
		t.Fatalf("generated_at %q: %v", report.GeneratedAt, err)
	}
	if len(report.Usage) != len(entries) {
		t.Fatalf("usage has %d entries, want %d: %+v", len(report.Usage), len(entries), report.Usage)
	}
	first := report.Usage[0]
	if first.Provider != "openai" || first.Model != "gpt-5" || first.InputTokens != 1200 || first.OutputTokens != 300 {
		t.Fatalf("entry = %+v", first)
	}
	observed, err := time.Parse(time.RFC3339, first.ObservedAt)
	if err != nil {
		t.Fatalf("observed_at %q: %v", first.ObservedAt, err)
	}
	if time.Since(observed) > time.Minute || time.Until(observed) > time.Minute {
		t.Fatalf("observed_at %v is not the server's current time", observed)
	}

	// The install id is per-install and survives a restart; usage history
	// survives too.
	idPath := filepath.Join(dir, "install-id")
	fi, err := os.Stat(idPath)
	if err != nil {
		t.Fatalf("install-id file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("install-id mode = %v, want 0600", fi.Mode().Perm())
	}
	idBytes, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(idBytes)) != report.InstallID {
		t.Fatalf("install-id file = %q, report = %q", idBytes, report.InstallID)
	}

	b2 := newTestBroker(t, dir)
	rec = doReq(t, b2.Handler(), http.MethodGet, "/usage", brokerTestToken, nil)
	again := decodeBody[UsageReport](t, rec)
	if again.InstallID != report.InstallID {
		t.Fatalf("install id changed across instances: %q then %q", report.InstallID, again.InstallID)
	}
	if len(again.Usage) != len(entries) {
		t.Fatalf("usage did not persist: %+v", again.Usage)
	}
}

func TestBrokerHealthzIsOpen(t *testing.T) {
	dir := t.TempDir()
	b := newTestBroker(t, dir)
	rec := doReq(t, b.Handler(), http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	health := decodeBody[healthPayload](t, rec)
	if health.Status != "ok" || health.Service != serviceBroker || health.Version != "test" {
		t.Fatalf("health = %+v", health)
	}
}
