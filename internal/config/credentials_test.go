package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialChainOrder(t *testing.T) {
	pc := &ProviderConfig{APIKey: "from-models-yml", AuthHeader: "x-api-key"}
	store := CredentialStore{"onegw": {Kind: "api_key", APIKey: "from-login"}}
	t.Setenv("ONEGW_API_KEY", "from-env")

	req := CredentialRequest{Provider: "onegw", ProviderCfg: pc, Store: store}

	// CLI wins over everything.
	req.CLIKey = "from-cli"
	got, err := ResolveCredential(req)
	if err != nil || got.Value != "from-cli" || got.Source != "cli" {
		t.Fatalf("cli: %+v err=%v", got, err)
	}

	// Then models.yml, then stored login, then env.
	req.CLIKey = ""
	if got, _ := ResolveCredential(req); got.Value != "from-models-yml" {
		t.Fatalf("models.yml should win: %+v", got)
	}
	req.ProviderCfg = &ProviderConfig{}
	if got, _ := ResolveCredential(req); got.Value != "from-login" || got.Source != "login" {
		t.Fatalf("stored key should win next: %+v", got)
	}
	req.Store = CredentialStore{}
	got, err = ResolveCredential(req)
	if err != nil || got.Value != "from-env" || got.Source != "env:ONEGW_API_KEY" {
		t.Fatalf("env fallback: %+v err=%v", got, err)
	}
	// A per-model key overrides the provider-level one.
	perModel := &ProviderConfig{
		APIKey: "provider-level",
		Models: []ModelConfig{{ID: "dev", APIKey: "model-level"}},
	}
	got, _ = ResolveCredential(CredentialRequest{Provider: "p", ProviderCfg: perModel, Model: "dev"})
	if got.Value != "model-level" {
		t.Fatalf("per-model key ignored: %+v", got)
	}
	// The auth header follows the provider config.
	if hdr := got.Header; hdr != "Authorization" {
		t.Fatalf("default header = %q", hdr)
	}
	got, _ = ResolveCredential(CredentialRequest{Provider: "p", ProviderCfg: &ProviderConfig{APIKey: "k", AuthHeader: "x-api-key"}})
	if got.Header != "x-api-key" {
		t.Fatalf("custom header = %q", got.Header)
	}
}

func TestCredentialMissingIsAnExplicitError(t *testing.T) {
	t.Setenv("GHOST_API_KEY", "")
	t.Setenv("GHOST_KEY", "")
	_, err := ResolveCredential(CredentialRequest{Provider: "ghost"})
	if err == nil {
		t.Fatal("a missing credential must be reported, not returned empty")
	}
	for _, want := range []string{"-api-key", "models.yml", "/login", "GHOST_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name the fix %q: %v", want, err)
		}
	}
}

func TestOAuthRefreshPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(DataDir(), 0o755)

	expired := StoredCredential{
		Kind: "oauth", AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	}
	req := CredentialRequest{
		Provider: "claude", Store: CredentialStore{"claude": expired},
		Refresh: func(c StoredCredential) (StoredCredential, error) {
			if c.RefreshToken != "rt" {
				t.Errorf("refresh got no refresh token: %+v", c)
			}
			return StoredCredential{Kind: "oauth", AccessToken: "new", RefreshToken: "rt2", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, nil
		},
	}
	got, err := ResolveCredential(req)
	if err != nil || got.Value != "new" || got.Kind != "oauth" {
		t.Fatalf("refresh: %+v err=%v", got, err)
	}
	// The refreshed token is persisted so the next start does not re-prompt.
	store, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if store["claude"].AccessToken != "new" || store["claude"].RefreshToken != "rt2" {
		t.Fatalf("refreshed credential not stored: %+v", store["claude"])
	}

	// No refresh function + expired = a clear error, never an empty token.
	req.Refresh = nil
	if _, err := ResolveCredential(req); err == nil || !strings.Contains(err.Error(), "re-login") {
		t.Fatalf("expired-without-refresh must demand re-login, got %v", err)
	}

	// A failed refresh surfaces rather than falling through to env.
	req.Refresh = func(StoredCredential) (StoredCredential, error) { return StoredCredential{}, os.ErrPermission }
	if _, err := ResolveCredential(req); err == nil {
		t.Fatal("a failed refresh must not masquerade as no credential")
	}
}

func TestStoredCredentialIsPrivateAndRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.MkdirAll(DataDir(), 0o755)
	if err := SaveCredential("onegw", StoredCredential{APIKey: "sk-secret-123456"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(CredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("credentials file is group/world readable: %o", perm)
	}
	store, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if store["onegw"].APIKey != "sk-secret-123456" || store["onegw"].Kind != "api_key" {
		t.Fatalf("round trip = %+v", store["onegw"])
	}
	if err := DeleteCredential("onegw"); err != nil {
		t.Fatal(err)
	}
	if store, _ = LoadCredentials(); len(store) != 0 {
		t.Fatalf("delete did not remove the entry: %v", store.Providers())
	}
	// Deleting an absent entry is not an error (idempotent logout).
	if err := DeleteCredential("onegw"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestLoadCredentialsCorruptIsAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.MkdirAll(filepath.Dir(CredentialsPath()), 0o755)
	if err := os.WriteFile(CredentialsPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(); err == nil {
		t.Fatal("a corrupt credential store must error, not silently log everyone out")
	}
}

func TestRedactNeverLeaksTheSecret(t *testing.T) {
	secret := "sk-supersecret-value-99999"
	got := Redact(secret)
	if strings.Contains(got, "supersecret") {
		t.Fatalf("redaction leaked the middle: %q", got)
	}
	if got == "" {
		t.Fatal("redaction removed everything")
	}
	short := Redact("abc")
	if strings.Contains(short, "abc") {
		t.Fatalf("short secret not masked: %q", short)
	}
	res := ResolvedCredential{Value: secret, Source: "env:ONEGW_API_KEY"}
	if !strings.Contains(res.Redacted(), "env:ONEGW_API_KEY") {
		t.Fatalf("source missing from display: %q", res.Redacted())
	}
}
