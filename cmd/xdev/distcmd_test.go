package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/dist"
)

// distOpenProvider is the seam `xdev bench` measures through: it must
// resolve the same provider a normal run would, from models.yml, without
// duplicating the credential chain.
func TestDistOpenProviderResolvesModelsYml(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	t.Setenv("XDEV_MODEL", "")
	models := `providers:
  onegw:
    baseUrl: http://127.0.0.1:9/v1
    api: openai-completions
    auth: none
    models:
      - { id: free, name: Free, contextWindow: 1000000 }
defaultModel: onegw/free
`
	if err := os.WriteFile(filepath.Join(dir, "models.yml"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	// resolveModel falls back to settings.defaultModel: keep the session's
	// loaded settings out of the way for the explicit and cfg-default paths.
	prev := loadedSettings
	loadedSettings = nil
	t.Cleanup(func() { loadedSettings = prev })

	prov, model, err := distOpenProvider("")
	if err != nil {
		t.Fatalf("distOpenProvider(\"\"): %v", err)
	}
	if prov.Name() != "onegw" || model != "free" {
		t.Errorf("resolved %s/%s, want onegw/free", prov.Name(), model)
	}
	if _, model, err := distOpenProvider("onegw/free"); err != nil || model != "free" {
		t.Errorf("explicit ref: model=%q err=%v, want free", model, err)
	}
	if _, _, err := distOpenProvider("nope/model"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("unknown provider error = %v, want \"unknown provider\"", err)
	}
}

// The bench provider seam must honour the same model resolution a run uses —
// that is the point of routing it through resolveModel.
func TestDistOpenProviderResolvesModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	t.Setenv("XDEV_MODEL", "")
	models := `providers:
  onegw:
    baseUrl: http://127.0.0.1:9/v1
    api: openai-completions
    auth: none
    models:
      - { id: free, name: Free }
      - { id: small, name: Small }
`
	if err := os.WriteFile(filepath.Join(dir, "models.yml"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	prev := loadedSettings
	loadedSettings = settings
	t.Cleanup(func() { loadedSettings = prev })

	_, model, err := distOpenProvider("onegw/small")
	if err != nil {
		t.Fatalf("distOpenProvider(onegw/small): %v", err)
	}
	if model != "small" {
		t.Errorf("model = %q, want small", model)
	}
}

// The dispatch table must route every distribution subcommand and reject
// anything else, so a typo never silently becomes a print-mode prompt.
func TestDistRunDispatch(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", filepath.Join(t.TempDir(), "agent"))
	var out, errw strings.Builder
	if code := dist.Run("setup", nil, "0.1.0-test", nil, &out, &errw); code != 0 {
		t.Errorf("setup exit = %d, stderr = %s", code, errw.String())
	}
	if code := dist.Run("nope", nil, "0.1.0-test", nil, &out, &errw); code != 2 {
		t.Errorf("unknown subcommand exit = %d, want 2", code)
	}
}
