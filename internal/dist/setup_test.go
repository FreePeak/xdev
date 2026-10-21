package dist

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/config"
)

// setupDataDir points config.DataDir() at a scratch directory so setup never
// touches the developer's real ~/.xdev/agent.
func setupDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "agent")
	t.Setenv("XDEV_AGENT_DIR", dir)
	return dir
}

func runSetup(t *testing.T) (code int, stdout, stderr string) {
	t.Helper()
	var out, errw bytes.Buffer
	code = setupMain(nil, "0.1.0-test", &out, &errw)
	return code, out.String(), errw.String()
}

func TestSetupCreatesDataDirAndStarterConfig(t *testing.T) {
	dataDir := setupDataDir(t)
	code, out, errw := runSetup(t)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	for _, sub := range []string{"", "sessions", "agents", "commands", "themes"} {
		fi, err := os.Stat(filepath.Join(dataDir, sub))
		if err != nil || !fi.IsDir() {
			t.Errorf("data dir %q missing: %v", sub, err)
		}
	}
	cfgPath := filepath.Join(dataDir, "config.yml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("starter config.yml: %v", err)
	}
	if !strings.Contains(string(raw), "approvalMode: yolo") {
		t.Errorf("starter config.yml does not carry the shipped defaults:\n%s", raw)
	}
	if !strings.Contains(out, "wrote starter "+cfgPath) {
		t.Errorf("setup did not report the starter write:\n%s", out)
	}
	// The template is printed (not written) for a machine with no providers.
	if !strings.Contains(out, "defaultModel: xdev-server/free") {
		t.Errorf("setup did not print the models.yml template:\n%s", out)
	}
	if !strings.Contains(out, "Next steps") {
		t.Errorf("setup did not print next steps:\n%s", out)
	}
}

// The starter config is a file xdev must be able to load: an unknown key in
// it would make every later run fail with the config quarantined.
func TestSetupStarterConfigLoads(t *testing.T) {
	dataDir := setupDataDir(t)
	if code, _, errw := runSetup(t); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	s, err := config.LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("LoadSettings on the starter config: %v", err)
	}
	if s.Theme != "auto" || s.ApprovalMode != "yolo" || s.MaxTurns != 200 || s.Memory != "local" {
		t.Errorf("starter config resolved to %+v, want the shipped defaults", s)
	}
	if s.MemoryLimit != 1<<30 {
		t.Errorf("memoryLimit = %d, want %d", s.MemoryLimit, int64(1<<30))
	}
	if _, err := os.Stat(filepath.Join(dataDir, "config.yml")); err != nil {
		t.Fatal(err)
	}
}

func TestSetupIsIdempotent(t *testing.T) {
	dataDir := setupDataDir(t)
	if code, _, errw := runSetup(t); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	cfgPath := filepath.Join(dataDir, "config.yml")
	first, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// A user edit must survive a second run: setup writes only when absent.
	edited := "theme: grokday\n"
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errw := runSetup(t)
	if code != 0 {
		t.Fatalf("second run exit = %d, stderr = %s", code, errw)
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Errorf("second run rewrote the user's config:\n got %q\nwant %q", got, edited)
	}
	if !strings.Contains(out, "kept existing "+cfgPath) {
		t.Errorf("second run did not report the existing config:\n%s", out)
	}
	if strings.Contains(out, "wrote starter") {
		t.Errorf("second run reported writing a starter again:\n%s", out)
	}
	if len(first) == 0 {
		t.Error("first run wrote an empty starter config")
	}
}

func TestSetupReportsConfiguredProviders(t *testing.T) {
	dataDir := setupDataDir(t)
	models := "providers:\n  zeta:\n    api: openai-completions\n  alpha:\n    api: openai-completions\n"
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "models.yml"), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errw := runSetup(t)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	if !strings.Contains(out, "providers: alpha, zeta") {
		t.Errorf("setup did not list the configured providers:\n%s", out)
	}
	if strings.Contains(out, "defaultModel: xdev-server/free") {
		t.Errorf("setup printed the template even though models.yml exists:\n%s", out)
	}
}

func TestSetupRejectsArguments(t *testing.T) {
	setupDataDir(t)
	var out, errw bytes.Buffer
	if code := setupMain([]string{"--yes"}, "0.1.0-test", &out, &errw); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "usage: xdev setup") {
		t.Errorf("stderr:\n%s", errw.String())
	}
}

// TestStarterConfigDoesNotPromiseACapItCannotDeliver: the starter file is
// written into EVERY user's config, so a comment that promises "0 =
// unbounded" is a lie in everyone's home directory — the settings layer
// treats a 0 as unset, so the run gets the default 200 turns.
//
// The file must also carry the value the settings layer will actually
// honour: a 0 in the file is silently discarded, so a file that says 0
// says nothing.
func TestStarterConfigDoesNotPromiseACapItCannotDeliver(t *testing.T) {
	if strings.Contains(setupStarterConfig, "unbounded") {
		t.Fatalf("the starter config promises a cap the settings layer refuses to give:\n%s", setupStarterConfig)
	}
	var s config.Settings
	dec := yaml.NewDecoder(strings.NewReader(setupStarterConfig))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.MaxTurns == 0 {
		t.Fatalf("the starter config writes maxTurns: 0, which the settings layer discards:\n%s", setupStarterConfig)
	}
	// And the resolved value is the shipped default, not something the file
	// silently changed.
	_ = setupDataDir(t)
	if code, _, errw := runSetup(t); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	resolved, err := config.LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.MaxTurns != 200 {
		t.Fatalf("the starter config resolves to %d turns, want the default 200", resolved.MaxTurns)
	}
}
