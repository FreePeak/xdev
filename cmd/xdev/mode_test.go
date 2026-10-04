package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tui"
)

// The mode surface is one vocabulary over two knobs the agent already turns —
// the plan guard and the approval policy. These tests pin the derivation and
// the write, because the failure that matters is quiet: a readout that says
// "bypass" while the policy still prompts, or a chip that says "plan" with a
// full toolset still in force.

// TestSessionModeDerivation pins the read: plan wins when the guard is on (a
// read-only run is stricter than any approval mode), a CLI override wins over
// the settings file exactly as it does for the per-turn policy, and anything
// unrecognised reads as bypass — which is what parseApprovalMode does with it.
func TestSessionModeDerivation(t *testing.T) {
	cases := []struct {
		name             string
		planActive       bool
		override, inFile string
		want             string
	}{
		{"plan guard wins over the approval mode", true, "", "yolo", tui.ModePlan},
		{"always-ask is default", false, "", "always-ask", tui.ModeDefault},
		{"write is auto", false, "", "write", tui.ModeAuto},
		{"yolo is bypass", false, "", "yolo", tui.ModeBypass},
		{"unset is bypass (the shipped default)", false, "", "", tui.ModeBypass},
		{"a CLI override beats the file", false, "write", "always-ask", tui.ModeAuto},
		{"an unknown value does not fail closed into a stricter mode", false, "", "nonsense", tui.ModeBypass},
	}
	for _, tc := range cases {
		if got := sessionModeOf(tc.planActive, tc.override, tc.inFile); got != tc.want {
			t.Errorf("%s: mode = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSetSessionModeDrivesBothKnobs is the contract that keeps /mode and
// /plan from becoming two truths: entering plan turns the guard on, leaving it
// turns it off (so a mode is reachable from plan without typing /plan off), and
// the other three write the approval vocabulary through the one apply path.
func TestSetSessionModeDrivesBothKnobs(t *testing.T) {
	planMode := &agent.PlanMode{}
	vibe := false
	var applied []string
	apply := func(m string) error { applied = append(applied, m); return nil }
	cur := func() string { return sessionModeOf(planMode.Active(), "", "yolo") }
	set := setSessionMode(planMode, func() bool { return vibe }, apply, cur)

	// plan is session state and never touches the settings file.
	if err := set(tui.ModePlan); err != nil {
		t.Fatal(err)
	}
	if !planMode.Active() {
		t.Fatal("/mode plan did not turn the guard on")
	}
	if len(applied) != 0 {
		t.Fatalf("plan mode wrote %v — it must not persist a read-only posture", applied)
	}

	// Naming another mode leaves plan, so the chip cannot read "plan" over a
	// full toolset.
	if err := set(tui.ModeAuto); err != nil {
		t.Fatal(err)
	}
	if planMode.Active() {
		t.Fatal("/mode auto left the plan guard on")
	}
	if len(applied) != 1 || applied[0] != "write" {
		t.Fatalf("applied = %v, want [write]", applied)
	}

	if err := set(tui.ModeDefault); err != nil {
		t.Fatal(err)
	}
	if err := set(tui.ModeBypass); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(applied, ","); got != "write,always-ask,yolo" {
		t.Fatalf("approval vocabulary = %q", got)
	}

	// An unknown name changes nothing and says which mode is still in force.
	applied = nil
	if err := set("yolo-lite"); err == nil {
		t.Fatal("an unknown mode must be an error")
	} else if !strings.Contains(err.Error(), "bypass") {
		t.Fatalf("the error must name the mode still in force: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("a rejected mode wrote %v", applied)
	}

	// The director's reduced toolset and read-only planning contradict each
	// other: one mode at a time, and the error says which one to leave. The
	// guard was OFF before this, and a refused transition must leave it off —
	// a mode switch that half-applied would leave the chip and the toolset
	// telling different stories.
	vibe = true
	if err := set(tui.ModePlan); err == nil || !strings.Contains(err.Error(), "vibe") {
		t.Fatalf("entering plan under vibe = %v, want a named conflict", err)
	}
	if planMode.Active() {
		t.Fatal("a refused plan transition turned the guard on anyway")
	}
}

// TestPersistApprovalModeWritesThroughConfigSet pins the one write path: the
// settings file is what the next xdev process reads, so the mode must land
// there (and in the in-memory layer every read this process does) rather than
// only in the live policy.
func TestPersistApprovalModeWritesThroughConfigSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDEV_AGENT_DIR", filepath.Join(home, ".xdev", "agent"))
	if err := os.MkdirAll(config.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := config.LoadSettings(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	loadedSettings = s
	t.Cleanup(func() { loadedSettings = nil })

	if err := persistApprovalMode("always-ask"); err != nil {
		t.Fatal(err)
	}
	if got := lastSettings().ApprovalMode; got != "always-ask" {
		t.Fatalf("in-memory approvalMode = %q", got)
	}
	raw, err := os.ReadFile(config.GlobalSettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "always-ask") {
		t.Fatalf("the settings file did not record the mode:\n%s", raw)
	}
	// A rejected value must not reach the file: config.Set validates against
	// the schema, so a typo fails here instead of poisoning the next start.
	if err := persistApprovalMode("nonsense"); err == nil {
		t.Fatal("an invalid approvalMode must fail at write time")
	}
}
