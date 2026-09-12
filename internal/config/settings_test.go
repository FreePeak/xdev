package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSettingsLayerPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	writeFile(t, GlobalSettingsPath(), `
theme: groknight
approvalMode: write
maxTurns: 50
modelRoles:
  default: onegw/free
  smol: onegw/tiny
disabledProviders: [bedrock]
`)
	writeFile(t, projectSettingsPath(cwd), `
theme: grokday
modelRoles:
  smol: onegw/dev
`)
	overlay := writeFile(t, filepath.Join(t.TempDir(), "extra.yml"), `
maxTurns: 7
`)

	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	// Later layers win per key; earlier keys survive.
	if s.Theme != "grokday" {
		t.Errorf("project should win theme: %q", s.Theme)
	}
	if s.ApprovalMode != "write" {
		t.Errorf("global approvalMode should hold: %q", s.ApprovalMode)
	}
	if s.MaxTurns != 7 {
		t.Errorf("overlay should win maxTurns: %d", s.MaxTurns)
	}
	if s.ModelRoles["default"] != "onegw/free" || s.ModelRoles["smol"] != "onegw/dev" {
		t.Errorf("modelRoles must merge per key: %v", s.ModelRoles)
	}
	// Defaults still show through where nothing set them.
	if s.MemoryLimit != 100<<20 {
		t.Errorf("memoryLimit default lost: %d", s.MemoryLimit)
	}
}

func TestSettingsShowThinking(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	// Default: unset in every layer means on.
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.ShowThinkingOn() {
		t.Fatal("showThinking must default to on")
	}
	// Explicit false survives the layer merge (the zero-skip hazard a
	// plain bool could not express) and overrides the global layer.
	writeFile(t, GlobalSettingsPath(), "showThinking: false\n")
	writeFile(t, projectSettingsPath(cwd), "showThinking: true\n")
	s, err = LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.ShowThinkingOn() {
		t.Fatal("project layer must win showThinking: true")
	}
	writeFile(t, projectSettingsPath(cwd), "showThinking: false\n")
	s, err = LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ShowThinkingOn() {
		t.Fatal("explicit false must survive the merge")
	}
}

func TestSettingsUnknownKeyIsRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, GlobalSettingsPath(), "them: typo\n") // cspell:disable-line
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "them") {
		t.Fatalf("unknown key must be reported, got %v", err)
	}
}

func TestSettingsInvalidApprovalMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, GlobalSettingsPath(), "approvalMode: ask-me-nice\n")
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "approvalMode") {
		t.Fatalf("invalid enum must fail, got %v", err)
	}
}

// TestBrokenConfigIsPreserved pins the data-loss guard: an unparseable
// persistent file is moved aside under a .broken-* name, not deleted, and
// the load still fails so the user learns about it.
func TestBrokenConfigIsPreserved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeFile(t, GlobalSettingsPath(), "theme: [unclosed\n")
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil {
		t.Fatal("broken config must fail the load")
	}
	if !strings.Contains(err.Error(), "preserved as") {
		t.Fatalf("error must name the backup: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("original must be moved out of the load path")
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	found := false
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".broken-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no .broken-* backup in %v", ents)
	}
}

func TestSettingsAbsentFilesAreNotErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := LoadSettings(t.TempDir(), []string{"/nope/missing.yml"})
	if err != nil {
		t.Fatalf("missing layers must be skipped: %v", err)
	}
	if s.Theme != "auto" || s.ApprovalMode != "yolo" || s.MaxTurns != 200 {
		t.Fatalf("defaults lost: %+v", s)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	for _, kv := range [][2]string{
		{"theme", "grokday"},
		{"maxTurns", "42"},
		{"modelRoles.smol", "onegw/dev"},
		{"disabledProviders", "bedrock,vertex"},
	} {
		if err := Set(path, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	// Typed parsing: numbers are numbers, lists are lists, maps nest.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, `maxTurns: "42"`) {
		t.Fatalf("numbers must not be quoted:\n%s", body)
	}
	if !strings.Contains(body, "modelRoles:") || !strings.Contains(body, "smol: onegw/dev") {
		t.Fatalf("dotted key did not nest:\n%s", body)
	}
	// Load the written file as the only overlay (defaults still apply).
	s, err := LoadSettings(t.TempDir(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxTurns != 42 || len(s.DisabledProviders) != 2 {
		t.Fatalf("typed round-trip failed: %+v", s)
	}
	got, err := Get(path, "theme")
	if err != nil || got != "grokday" {
		t.Fatalf("Get = %q err=%v", got, err)
	}
	// A later Set preserves earlier keys.
	if err := Set(path, "approvalMode", "write"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(path, "theme"); got != "grokday" {
		t.Fatalf("Set clobbered other keys: %q", got)
	}
}

// TestSetRefusesUnparseableFile: editing a broken file must not overwrite
// the user's bytes with a guess.
func TestSetRefusesUnparseableFile(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "config.yml"), "theme: [oops\n")
	before, _ := os.ReadFile(path)
	if err := Set(path, "theme", "groknight"); err == nil {
		t.Fatal("Set must refuse an unparseable file")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("refusal must leave the file untouched")
	}
}

// TestSettingsPersonality pins the `personality` key: schema default,
// layer override, and rejection of a value outside the enum.
func TestSettingsPersonality(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Personality != "default" {
		t.Fatalf("schema default = %q, want default", s.Personality)
	}

	writeFile(t, projectSettingsPath(cwd), "personality: friendly\n")
	s, err = LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Personality != "friendly" {
		t.Fatalf("project layer = %q", s.Personality)
	}

	writeFile(t, projectSettingsPath(cwd), "personality: moody\n")
	if _, err := LoadSettings(cwd, nil); err == nil || !strings.Contains(err.Error(), "personality") {
		t.Fatalf("unknown preset must be rejected, got %v", err)
	}
}

// TestCompactionMethodOrderKey covers the compaction.methodOrder knob: the
// shipped default, the hand-written scalar form, and the list form
// `xdev config set` produces (a scalar-only field would reject the CLI's own
// output and quarantine the user's config file).
func TestCompactionMethodOrderKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionMethodOrder(); got != DefaultCompactionMethodOrder {
		t.Fatalf("default methodOrder = %q, want %q", got, DefaultCompactionMethodOrder)
	}

	scalar := writeFile(t, filepath.Join(t.TempDir(), "scalar.yml"),
		"compaction:\n  methodOrder: promotion,threshold\n")
	s, err = LoadSettings(cwd, []string{scalar})
	if err != nil {
		t.Fatalf("scalar form must load: %v", err)
	}
	if got := s.CompactionMethodOrder(); got != "promotion,threshold" {
		t.Fatalf("scalar methodOrder = %q", got)
	}

	listed := false
	for _, line := range List(s, "/tmp/config.yml") {
		if line == "compaction.methodOrder promotion,threshold" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("config list must show the knob: %v", List(s, "/tmp/config.yml"))
	}

	path := filepath.Join(t.TempDir(), "cli.yml")
	if err := Set(path, "compaction.methodOrder", "overflow,threshold"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "- overflow") {
		t.Fatalf("comma value must store as a list:\n%s", raw)
	}
	s, err = LoadSettings(cwd, []string{path})
	if err != nil {
		t.Fatalf("list form must load: %v", err)
	}
	if got := s.CompactionMethodOrder(); got != "overflow,threshold" {
		t.Fatalf("list methodOrder = %q", got)
	}
}

// TestCompactionMethodOrderLayers: a later layer replaces the order, an
// earlier one survives when the later omits the key.
func TestCompactionMethodOrderLayers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), "compaction:\n  methodOrder: promotion,threshold\n")

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionMethodOrder(); got != "promotion,threshold" {
		t.Fatalf("global methodOrder = %q", got)
	}

	overlay := writeFile(t, filepath.Join(t.TempDir(), "extra.yml"), "maxTurns: 3\n")
	s, err = LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CompactionMethodOrder(); got != "promotion,threshold" {
		t.Fatalf("a layer without the key must not clear it: %q", got)
	}
}

// TestCompactionMethodOrderUnknownKeyIsRejected: the tolerant value type
// must not weaken the strict layer decode.
func TestCompactionMethodOrderUnknownKeyIsRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bad := writeFile(t, filepath.Join(t.TempDir(), "bad.yml"),
		"compaction:\n  methodOder: threshold\n")
	if _, err := LoadSettings(t.TempDir(), []string{bad}); err == nil {
		t.Fatal("a typo inside the compaction block must be rejected")
	}
}
