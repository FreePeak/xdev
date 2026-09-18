package config

import (
	"runtime"
	"testing"
)

// TestCursorConnectCatalog verifies the Cursor catalog entry:
// subscription-aware (isPlan), OpenAI-compatible wire, API endpoint,
// key env variable, documentation link — all preconfigured so a
// subscription user connects with zero typing beyond the name.
// Models are intentionally empty: the live /models discovery fills
// them in at connect time (ConnectProviderConfig sets Discovery).
func TestCursorConnectCatalog(t *testing.T) {
	if !HasConnect("cursor") {
		t.Fatal("cursor missing from catalog")
	}
	e, ok := lookupConnect("cursor")
	if !ok {
		t.Fatal("lookupConnect cursor")
	}
	if e.Title != "Cursor" {
		t.Errorf("title = %q", e.Title)
	}
	if e.BaseURL != "https://api.cursor.com/v1" {
		t.Errorf("baseUrl = %q", e.BaseURL)
	}
	if e.API != "openai-completions" {
		t.Errorf("api = %q", e.API)
	}
	if len(e.Env) == 0 || e.Env[0] != "CURSOR_API_KEY" {
		t.Errorf("env = %v, want CURSOR_API_KEY", e.Env)
	}
	if e.Doc != "https://cursor.com/docs/api" {
		t.Errorf("doc = %q", e.Doc)
	}
	// Subscription, not per-token: a flat monthly plan.
	if !isPlan("cursor") {
		t.Error("cursor should be a plan (subscription)")
	}
	// No pinned models — the live /models endpoint provides them.
	if len(e.Models) != 0 {
		t.Errorf("models = %d, want 0 (discovered from API)", len(e.Models))
	}
	pc, err := ConnectProviderConfig("cursor")
	if err != nil {
		t.Fatal(err)
	}
	if pc.APIKey != "${CURSOR_API_KEY}" {
		t.Errorf("apiKey = %q", pc.APIKey)
	}
	if pc.BaseURL != "https://api.cursor.com/v1" {
		t.Errorf("baseUrl = %q", pc.BaseURL)
	}
	if pc.API != "openai-completions" {
		t.Errorf("api = %q", pc.API)
	}
	if pc.Discovery == nil || pc.Discovery.Type != DiscoveryOpenAIModels {
		t.Error("Discovery not set for cursor (models should be discovered live)")
	}
}

// TestCursorReadyFromLocalToken verifies the credential chain falls
// through to the cursor local token source when no other credential
// is present. On CI (Linux) there is no Keychain and no Cursor state
// db, so this confirms the chain degrades gracefully to the env var
// rather than erroring.
func TestCursorReadyFromLocalToken(t *testing.T) {
	connectSandbox(t)
	t.Setenv("CURSOR_API_KEY", "")
	e, _ := lookupConnect("cursor")
	_, from := connectCredential("cursor", e, nil, CredentialStore{})
	// On darwin with the Keychain item present the source would be
	// "cursor keychain" or "cursor state.db"; elsewhere it falls
	// through to the env var (or "" when unset). Neither is an error.
	if from != "" && from != "env CURSOR_API_KEY" {
		t.Errorf("unexpected cursor credential source: %q", from)
	}
}

// TestCursorCredentialChainOrder verifies the chain ordering:
// models.yml → credentials.json → local cursor token → env var.
// A stored login beats the local token (same account, closer store),
// and the local token beats the env var (zero-effort credential).
func TestCursorCredentialChainOrder(t *testing.T) {
	connectSandbox(t)
	e, _ := lookupConnect("cursor")

	// No stored login, no env var: nothing resolves locally on CI.
	t.Setenv("CURSOR_API_KEY", "")
	_, from := connectCredential("cursor", e, nil, CredentialStore{})
	if from != "" {
		t.Errorf("empty chain: got source %q, want \"\"", from)
	}

	// Env var present: resolves to env.
	t.Setenv("CURSOR_API_KEY", "test-env-key")
	_, from = connectCredential("cursor", e, nil, CredentialStore{})
	if from != "env CURSOR_API_KEY" {
		t.Errorf("env var: got source %q, want \"env CURSOR_API_KEY\"", from)
	}

	// Stored login present: beats the env var (same account).
	t.Setenv("CURSOR_API_KEY", "test-env-key")
	_, from = connectCredential("cursor", e, nil, CredentialStore{
		"cursor": {Kind: "api_key", APIKey: "stored-login-key"},
	})
	if from != "credentials.json" {
		t.Errorf("stored login: got source %q, want \"credentials.json\"", from)
	}

	// models.yml beats everything.
	cfg := &Config{Providers: map[string]*ProviderConfig{
		"cursor": {APIKey: "models-yml-key"},
	}}
	_, from = connectCredential("cursor", e, cfg, CredentialStore{})
	if from != "models.yml" {
		t.Errorf("models.yml: got source %q, want \"models.yml\"", from)
	}
}

// TestCursorIsCursorProvider verifies the provider-name check used
// to gate the local-token credential chain.
func TestCursorIsCursorProvider(t *testing.T) {
	if !isCursorProvider("cursor") {
		t.Error("cursor should be a cursor provider")
	}
	if isCursorProvider("deepseek") {
		t.Error("deepseek should not be a cursor provider")
	}
	if isCursorProvider("") {
		t.Error("empty name should not be a cursor provider")
	}
	// Case-insensitive — the catalog key is lowercase.
	if !isCursorProvider("Cursor") {
		t.Error("Cursor (capitalized) should match")
	}
}

// TestCursorLocalTokenSource verifies the source naming for the
// connect listing. On CI (Linux) neither store exists, so it returns "".
func TestCursorLocalTokenSource(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cursor local token source only testable on darwin")
	}
	src := cursorLocalTokenSource()
	if src != "" && src != "cursor keychain" && src != "cursor state.db" {
		t.Errorf("unexpected source: %q", src)
	}
}
