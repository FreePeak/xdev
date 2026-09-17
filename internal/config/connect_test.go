// Tests for /connect: the catalog's shape, the credential routing, and the
// comment-preserving models.yml write. Each test points DataDir at a temp HOME
// so nothing touches the developer's own ~/.xdev/agent.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// connectSandbox isolates every file connect.go writes — models.yml and, for
// the --key path, credentials.json — behind a temp directory, and fails the test
// that forgets it. t.Setenv("XDEV_AGENT_DIR", …) is the strongest knob profile.go
// has: it collapses all three roots into one directory and skips the XDG record
// and the legacy default. (Isolating HOME instead is not enough on a developer
// machine that ran `xdev config init-xdg`: the XDG record lives under ~/.xdev,
// which a redirected HOME still resolves through installDir's own XDEV_AGENT_DIR
// fallthrough — a test that passed while writing to the real config.)
func connectSandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	t.Setenv("HOME", dir) // belt: anything resolving home the slow way lands here too
	t.Setenv("DEEPSEEK_API_KEY", "")
	return dir
}

// connectTestFile stages and returns the sandbox's models.yml. It insists on a
// sandbox rather than trusting each caller to remember one: the first version of
// this helper wrote to DataDir() unconditionally, and two tests that did not
// isolate destroyed the developer's own provider config.
func connectTestFile(t *testing.T, content string) string {
	t.Helper()
	dir := os.Getenv("XDEV_AGENT_DIR")
	if dir == "" {
		t.Fatal("connect test without connectSandbox: refusing to write a real data dir")
	}
	if DataDir() != dir {
		t.Fatalf("DataDir() = %s, sandbox is %s: the isolation knob is not the one connect uses", DataDir(), dir)
	}
	path := filepath.Join(dir, "models.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The catalog is the product: it must cover the hosts people pay for, and every
// row must be something the loader can actually build.
func TestConnectCatalogIsUsable(t *testing.T) {
	if len(connectCatalog) < 150 {
		t.Errorf("catalog has %d providers, want the models.dev set (>150)", len(connectCatalog))
	}
	for name, e := range connectCatalog {
		// https for a hosted account; plain http is allowed only to a local
		// server (LM Studio and friends), and a templated base is the host's own
		// config to fill in. Anything else is a typo in generated data.
		if !strings.HasPrefix(e.BaseURL, "https://") &&
			!strings.HasPrefix(e.BaseURL, "http://127.0.0.1:") &&
			!strings.HasPrefix(e.BaseURL, "http://localhost:") &&
			!strings.HasPrefix(e.BaseURL, "${") {
			t.Errorf("%s: baseUrl %q is neither https nor local", name, e.BaseURL)
		}
		if e.Title == "" {
			t.Errorf("%s: no display title", name)
		}
		if len(e.Models) == 0 {
			t.Errorf("%s: no pinned models", name)
		}
		if len(e.Models) > 12 {
			t.Errorf("%s: %d pinned models, want the capped newest set", name, len(e.Models))
		}
		pc, err := ConnectProviderConfig(name)
		if err != nil {
			t.Fatalf("%s: ConnectProviderConfig: %v", name, err)
		}
		// RegisterProvider is the gate a run goes through; if connect writes a
		// block it rejects, the provider is unreachable.
		if err := (&Config{Providers: map[string]*ProviderConfig{}}).RegisterProvider(name, pc); err != nil {
			t.Errorf("%s: RegisterProvider: %v", name, err)
		}
	}
}

// extraConnectCatalog is not generated; a regen of connect_catalog.go must
// not drop xdev-server, and the row must still RegisterProvider.
func TestExtraConnectCatalog(t *testing.T) {
	if !HasConnect("xdev-server") {
		t.Fatal("xdev-server missing from extra catalog")
	}
	e, ok := lookupConnect("xdev-server")
	if !ok {
		t.Fatal("lookupConnect xdev-server")
	}
	if e.BaseURL != "${XDEV_SERVER_URL}/v1" {
		t.Errorf("baseUrl = %q", e.BaseURL)
	}
	if len(e.Env) == 0 || e.Env[0] != "XDEV_SERVER_KEY" {
		t.Errorf("env = %v, want XDEV_SERVER_KEY", e.Env)
	}
	if ConnectDefaultRef("xdev-server") != "xdev-server/free" {
		t.Errorf("default = %q", ConnectDefaultRef("xdev-server"))
	}
	pc, err := ConnectProviderConfig("xdev-server")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Config{Providers: map[string]*ProviderConfig{}}).RegisterProvider("xdev-server", pc); err != nil {
		t.Errorf("RegisterProvider: %v", err)
	}
}

func TestConnectWritesProviderBlock(t *testing.T) {
	connectSandbox(t)
	path := connectTestFile(t, `# my hand-written config
providers:
  onegw:
    baseUrl: https://gateway.example/v1   # inline note
    api: openai-completions
    apiKey: ${ONEGW_KEY}
defaultModel: onegw/free
`)
	if _, err := Connect("deepseek", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"# my hand-written config",            // file comment survived
		"# inline note",                       // provider comment survived
		"baseUrl: https://gateway.example/v1", // sibling block untouched
		"apiKey: ${ONEGW_KEY}",                // ...including its reference
		"deepseek:",                           // new provider written
		"https://api.deepseek.com",            // ...with its endpoint
		"apiKey: ${DEEPSEEK_API_KEY}",         // ...as a reference, not a key
		"api: openai-completions",             // ...with the wire kind
		"discovery:",                          // ...and opt-in listing
		"defaultModel: onegw/free",            // ...and the default kept
	} {
		if !strings.Contains(got, want) {
			t.Errorf("models.yml missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sk-") {
		t.Errorf("a literal key landed in models.yml:\n%s", got)
	}
	// The written file must load, and the new provider must resolve.
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatalf("written models.yml does not load: %v\n%s", err, got)
	}
	pc := cfg.Providers["deepseek"]
	if pc == nil {
		t.Fatalf("deepseek not in the reloaded config")
	}
	if pc.APIKey != "" {
		t.Errorf("APIKey = %q, want empty while DEEPSEEK_API_KEY is unset", pc.APIKey)
	}
	if len(pc.Models) == 0 || pc.Models[0].ContextWindow <= 0 {
		t.Errorf("pinned models missing windows: %+v", pc.Models)
	}
	if pc.Discovery == nil {
		t.Errorf("discovery not written: a host's newer models would stay invisible")
	}
}

// Re-connecting must not double-write, and must not disturb the other rows.
func TestConnectIsIdempotent(t *testing.T) {
	connectSandbox(t)
	path := connectTestFile(t, "")
	for i := 0; i < 3; i++ {
		if _, err := Connect("deepseek", ""); err != nil {
			t.Fatalf("Connect round %d: %v", i, err)
		}
	}
	raw, _ := os.ReadFile(path)
	if n := strings.Count(string(raw), "deepseek:"); n != 1 {
		t.Errorf("deepseek appears %d times, want 1:\n%s", n, raw)
	}
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, raw)
	}
	if len(cfg.Providers) != 1 {
		t.Errorf("providers = %d, want 1", len(cfg.Providers))
	}
	// A second provider appends rather than replacing.
	if _, err := Connect("zai", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	cfg, err = LoadModels(path)
	if err != nil {
		t.Fatalf("reload 2: %v\n%s", err, raw)
	}
	if len(cfg.Providers) != 2 {
		t.Errorf("providers = %d, want 2:\n%s", len(cfg.Providers), raw)
	}
}

// A --key belongs in the 0600 store; models.yml keeps the reference.
func TestConnectKeyGoesToCredentialStore(t *testing.T) {
	connectSandbox(t)
	path := connectTestFile(t, "")
	if _, err := Connect("deepseek", "sk-secret-value"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "sk-secret-value") {
		t.Errorf("the key was written to models.yml (0644 territory):\n%s", raw)
	}
	store, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if store["deepseek"].APIKey != "sk-secret-value" {
		t.Errorf("stored credential = %+v", store["deepseek"])
	}
	if fi, err := os.Stat(CredentialsPath()); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("credentials.json mode = %v, want 0600", fi.Mode())
	}
}

func TestConnectUnknownProvider(t *testing.T) {
	connectSandbox(t)
	if _, err := Connect("not-a-provider", ""); err == nil {
		t.Fatal("unknown provider connected without error")
	} else if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("err = %v", err)
	}
}

// An unparseable models.yml must be reported, never overwritten: those bytes
// are the user's.
func TestUpsertRefusesUnparseableFile(t *testing.T) {
	connectSandbox(t)
	path := connectTestFile(t, "providers:\n  x: [unclosed\n")
	before, _ := os.ReadFile(path)
	if _, err := Connect("deepseek", ""); err == nil {
		t.Fatal("connect wrote over an unparseable file")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("the file was modified despite the error")
	}
}

func TestConnectOptionsState(t *testing.T) {
	connectSandbox(t)
	t.Setenv("DEEPSEEK_API_KEY", "from-env")
	path := connectTestFile(t, "providers: {}\n")
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := ConnectOptions(cfg, CredentialStore{})
	byName := map[string]ConnectOption{}
	for _, o := range opts {
		byName[o.Name] = o
	}
	if len(opts) != len(ConnectNames()) {
		t.Errorf("listing has %d rows, want %d", len(opts), len(ConnectNames()))
	}
	ds := byName["deepseek"]
	if !ds.Ready || ds.ReadyFrom != "env DEEPSEEK_API_KEY" {
		t.Errorf("deepseek = %+v, want ready from the environment", ds)
	}
	if ds.Env != "DEEPSEEK_API_KEY" {
		t.Errorf("deepseek Env = %q", ds.Env)
	}
	if byName["anthropic"].Ready {
		t.Errorf("anthropic reports ready without a credential")
	}
	if !strings.Contains(ConnectDefaultRef("deepseek"), "deepseek/") {
		t.Errorf("DefaultRef = %q", ConnectDefaultRef("deepseek"))
	}
	// Connecting flips Connected, through the real write.
	if _, err := Connect("deepseek", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	opts = ConnectOptions(cfg, CredentialStore{})
	for _, o := range opts {
		if o.Name == "deepseek" && !o.Connected {
			t.Errorf("deepseek not marked connected after Connect")
		}
	}
}

func TestSetModelsDefault(t *testing.T) {
	connectSandbox(t)
	path := connectTestFile(t, `# keep me
providers:
  a:
    baseUrl: https://a/v1
    api: openai-completions
defaultModel: a/old
`)
	if err := SetModelsDefault(path, "a/new"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	if !strings.Contains(got, "# keep me") {
		t.Errorf("comment lost:\n%s", got)
	}
	cfg, err := LoadModels(path)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, got)
	}
	if cfg.DefaultModel != "a/new" {
		t.Errorf("defaultModel = %q", cfg.DefaultModel)
	}
	// And on a file with no default yet.
	empty := connectTestFile(t, "providers: {}\n")
	if err := SetModelsDefault(empty, "b/m"); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadModels(empty)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModel != "b/m" {
		t.Errorf("defaultModel = %q", cfg.DefaultModel)
	}
}

// The write keeps the bytes it replaces: an atomic rename is still an
// overwrite, and models.yml carries comments nothing can regenerate.
func TestUpsertKeepsPreviousBytes(t *testing.T) {
	connectSandbox(t)
	path := connectTestFile(t, "# hand-written\nproviders: {}\n")
	if _, err := Connect("deepseek", ""); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("no backup of the replaced file: %v", err)
	}
	if !strings.Contains(string(old), "# hand-written") {
		t.Errorf("backup = %q", old)
	}
}
