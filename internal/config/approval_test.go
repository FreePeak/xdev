package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

func TestSettingsPolicyBridge(t *testing.T) {
	s := &Settings{
		ApprovalMode:  "write",
		ToolsApproval: map[string]string{"read": "allow", "bash": "prompt"},
		BashPatterns:  []string{"deny:rm -rf *", "git push *"},
	}
	pol, err := s.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if pol.Mode != tool.Write {
		t.Fatalf("mode = %s", pol.Mode)
	}
	if pol.PerTool["read"] != tool.ActionAllow || pol.PerTool["bash"] != tool.ActionPrompt {
		t.Fatalf("per-tool = %v", pol.PerTool)
	}
	if len(pol.BashPatterns) != 2 || pol.BashPatterns[0].Action != tool.ActionDeny {
		t.Fatalf("rules = %+v", pol.BashPatterns)
	}
	// The bare rule defaults to prompt.
	if pol.BashPatterns[1].Action != tool.ActionPrompt {
		t.Fatalf("bare pattern = %s", pol.BashPatterns[1].Action)
	}
}

func TestSettingsPolicyRejectsBadValues(t *testing.T) {
	badAction := &Settings{ApprovalMode: "yolo", ToolsApproval: map[string]string{"bash": "maybe"}}
	if _, err := badAction.Policy(); err == nil || !strings.Contains(err.Error(), "toolsApproval[bash]") {
		t.Fatalf("bad per-tool action must error by name, got %v", err)
	}
	badRule := &Settings{ApprovalMode: "yolo", BashPatterns: []string{"nope:x"}}
	if _, err := badRule.Policy(); err == nil {
		t.Fatal("bad bash rule must error")
	}
	badMode := &Settings{ApprovalMode: "sometimes"}
	if _, err := badMode.Policy(); err == nil {
		t.Fatal("bad mode must error")
	}
}

func TestSettingsYoloDefaultLoadsPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := s.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if pol.Mode != tool.Yolo {
		t.Fatalf("shipped default = %s, want yolo", pol.Mode)
	}
	if len(pol.BashPatterns) != 0 {
		t.Fatalf("unexpected rules: %v", pol.BashPatterns)
	}
}

// TestApprovalModeFromFile: the settings file must drive a real denial.
func TestApprovalModeFromFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, GlobalSettingsPath(), `
approvalMode: yolo
bashPatterns:
  - "deny:rm -rf *"
`)
	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := s.Policy()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := pol.Decide("bash", []byte(`{"command":"rm -rf /"}`))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != tool.ActionDeny {
		t.Fatalf("configured deny rule did not fire: %s", dec.Action)
	}
}

func TestSettingsBashGroupReachesPolicy(t *testing.T) {
	// The `bash` group (M13 #56) decodes through the layering merge and lands
	// in the resolved policy: the compound opt-in reaches the pattern
	// resolver, and the interceptor command is trimmed.
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	on := filepath.Join(dir, "on.yml")
	if err := os.WriteFile(on, []byte("bash:\n  allowCompoundCommands: true\n  interceptor: \"  /bin/review.sh  \"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(dir, []string{on})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := s.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if !pol.AllowCompoundCommands {
		t.Fatal("bash.allowCompoundCommands did not reach the policy")
	}
	if pol.BashInterceptor.Command != "/bin/review.sh" {
		t.Fatalf("interceptor = %q, want it trimmed", pol.BashInterceptor.Command)
	}

	// A later layer can turn the opt-in back off (the pointer exists for
	// exactly this) without wiping the interceptor beside it.
	off := filepath.Join(dir, "off.yml")
	if err := os.WriteFile(off, []byte("bash:\n  allowCompoundCommands: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadSettings(dir, []string{on, off})
	if err != nil {
		t.Fatal(err)
	}
	s2pol, err := s2.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if s2pol.AllowCompoundCommands {
		t.Fatal("a later layer must be able to disable the opt-in")
	}
	if s2pol.BashInterceptor.Command != "/bin/review.sh" {
		t.Fatalf("a partial layer wiped the interceptor: %q", s2pol.BashInterceptor.Command)
	}
}

func TestSettingsBashDefaultsAreConservative(t *testing.T) {
	// Shipped defaults: per-segment resolution and no interceptor.
	t.Setenv("HOME", t.TempDir())
	s, err := LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := s.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if pol.AllowCompoundCommands || !pol.BashInterceptor.Off() {
		t.Fatalf("default bash policy = %+v", pol)
	}
}
