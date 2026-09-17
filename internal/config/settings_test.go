package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
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
toolsApproval:
  bash: allow
  read: prompt
disabledProviders: [bedrock]
`)
	// The repository layer sticks to what a clone may choose (#114); the
	// per-key map merge below is exercised between two trusted layers.
	writeFile(t, projectSettingsPath(cwd), `
theme: grokday
`)
	overlay := writeFile(t, filepath.Join(t.TempDir(), "extra.yml"), `
maxTurns: 7
toolsApproval:
  read: deny
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
	if s.ToolsApproval["bash"] != "allow" || s.ToolsApproval["read"] != "deny" {
		t.Errorf("toolsApproval must merge per key: %v", s.ToolsApproval)
	}
	// Defaults still show through where nothing set them.
	if s.MemoryLimit != 100<<20 {
		t.Errorf("memoryLimit default lost: %d", s.MemoryLimit)
	}
	if s.Memory != "local" {
		t.Errorf("memory default lost: %q, want local", s.Memory)
	}
}

func TestMemoryDefaultsToLocalAndOffWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Memory != "local" {
		t.Fatalf("schema default = %q, want local", s.Memory)
	}

	overlay := writeFile(t, filepath.Join(t.TempDir(), "off.yml"), "memory: off\n")
	off, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	if off.Memory != "off" {
		t.Fatalf("explicit off = %q, want off", off.Memory)
	}
}

// TestSettingsThinkingLevel pins the request-side level key: unset is "auto"
// (the model role decides), the project layer cannot loosen a global "off"
// (later layers win, the same rule as showThinking), and a value outside the
// verbatim vocabulary fails at load instead of degrading to auto.
func TestSettingsThinkingLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ThinkingLevel(); got != "auto" {
		t.Fatalf("unset thinking = %q, want auto", got)
	}
	if nilLevel := (*Settings)(nil).ThinkingLevel(); nilLevel != "auto" {
		t.Fatalf("nil settings = %q, want auto", nilLevel)
	}

	// The persisted key survives the merge and is what the flag folds under.
	writeFile(t, GlobalSettingsPath(), "thinking: off\n")
	s, err = LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ThinkingLevel(); got != "off" {
		t.Fatalf("global thinking = %q, want off", got)
	}

	// A repository cannot spend the user's tokens: `thinking` is absent from
	// repoSafeSettingsKeys, so the project layer is pruned and the global
	// value stands (the drop is reported, not silent).
	writeFile(t, projectSettingsPath(cwd), "thinking: high\n")
	s, err = LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ThinkingLevel(); got != "off" {
		t.Fatalf("project thinking = %q, want the global off", got)
	}

	writeFile(t, GlobalSettingsPath(), "thinking: bogus\n")
	if _, err := LoadSettings(cwd, nil); err == nil {
		t.Fatal("an unknown thinking level must fail at load")
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

// TestLegacyModelRolesMigrates: the model-role aliases were removed, so a
// config.yml that still carries modelRoles must not be renamed to
// .broken-<stamp> on the next start — the one binding that meant something
// (the model a run uses) becomes defaultModel and the file keeps loading.
func TestLegacyModelRolesMigrates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeFile(t, GlobalSettingsPath(), "modelRoles:\n  default: onegw/free\n  smol: onegw/tiny\nshowThinking: true\n")
	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("a legacy modelRoles block must still load: %v", err)
	}
	if s.DefaultModel != "onegw/free" {
		t.Fatalf("defaultModel = %q, want the migrated modelRoles.default", s.DefaultModel)
	}
	if !s.ShowThinkingOn() {
		t.Fatal("the rest of the file was dropped with the migration")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the user's file must stay where it is: %v", statErr)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".broken-") {
			t.Fatalf("a migrated file was treated as broken: %v", e.Name())
		}
	}
	// An alias target is not a model ref: it is dropped, not forwarded.
	writeFile(t, GlobalSettingsPath(), "modelRoles:\n  default: \"@smol\"\n")
	s, err = LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("an alias-valued modelRoles must still load: %v", err)
	}
	if s.DefaultModel != "" {
		t.Fatalf("defaultModel = %q, want empty (an alias is not a ref)", s.DefaultModel)
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
		{"toolsApproval.smol", "allow"},
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
	if !strings.Contains(body, "toolsApproval:") || !strings.Contains(body, "smol: allow") {
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

// TestSetRefusesUnknownKey pins the P0 guard: `xdev config set <typo> <value>`
// used to write the typo and report success, and the NEXT start then rejected
// the file (per-layer loads are strict KnownFields), moved it aside as
// .broken-* and came up on defaults — losing every setting the user had. The
// key is checked against the Settings schema before anything is written, and
// the result must still decode strictly. Open key sets stay settable: their
// names are data, not schema.
func TestSetRefusesUnknownKey(t *testing.T) {
	const original = "theme: groknight\n"
	path := writeFile(t, filepath.Join(t.TempDir(), "config.yml"), original)
	for _, key := range []string{
		"showThinkng", // the typo that cost a real config its life
		"typokey",
		"bash.backgroundTimeoutSeconds",
		"a.",
	} {
		if err := Set(path, key, "1"); err == nil {
			t.Fatalf("Set(%q) must be refused", key)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != original {
		t.Fatalf("a refused Set must leave the file untouched:\n%s", after)
	}
	for _, kv := range [][2]string{
		{"theme", "grokday"}, {"maxTurns", "42"}, {"advisor", "true"},
		{"personality", "friendly"}, {"defaultModel", "onegw/xdev"},
		{"toolsApproval.bash", "allow"}, {"memoryMnemopi.scope", "project"},
		{"hooks.preToolUse", "true"},
	} {
		if err := Set(path, kv[0], kv[1]); err != nil {
			t.Fatalf("Set(%q): %v", kv[0], err)
		}
	}
	// What the command wrote must survive the load path it protects.
	if _, err := LoadSettings(t.TempDir(), []string{path}); err != nil {
		t.Fatalf("written file rejected on load: %v", err)
	}
}

// TestSettingsWritesAreAtomic: Set replaces the file with one rename, so a
// concurrent reader never catches a truncated file. A plain WriteFile does:
// the read lands mid-write, the strict load rejects the half-written YAML and
// the whole config is quarantined as .broken-* — the data loss this writer
// used to cause whenever two sessions wrote at once.
func TestSettingsWritesAreAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := Set(path, "theme", "grokday"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := Set(path, "maxTurns", fmt.Sprint(1000+i)); err != nil {
				t.Errorf("Set: %v", err)
				return
			}
		}
	}()
	// A reader must never catch a half-written file: the bytes it sees have to
	// decode as a whole Settings document.
	for i := 0; i < 300; i++ {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var probe Settings
		d := yaml.NewDecoder(bytes.NewReader(raw))
		d.KnownFields(true)
		if err := d.Decode(&probe); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("a reader saw a partial file: %v\n%s", err, raw)
		}
	}
	close(stop)
	wg.Wait()
	// The mode survives the rewrite (config.yml is 0600 by convention).
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v err=%v, want 0600", fi.Mode(), err)
	}
}

// TestDeleteKeyGuardsTheSameWay: `config reset` writes the file too, so it
// needs the same unknown-key refusal (a reset must not be a way to brick it).
func TestDeleteKeyGuardsTheSameWay(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "config.yml"), "theme: groknight\n")
	if err := DeleteKey(path, "them"); err == nil {
		t.Fatal("DeleteKey must refuse an unknown key")
	}
	if err := DeleteKey(path, "theme"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(path, "theme"); got != "" {
		t.Fatalf("theme = %q, want it deleted", got)
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

	// personality is profile-owned (#114): a repository may not choose the
	// assistant's persona, so the layers here are the user's own files.
	friendly := writeFile(t, filepath.Join(t.TempDir(), "friendly.yml"), "personality: friendly\n")
	s, err = LoadSettings(cwd, []string{friendly})
	if err != nil {
		t.Fatal(err)
	}
	if s.Personality != "friendly" {
		t.Fatalf("user overlay = %q", s.Personality)
	}

	moody := writeFile(t, filepath.Join(t.TempDir(), "moody.yml"), "personality: moody\n")
	if _, err := LoadSettings(cwd, []string{moody}); err == nil || !strings.Contains(err.Error(), "personality") {
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

// TestListRendersTheEnforcedSurface: `xdev config list` is how a user sees
// what the session will actually enforce, so every grouped key (per-role
// effort, per-tool approval, bash patterns, hooks) must appear — map groups
// sorted, hook bodies never dumped. The empty case pins the base line set.
func TestListRendersTheEnforcedSurface(t *testing.T) {
	const path = "/tmp/xdev/config.yml"
	tests := []struct {
		name string
		s    *Settings
		want []string
	}{
		{
			name: "empty settings: the base lines plus the config path",
			s:    &Settings{},
			want: []string{
				"theme ", "approvalMode ", "maxTurns 0", "memoryLimit 0",
				"showThinking true", "advisor false", "memory off",
				"sidebarMode auto", "thinking auto", "config " + path,
			},
		},
		{
			name: "enforced groups: sorted by key, hooks as a count",
			s: &Settings{
				ToolsApproval: map[string]string{"read": "allow", "bash": "prompt", "write": "allow"},
				BashPatterns:  []string{"deny:rm -rf *", "allow:ls"},
				Hooks:         map[string]any{"post-tool": "secret-hook-body"},
			},
			want: []string{
				"theme ", "approvalMode ", "maxTurns 0", "memoryLimit 0",
				"showThinking true", "advisor false", "memory off",
				"toolsApproval.bash prompt",
				"toolsApproval.read allow",
				"toolsApproval.write allow",
				"bashPatterns deny:rm -rf *, allow:ls",
				"hooks 1 configured", // the hook body is never part of the list
				"config " + path,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Repeat: an unsorted group ranges the map in a new random order
			// each time, so a single run could pass by luck.
			for range 4 {
				lines := List(tc.s, path)
				for _, want := range tc.want {
					if !slices.Contains(lines, want) {
						t.Fatalf("List is missing %q:\n%s", want, strings.Join(lines, "\n"))
					}
				}
			}
		})
	}
}
