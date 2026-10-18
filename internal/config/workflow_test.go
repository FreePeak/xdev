package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workflow group is deliberately a *list of named presets* and nothing
// else. xdev ships no built-in preset: a preset that exists in the binary is a
// fixed pipeline wearing a config file's clothes, which is the shape this
// design was chosen to avoid.

func writeCfg(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDEV_AGENT_DIR", dir)
}

func loadCfg(t *testing.T) *Settings {
	t.Helper()
	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	return s
}

const goodPreset = `
workflow:
  presets:
    - name: quick-fix
      description: One file, one model call.
      backend: opencode
      agent: lean
      directory: "."
      prompt: Fix the failing test.
      context:
        model: {providerID: opencode-go, modelID: glm-5.3-flash}
        permissions:
          - {action: edit, resource: "*", effect: ask}
`

func TestWorkflowPresetLoads(t *testing.T) {
	writeCfg(t, goodPreset)
	s := loadCfg(t)
	if len(s.Workflow.Presets) != 1 {
		t.Fatalf("presets = %d, want 1", len(s.Workflow.Presets))
	}
	p := s.Workflow.Presets[0]
	if p.Name != "quick-fix" || p.Backend != "opencode" || p.Agent != "lean" {
		t.Fatalf("preset = %+v", p)
	}
	if p.Description != "One file, one model call." || p.Directory != "." {
		t.Errorf("preset = %+v", p)
	}
	if p.Context == nil {
		t.Fatal("context block did not survive the decode")
	}
	if p.Context.Model == nil || p.Context.Model.ProviderID != "opencode-go" ||
		p.Context.Model.ModelID != "glm-5.3-flash" {
		t.Errorf("context.model = %+v", p.Context.Model)
	}
	if len(p.Context.Permissions) != 1 {
		t.Fatalf("permissions = %+v", p.Context.Permissions)
	}
	rule := p.Context.Permissions[0]
	if rule.Action != "edit" || rule.Resource != "*" || rule.Effect != "ask" {
		t.Errorf("rule = %+v", rule)
	}
}

// F5: agent is the context lever, and a preset without one is a preset that
// silently runs on whatever opencode last used.
func TestBackendPresetRequiresAgent(t *testing.T) {
	writeCfg(t, `
workflow:
  presets:
    - name: nameless
      backend: opencode
      prompt: hi
      context: {}
`)
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("err = %v, want a complaint naming agent", err)
	}
	if !strings.Contains(err.Error(), "nameless") {
		t.Errorf("err = %v, want it to name the preset", err)
	}
}

// F4: `context:` is mandatory on a backend preset, even when empty. Its
// presence is the enforcement — a preset with no declared budget is exactly
// the naive setup this round exists to prevent, and an absent key is how that
// setup sneaks back in.
func TestBackendPresetRequiresContextBlock(t *testing.T) {
	writeCfg(t, `
workflow:
  presets:
    - name: no-context
      backend: opencode
      agent: lean
      prompt: hi
`)
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("err = %v, want a complaint naming context", err)
	}
	if !strings.Contains(err.Error(), "no-context") {
		t.Errorf("err = %v, want it to name the preset", err)
	}
}

// An empty-but-present context block is a deliberate choice to take opencode's
// own defaults, and must load.
func TestBackendPresetAcceptsEmptyContextBlock(t *testing.T) {
	writeCfg(t, `
workflow:
  presets:
    - name: defaults
      backend: opencode
      agent: lean
      prompt: hi
      context: {}
`)
	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if len(s.Workflow.Presets) != 1 || s.Workflow.Presets[0].Context == nil {
		t.Fatalf("presets = %+v", s.Workflow.Presets)
	}
	if s.Workflow.Presets[0].Context.Model != nil {
		t.Errorf("expected opencode's own model to stand, got %+v", s.Workflow.Presets[0].Context.Model)
	}
}

func TestPresetRejectsUnknownBackend(t *testing.T) {
	writeCfg(t, `
workflow:
  presets:
    - name: wrong
      backend: claude-code
      agent: x
      prompt: hi
      context: {}
`)
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "claude-code") {
		t.Fatalf("err = %v, want a complaint naming the unknown backend", err)
	}
}

func TestPresetRejectsBadPermissionEffect(t *testing.T) {
	writeCfg(t, `
workflow:
  presets:
    - name: bad-effect
      backend: opencode
      agent: lean
      prompt: hi
      context:
        permissions:
          - {action: edit, resource: "*", effect: maybe}
`)
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "maybe") {
		t.Fatalf("err = %v, want a complaint naming the bad effect", err)
	}
}

func TestPresetRejectsDuplicateNames(t *testing.T) {
	writeCfg(t, `
workflow:
  presets:
    - name: twin
      backend: opencode
      agent: a
      prompt: hi
      context: {}
    - name: twin
      backend: opencode
      agent: b
      prompt: hi
      context: {}
`)
	_, err := LoadSettings(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "twin") {
		t.Fatalf("err = %v, want a complaint naming the duplicate", err)
	}
}

// A pure-local preset drives no backend, so it owes no context budget.
func TestLocalPresetNeedsNoBackendOrContext(t *testing.T) {
	writeCfg(t, `
workflow:
  presets:
    - name: local-note
      prompt: just write it down
`)
	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if len(s.Workflow.Presets) != 1 {
		t.Fatalf("presets = %+v", s.Workflow.Presets)
	}
	if got := s.Preset("local-note"); got == nil {
		t.Fatal("Preset lookup missed local-note")
	}
}

// A layer that lists presets means exactly that list — the same rule
// statusLine.segments follows. A project overlay replaces the global set
// wholesale rather than appending to it.
func TestPresetListReplacesRatherThanAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
workflow:
  presets:
    - name: global-one
      backend: opencode
      agent: a
      prompt: hi
      context: {}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDEV_AGENT_DIR", dir)

	overlay := filepath.Join(dir, "project.yml")
	if err := os.WriteFile(overlay, []byte(`
workflow:
  presets:
    - name: project-one
      backend: opencode
      agent: b
      prompt: hi
      context: {}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettings(t.TempDir(), []string{overlay})
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if len(s.Workflow.Presets) != 1 || s.Workflow.Presets[0].Name != "project-one" {
		t.Fatalf("presets = %+v, want exactly [project-one]", s.Workflow.Presets)
	}
	if s.Preset("global-one") != nil {
		t.Error("global preset survived a list that replaced it")
	}
}

func TestPresetLookupMissesReturnNil(t *testing.T) {
	writeCfg(t, goodPreset)
	s := loadCfg(t)
	if s.Preset("nope") != nil {
		t.Error("Preset(nope) should be nil")
	}
	if s.Preset("quick-fix") == nil {
		t.Error("Preset(quick-fix) should resolve")
	}
}

func TestNoPresetsIsNotAnError(t *testing.T) {
	writeCfg(t, "theme: dark\n")
	s := loadCfg(t)
	if len(s.Workflow.Presets) != 0 {
		t.Fatalf("presets = %+v, want none", s.Workflow.Presets)
	}
	if len(s.PresetNames()) != 0 {
		t.Fatalf("names = %v, want none", s.PresetNames())
	}
}
