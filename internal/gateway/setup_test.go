package gateway

import (
	"strings"
	"testing"
)

// TestRenderUnitStartsAtLoginAndRestarts pins the two properties that make
// the daemon "run in the background": the supervisor must start it without a
// login shell, and must restart it when it exits.
func TestRenderUnitStartsAtLoginAndRestarts(t *testing.T) {
	got := RenderUnit(InstallOptions{
		Exe:     "/usr/local/bin/xdev",
		DataDir: "/Users/tester/.xdev/agent",
		Version: "1.2.3",
		Args:    []string{"--profile", "home"},
	})
	switch {
	case !strings.Contains(got, "/usr/local/bin/xdev"):
		t.Errorf("unit does not run the installed binary:\n%s", got)
	case !strings.Contains(got, "gateway"):
		t.Errorf("unit does not run the gateway subcommand:\n%s", got)
	case !strings.Contains(got, "run"):
		t.Errorf("unit does not run the daemon loop:\n%s", got)
	case !strings.Contains(got, "XDEV_AGENT_DIR"):
		t.Errorf("unit does not pin the data dir — launchd has no shell profile to read it from:\n%s", got)
	case !strings.Contains(got, "LogPath") && !strings.Contains(got, ".xdev/agent/gateway/gateway.log"):
		t.Errorf("unit does not send stdout/stderr to the log:\n%s", got)
	}
}

// TestUnitEnvPinsTheDataDirEverywhere: a daemon started by launchd sees
// neither PATH nor a shell profile, so the data dir must ride in the
// environment or the bridge comes up against the wrong data.
func TestUnitEnvPinsTheDataDir(t *testing.T) {
	pairs := unitEnvPairs("/Users/tester/.xdev/agent")
	if len(pairs) == 0 || pairs[0][0] != "XDEV_AGENT_DIR" {
		t.Fatalf("unitEnvPairs = %v, want XDEV_AGENT_DIR first", pairs)
	}
	if pairs[0][1] != "/Users/tester/.xdev/agent" {
		t.Errorf("XDEV_AGENT_DIR = %q", pairs[0][1])
	}
}

// TestParseChatListRejectsJunk: an allowlist entry that cannot be parsed is
// an error rather than a silently dropped one, because dropping one turns a
// working bridge into one that refuses its owner.
func TestParseChatListRejectsJunk(t *testing.T) {
	if _, err := ParseChatList("123, abc, 456"); err == nil {
		t.Error("ParseChatList accepted a non-numeric entry")
	}
	ids, err := ParseChatList(" 123 , 456 ")
	if err != nil {
		t.Fatalf("ParseChatList: %v", err)
	}
	if len(ids) != 2 || ids[0] != 123 || ids[1] != 456 {
		t.Errorf("ids = %v, want [123 456]", ids)
	}
}

// TestFormatChatListRoundTrips: the written form must parse back into the
// same set, or `setup` writes an allowlist the daemon will not accept.
func TestFormatChatListRoundTrips(t *testing.T) {
	got, err := ParseChatList(FormatChatList([]int64{798213991, 1969551591}))
	if err != nil {
		t.Fatalf("parse back: %v", err)
	}
	if len(got) != 2 || got[0] != 798213991 {
		t.Errorf("round trip = %v", got)
	}
	if FormatChatList(nil) != "" {
		t.Errorf("FormatChatList(nil) = %q, want empty", FormatChatList(nil))
	}
}

// TestWriteTokenRefusesJunkAndMode: a pasted non-token must fail at setup
// rather than in a daemon that polls nothing, and the file must be 0600.
func TestWriteTokenRefusesJunkAndMode(t *testing.T) {
	dir := t.TempDir()
	if err := WriteToken(dir, "not a token"); err == nil {
		t.Error("WriteToken accepted a non-token")
	}
	tok := "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest"
	if err := WriteToken(dir, tok); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if got := ReadTokenFile(dir); got != tok {
		t.Errorf("ReadTokenFile = %q, want %q", got, tok)
	}
}

// TestResolveTokenPrefersEnv: TELEGRAM_BOT_TOKEN wins over the file, which
// is how a supervisor overrides a stale token without touching the data dir.
func TestResolveTokenPrefersEnv(t *testing.T) {
	dir := t.TempDir()
	tok := "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest"
	if err := WriteToken(dir, tok); err != nil {
		t.Fatal(err)
	}
	t.Setenv(TokenEnv, "999999999:BBbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if got := ResolveToken(dir); got != "999999999:BBbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("ResolveToken = %q, want the env token", got)
	}
}
