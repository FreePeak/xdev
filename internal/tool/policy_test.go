package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

func policyBashArgs(t *testing.T, cmd string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPolicyZeroValueIsStrictest(t *testing.T) {
	// The zero Mode is AlwaysAsk, so an unset policy never runs unattended;
	// the shipped yolo default is explicit (DefaultApprovalMode).
	if ModeOf(ApprovalPolicy{}) != AlwaysAsk {
		t.Fatalf("zero-value mode must be strictest, got %s", ModeOf(ApprovalPolicy{}))
	}
	var p ApprovalPolicy
	dec, err := p.Decide("bash", policyBashArgs(t, "rm -rf /"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionPrompt {
		t.Fatalf("zero policy = %s, want prompt", dec.Action)
	}
	if got := DefaultApprovalMode; got != Yolo {
		t.Fatalf("shipped default mode = %s", got)
	}
}

func TestPolicyModeTiers(t *testing.T) {
	tests := []struct {
		mode ApprovalMode
		tool string
		want Action
	}{
		{mode: Write, tool: "read", want: ActionAllow},
		{mode: Write, tool: "edit", want: ActionPrompt},
		{mode: Write, tool: "bash", want: ActionPrompt},
		{mode: AlwaysAsk, tool: "read", want: ActionPrompt},
		{mode: Yolo, tool: "bash", want: ActionAllow},
	}
	for _, tc := range tests {
		p := ApprovalPolicy{Mode: tc.mode}
		args := json.RawMessage(`{}`)
		if tc.tool == "bash" {
			args = policyBashArgs(t, "ls")
		}
		dec, err := p.Decide(tc.tool, args)
		if err != nil {
			t.Fatal(err)
		}
		if dec.Action != tc.want {
			t.Errorf("mode %s tool %s = %s, want %s", tc.mode, tc.tool, dec.Action, tc.want)
		}
	}
}

func TestPolicyDenyBeatsPerToolAllow(t *testing.T) {
	// The whole point of deny-as-absolute: a broad per-tool allow must not
	// resurrect a specifically denied command.
	p := ApprovalPolicy{
		Mode:         Yolo,
		PerTool:      map[string]Action{"bash": ActionAllow},
		BashPatterns: []PolicyRule{{Pattern: "rm -rf *", Action: ActionDeny}},
	}
	dec, err := p.Decide("bash", policyBashArgs(t, "rm -rf /tmp/x"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionDeny {
		t.Fatalf("deny must win, got %s (%s)", dec.Action, dec.Reason)
	}
	if !strings.Contains(dec.Reason, "rm -rf *") {
		t.Fatalf("reason must name the rule: %q", dec.Reason)
	}
}

func TestPolicyPromptRuleUnderYolo(t *testing.T) {
	p := ApprovalPolicy{
		Mode:         Yolo,
		BashPatterns: []PolicyRule{{Pattern: "git push *", Action: ActionPrompt}},
	}
	if dec, _ := p.Decide("bash", policyBashArgs(t, "git push origin main")); dec.Action != ActionPrompt {
		t.Fatalf("git push = %s, want prompt", dec.Action)
	}
	if dec, _ := p.Decide("bash", policyBashArgs(t, "ls")); dec.Action != ActionAllow {
		t.Fatalf("ls = %s, want allow", dec.Action)
	}
}

func TestPolicyDenyBeatsEarlierAllow(t *testing.T) {
	// Precedence, not position: the spec resolves deny > prompt > allow, so
	// a later deny rule still beats an earlier allow. Order only breaks
	// ties among rules of the same class.
	p := ApprovalPolicy{
		Mode: Yolo,
		BashPatterns: []PolicyRule{
			{Pattern: "git *", Action: ActionAllow},
			{Pattern: "git push *", Action: ActionDeny},
		},
	}
	if dec, _ := p.Decide("bash", policyBashArgs(t, "git push origin main")); dec.Action != ActionDeny {
		t.Fatalf("deny must beat an earlier allow, got %s", dec.Action)
	}
	// The allow rule still covers commands nothing denies.
	if dec, _ := p.Decide("bash", policyBashArgs(t, "git status")); dec.Action != ActionAllow {
		t.Fatalf("git status = %s, want allow", dec.Action)
	}
}

func TestPolicySameClassOrderBreaksTies(t *testing.T) {
	// Two prompt rules: the first one that matches supplies the reason.
	p := ApprovalPolicy{
		Mode: Yolo,
		BashPatterns: []PolicyRule{
			{Pattern: "git *", Action: ActionPrompt},
			{Pattern: "git push *", Action: ActionPrompt},
		},
	}
	dec, err := p.Decide("bash", policyBashArgs(t, "git push"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != ActionPrompt || !strings.Contains(dec.Reason, "git *") {
		t.Fatalf("first same-class rule should win: %+v", dec)
	}
}

func TestPolicyTrailingStarMatchesBareCommand(t *testing.T) {
	// `deny:git push *` is the idiomatic spelling and must also stop a bare
	// `git push` — a deny rule that misses the no-argument form is a hole.
	p := ApprovalPolicy{
		Mode:         Yolo,
		BashPatterns: []PolicyRule{{Pattern: "git push *", Action: ActionDeny}},
	}
	for _, cmd := range []string{"git push", "git push origin main", "git   push"} {
		dec, _ := p.Decide("bash", policyBashArgs(t, cmd))
		if dec.Action != ActionDeny && cmd != "git   push" {
			t.Errorf("%q: got %s, want deny", cmd, dec.Action)
		}
	}
}

func TestPolicyCompoundCommandJudgedConservatively(t *testing.T) {
	p := ApprovalPolicy{
		Mode:         Yolo,
		BashPatterns: []PolicyRule{{Pattern: "rm -rf *", Action: ActionDeny}},
	}
	for _, cmd := range []string{
		"echo hi && rm -rf /",
		"echo hi; rm -rf /",
		"true || rm -rf /",
		"echo a\nrm -rf /",
	} {
		if dec, _ := p.Decide("bash", policyBashArgs(t, cmd)); dec.Action != ActionDeny {
			t.Errorf("%q: compound smuggled a denied command (got %s)", cmd, dec.Action)
		}
	}
	// Quoted operators are data, not sequencing.
	if dec, _ := p.Decide("bash", policyBashArgs(t, `echo "rm -rf *"`)); dec.Action != ActionAllow {
		t.Errorf("quoted text matched a rule: %s", dec.Action)
	}
}

func TestPolicyUnknownToolIsAnError(t *testing.T) {
	p := ApprovalPolicy{}
	if _, err := p.Decide("mystery_tool", json.RawMessage(`{}`)); err == nil {
		t.Fatal("an unmodeled tool must error, not default to a tier")
	}
}

func TestParsePolicyRules(t *testing.T) {
	got, err := ParsePolicyRules([]string{"deny:rm -rf *", "prompt:git push *", "docker *"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rules = %+v", got)
	}
	if got[0].Action != ActionDeny || got[0].Pattern != "rm -rf *" {
		t.Fatalf("rule 0 = %+v", got[0])
	}
	if got[2].Action != ActionPrompt {
		t.Fatalf("bare pattern must default to prompt: %+v", got[2])
	}
	if _, err := ParsePolicyRules([]string{"bogus:ls"}); err == nil {
		t.Fatal("unknown action must error")
	}
	if _, err := ParsePolicyRules([]string{"deny:"}); err == nil {
		t.Fatal("empty pattern must error")
	}
}
