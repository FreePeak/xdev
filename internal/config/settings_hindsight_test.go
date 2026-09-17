package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHindsightSettings pins the additive hindsight.* group (M12 #43): the
// keys default to unset, a layer sets them, an unknown enum is rejected, and
// the resolved values surface in the config list.
func TestHindsightSettings(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // keep the user's real config out
	cwd := t.TempDir()

	base, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if base.Hindsight.APIURL != "" || base.Hindsight.Scoping != "" {
		t.Fatalf("hindsight keys must default to unset: %+v", base.Hindsight)
	}

	overlay := filepath.Join(t.TempDir(), "overlay.yml")
	body := strings.Join([]string{
		"memory: hindsight",
		"hindsight:",
		"  apiUrl: http://memory.internal:8888",
		"  apiToken: secret-token-value",
		"  bankId: team",
		"  projectSelector: /Users/me/work/leankg",
		"  scoping: per-project",
		"  retainMode: last-turn",
		"  recallBudget: high",
		"  autoRetain: false",
		"  retainEveryNTurns: 5",
		"  injectionTokenLimit: 512",
		"  requestTimeoutMs: 15000",
	}, "\n")
	if err := os.WriteFile(overlay, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	if s.Memory != "hindsight" {
		t.Fatalf("memory = %q, want hindsight", s.Memory)
	}
	h := s.Hindsight
	if h.APIURL != "http://memory.internal:8888" || h.BankID != "team" || h.Scoping != "per-project" || h.RetainMode != "last-turn" || h.RecallBudget != "high" {
		t.Fatalf("hindsight block = %+v", h)
	}
	if h.ProjectSelector != "/Users/me/work/leankg" {
		t.Fatalf("projectSelector = %q, want the configured project path", h.ProjectSelector)
	}
	if h.AutoRetain == nil || *h.AutoRetain {
		t.Fatalf("autoRetain = %v, want an explicit false", h.AutoRetain)
	}
	if h.RetainEveryNTurns != 5 || h.InjectionTokenLimit != 512 || h.RequestTimeoutMS != 15000 {
		t.Fatalf("hindsight numbers = %+v", h)
	}
	listed := strings.Join(List(s, "x"), "\n")
	for _, want := range []string{"memory hindsight", "hindsight.apiUrl http://memory.internal:8888", "hindsight.scoping per-project", "hindsight.bankId team", "hindsight.projectSelector /Users/me/work/leankg", "hindsight.apiToken (set)"} {
		if !strings.Contains(listed, want) {
			t.Errorf("config list missing %q:\n%s", want, listed)
		}
	}
	if strings.Contains(listed, "secret-token-value") {
		t.Error("config list leaked the hindsight token")
	}

	// A typo in an enum is reported instead of silently taking the default.
	bad := filepath.Join(t.TempDir(), "bad.yml")
	if err := os.WriteFile(bad, []byte("hindsight:\n  scoping: per-project-tagged-typo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(cwd, []string{bad}); err == nil || !strings.Contains(err.Error(), "hindsight.scoping") {
		t.Fatalf("unknown scoping error = %v, want a hindsight.scoping message", err)
	}
}
