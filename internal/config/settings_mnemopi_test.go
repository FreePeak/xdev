package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMnemopiSettings pins the additive memoryMnemopi block (M12 #44): the
// shipped defaults are usable, a layer overrides field by field, and an
// unknown enum or a project-tagged bank without a tag is rejected instead of
// silently degrading.
func TestMnemopiSettings(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // keep the user's real config out
	cwd := t.TempDir()

	base, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := base.MnemopiConfig()
	want := DefaultMnemopiSettings()
	if got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	if got.LLMMode != "smol" || got.Scope != "project" || got.InjectionTokenLimit == 0 || got.QueueDrainMillis == 0 {
		t.Fatalf("defaults are not usable: %+v", got)
	}
	// A nil settings value must still answer with the defaults (pre-main
	// callers build the backend before settings are loaded).
	var nilSettings *Settings
	if nilSettings.MnemopiConfig() != want {
		t.Fatal("a nil settings value must fall back to the defaults")
	}

	overlay := filepath.Join(t.TempDir(), "overlay.yml")
	body := "memory: mnemopi\nmemoryMnemopi:\n  llmMode: remote\n  recallLimit: 12\n  scope: project-tagged\n  tag: work\n"
	if err := os.WriteFile(overlay, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	got = s.MnemopiConfig()
	if got.LLMMode != "remote" || got.RecallLimit != 12 || got.Scope != "project-tagged" || got.Tag != "work" {
		t.Fatalf("overlay did not merge field by field: %+v", got)
	}
	// Untouched fields keep their shipped default.
	if got.InjectionTokenLimit != want.InjectionTokenLimit || got.RetainEveryNTurns != want.RetainEveryNTurns {
		t.Fatalf("unspecified fields lost their defaults: %+v", got)
	}
	if s.Memory != "mnemopi" {
		t.Fatalf("memory = %q, want mnemopi", s.Memory)
	}
	listed := strings.Join(List(s, "x"), "\n")
	for _, key := range []string{"memoryMnemopi.scope project-tagged", "memoryMnemopi.llmMode remote", "memoryMnemopi.recallLimit 12"} {
		if !strings.Contains(listed, key) {
			t.Errorf("config list is missing %q:\n%s", key, listed)
		}
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"llmMode", "memoryMnemopi: {llmMode: telepathic}\n", "llmMode"},
		{"scope", "memoryMnemopi: {scope: galactic}\n", "scope"},
		{"tagless", "memoryMnemopi: {scope: project-tagged}\n", "needs memoryMnemopi.tag"},
		{"negative", "memoryMnemopi: {recallLimit: -2}\n", "must not be negative"},
		{"backend", "memory: telepathy\n", "unknown memory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "bad.yml")
			if err := os.WriteFile(bad, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSettings(cwd, []string{bad}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	// -1 is the documented way to disable the turn trigger from a layer.
	off := filepath.Join(t.TempDir(), "off.yml")
	if err := os.WriteFile(off, []byte("memoryMnemopi: {retainEveryNTurns: -1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	noTurns, err := LoadSettings(cwd, []string{off})
	if err != nil {
		t.Fatalf("retainEveryNTurns -1 must be accepted: %v", err)
	}
	if noTurns.MnemopiConfig().RetainEveryNTurns != -1 {
		t.Fatalf("retainEveryNTurns = %d, want -1", noTurns.MnemopiConfig().RetainEveryNTurns)
	}
}
