package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGatewaySettings pins the `gateway:` group end to end: an absent group
// leaves the bridge off, a config layer turns it on with an allowlist, the
// env override lands, and out-of-range numbers are refused with a message
// naming the key rather than silently dropping the value.
func TestGatewaySettings(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir()) // keep the user's real config out
	cwd := t.TempDir()

	base, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if base.GatewayEnabled() {
		t.Error("gateway must be off unless a layer turns it on")
	}
	if got := base.GatewayAllowedChats(); len(got) != 0 {
		t.Errorf("allowlist must start empty, got %v", got)
	}

	overlay := filepath.Join(t.TempDir(), "overlay.yml")
	body := strings.Join([]string{
		"gateway:",
		"  enabled: true",
		"  allowedChats: [111, 222]",
		"  workspace: /tmp/gateway-ws",
		"  replyChunk: 3500",
		"  pollTimeout: 30",
		"  stream: false",
		"  streamIntervalMs: 2500",
	}, "\n")
	if err := os.WriteFile(overlay, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	on, err := LoadSettings(cwd, []string{overlay})
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	if !on.GatewayEnabled() {
		t.Error("gateway.enabled did not land")
	}
	if got := on.GatewayAllowedChats(); len(got) != 2 || got[0] != 111 || got[1] != 222 {
		t.Errorf("allowlist = %v, want [111 222]", got)
	}
	if got := on.GatewayWorkspace(); got != "/tmp/gateway-ws" {
		t.Errorf("workspace = %q", got)
	}
	if got := on.GatewayReplyChunk(); got != 3500 {
		t.Errorf("replyChunk = %d, want 3500", got)
	}
	if got := on.GatewayPollTimeout(); got != 30 {
		t.Errorf("pollTimeout = %d, want 30", got)
	}
	// Streaming is ON unless a layer turns it off: an operator who wants the
	// old "one reply at the end" behaviour sets it false, and the pointer is
	// what makes that distinguishable from "unset".
	if on.GatewayStream() {
		t.Error("gateway.stream: false did not land — the choice would be ignored")
	}
	if got := on.GatewayStreamInterval(); got != 2500*time.Millisecond {
		t.Errorf("streamIntervalMs = %s, want 2.5s", got)
	}
	fresh, err := LoadSettings(cwd, nil)
	if err != nil {
		t.Fatalf("load fresh: %v", err)
	}
	if !fresh.GatewayStream() {
		t.Error("streaming must default ON — the phone should see the answer grow")
	}
	if got := fresh.GatewayStreamInterval(); got != 1500*time.Millisecond {
		t.Errorf("default stream interval = %s, want 1.5s", got)
	}
	// The floor: a sub-250ms cadence spends the turn being rate-limited.
	fast := filepath.Join(t.TempDir(), "fast.yml")
	if err := os.WriteFile(fast, []byte("gateway:\n  streamIntervalMs: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clamped, err := LoadSettings(cwd, []string{fast})
	if err != nil {
		t.Fatalf("load fast: %v", err)
	}
	if got := clamped.GatewayStreamInterval(); got != 250*time.Millisecond {
		t.Errorf("a 1ms cadence was not clamped to the 250ms floor, got %s", got)
	}

	// Env override: how a supervisor authorizes a daemon without editing
	// the config file (a launchd agent has no shell profile to read).
	t.Setenv("TELEGRAM_ALLOWED_CHATS", "333,444")
	if got := on.GatewayAllowedChats(); len(got) != 4 || got[3] != 444 {
		t.Errorf("env allowlist did not extend the settings: %v", got)
	}

	// The range guards, and the thing they refuse: a silent default.
	for _, bad := range []struct{ key, value, want string }{
		{"gateway:\n  replyChunk: 9000", "", "replyChunk"},
		{"gateway:\n  pollTimeout: 999", "", "pollTimeout"},
		{"gateway:\n  leaseWait: -1", "", "leaseWait"},
	} {
		p := filepath.Join(t.TempDir(), "overlay.yml")
		if err := os.WriteFile(p, []byte(bad.key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSettings(cwd, []string{p}); err == nil {
			t.Errorf("%s was accepted; want a refusal naming the key", strings.ReplaceAll(bad.key, "\n", ", "))
		} else if !strings.Contains(err.Error(), bad.want) {
			t.Errorf("error for %q does not name the key: %v", bad.key, err)
		}
	}
}
