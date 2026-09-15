package config

import (
	"path/filepath"
	"testing"
)

// retry.infinite is the persistent form of "always retry": the schema reads
// it, layering turns it on one-way, `config set` writes a shape that reads
// back, and `config list` shows it.
func TestSettingsRetryInfinite(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()
	writeFile(t, GlobalSettingsPath(), "retry:\n  infinite: true\n")
	s, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.RetryConfig().Infinite {
		t.Fatal("retry.infinite did not survive decoding")
	}

	// One-way: a later false layer cannot turn off what an earlier one
	// turned on (the prewalk.enabled rule).
	off := writeFile(t, filepath.Join(t.TempDir(), "overlay.yml"), "retry:\n  infinite: false\n")
	s2, err := LoadSettings(cwd, []string{off})
	if err != nil {
		t.Fatal(err)
	}
	if !s2.RetryConfig().Infinite {
		t.Fatal("a false layer turned retry.infinite off; the merge must be one-way")
	}
	// The flag shape round-trips through `config set`, and the list shows it.
	overlay := t.TempDir() + "/overlay.yml"
	if err := Set(overlay, "retry.infinite", "true"); err != nil {
		t.Fatal(err)
	}
	s3, err := LoadSettings(t.TempDir(), []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	if !s3.RetryConfig().Infinite {
		t.Fatal("config set retry.infinite true did not read back")
	}
	var found bool
	for _, line := range List(s3, overlay) {
		if line == "retry.infinite true" {
			found = true
		}
	}
	if !found {
		t.Fatal("config list does not show retry.infinite")
	}
}
