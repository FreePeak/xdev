package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// defaultRootEnv clears every base-dir knob and points HOME at a fresh dir.
// Placeholders in the table below expand at run time.
func defaultRootEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDEV_AGENT_DIR", "")
	t.Setenv("XDEV_PROFILE", "")
	if err := SetProfile(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetProfile("") })
}

// TestBaseDirPrecedence is the resolution table: --profile > XDEV_PROFILE >
// XDEV_AGENT_DIR > XDG (once init-xdg recorded it) > ~/.xdev/agent, with
// the profile nesting under whichever root the lower knobs pick.
func TestBaseDirPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		profile    string // --profile value
		envProfile string // XDEV_PROFILE
		agentDir   bool   // set XDEV_AGENT_DIR to a sandbox dir
		initXDG    bool
		wantData   string
		wantState  string
		wantCache  string
	}{
		{
			name:      "legacy default",
			wantData:  "{home}/.xdev/agent",
			wantState: "{home}/.xdev/agent",
			wantCache: "{home}/.xdev/agent",
		},
		{
			name:      "agent dir override",
			agentDir:  true,
			wantData:  "{agent}",
			wantState: "{agent}",
			wantCache: "{agent}",
		},
		{
			name:      "xdg roots after init-xdg",
			initXDG:   true,
			wantData:  "{xdg}/data/xdev",
			wantState: "{xdg}/state/xdev",
			wantCache: "{xdg}/cache/xdev",
		},
		{
			name:      "agent dir beats xdg",
			agentDir:  true,
			initXDG:   true,
			wantData:  "{agent}",
			wantState: "{agent}",
			wantCache: "{agent}",
		},
		{
			name:      "profile flag",
			profile:   "work",
			wantData:  "{home}/.xdev/agent/profiles/work",
			wantState: "{home}/.xdev/agent/profiles/work",
			wantCache: "{home}/.xdev/agent/profiles/work",
		},
		{
			name:       "profile env",
			envProfile: "work",
			wantData:   "{home}/.xdev/agent/profiles/work",
			wantState:  "{home}/.xdev/agent/profiles/work",
			wantCache:  "{home}/.xdev/agent/profiles/work",
		},
		{
			name:       "profile flag beats env",
			profile:    "flag",
			envProfile: "env",
			wantData:   "{home}/.xdev/agent/profiles/flag",
			wantState:  "{home}/.xdev/agent/profiles/flag",
			wantCache:  "{home}/.xdev/agent/profiles/flag",
		},
		{
			name:      "profile under agent dir",
			profile:   "work",
			agentDir:  true,
			wantData:  "{agent}/profiles/work",
			wantState: "{agent}/profiles/work",
			wantCache: "{agent}/profiles/work",
		},
		{
			name:      "profile nests under the xdg data root",
			profile:   "work",
			initXDG:   true,
			wantData:  "{xdg}/data/xdev/profiles/work",
			wantState: "{xdg}/state/xdev/profiles/work",
			wantCache: "{xdg}/cache/xdev/profiles/work",
		},
		{
			name:       "blank env profile is the default base",
			envProfile: "   ",
			wantData:   "{home}/.xdev/agent",
			wantState:  "{home}/.xdev/agent",
			wantCache:  "{home}/.xdev/agent",
		},
		{
			name:       "reserved default profile is the default base",
			envProfile: "default",
			wantData:   "{home}/.xdev/agent",
			wantState:  "{home}/.xdev/agent",
			wantCache:  "{home}/.xdev/agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			xdg := t.TempDir()
			defaultRootEnv(t, home)
			t.Setenv("XDG_DATA_HOME", filepath.Join(xdg, "data"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(xdg, "state"))
			t.Setenv("XDG_CACHE_HOME", filepath.Join(xdg, "cache"))
			agentDir := ""
			if tc.agentDir {
				agentDir = filepath.Join(t.TempDir(), "sandbox")
				t.Setenv("XDEV_AGENT_DIR", agentDir)
			}
			if tc.initXDG {
				if err := InitXDG(ResolveXDGRoots()); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("XDEV_PROFILE", tc.envProfile)
			if err := SetProfile(tc.profile); err != nil {
				t.Fatal(err)
			}
			expand := strings.NewReplacer("{home}", home, "{xdg}", xdg, "{agent}", agentDir).Replace
			if got := DataDir(); got != expand(tc.wantData) {
				t.Errorf("DataDir() = %q, want %q", got, expand(tc.wantData))
			}
			if got := StateDir(); got != expand(tc.wantState) {
				t.Errorf("StateDir() = %q, want %q", got, expand(tc.wantState))
			}
			if got := CacheDir(); got != expand(tc.wantCache) {
				t.Errorf("CacheDir() = %q, want %q", got, expand(tc.wantCache))
			}
		})
	}
}

// TestProfilesIsolateState: two profiles — and the default base — share no
// session directory and no settings file.
func TestProfilesIsolateState(t *testing.T) {
	home := t.TempDir()
	defaultRootEnv(t, home)

	sessions := map[string]string{}
	settings := map[string]string{}
	for _, p := range []string{"work", "personal"} {
		if err := SetProfile(p); err != nil {
			t.Fatal(err)
		}
		sessions[p] = filepath.Join(DataDir(), "sessions")
		settings[p] = GlobalSettingsPath()
	}
	if err := SetProfile(""); err != nil {
		t.Fatal(err)
	}
	sessions[""] = filepath.Join(DataDir(), "sessions")
	settings[""] = GlobalSettingsPath()

	seen := map[string]string{}
	for _, p := range []string{"", "work", "personal"} {
		if other, dup := seen[sessions[p]]; dup {
			t.Errorf("profiles %q and %q share the sessions dir %q", p, other, sessions[p])
		}
		seen[sessions[p]] = p
		if sessions[p] != filepath.Join(settings[p], "..", "sessions") {
			t.Errorf("profile %q: sessions %q is not under the data dir", p, sessions[p])
		}
	}
	for _, p := range []string{"work", "personal"} {
		if settings[p] == settings[""] {
			t.Errorf("profile %q shares the settings file %q with the default base", p, settings[p])
		}
	}
}

// TestXDGInitializedIsOptIn: the record, its permissions, and the fallback
// to the legacy base when the record is unusable.
func TestXDGInitializedIsOptIn(t *testing.T) {
	home := t.TempDir()
	defaultRootEnv(t, home)

	if _, ok := XDGInitialized(); ok {
		t.Fatal("XDG must be inactive until init-xdg runs")
	}
	if got := DataDir(); got != filepath.Join(home, ".xdev", "agent") {
		t.Fatalf("DataDir() = %q, want the legacy base", got)
	}

	want := XDGRoots{
		Data:  filepath.Join(home, "share", "xdev"),
		State: filepath.Join(home, "state", "xdev"),
		Cache: filepath.Join(home, "cache", "xdev"),
	}
	if err := InitXDG(want); err != nil {
		t.Fatal(err)
	}
	got, ok := XDGInitialized()
	if !ok || got != want {
		t.Fatalf("XDGInitialized() = %+v, %v; want %+v, true", got, ok, want)
	}
	for _, dir := range []string{want.Data, want.State, want.Cache} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("root %s not created: %v", dir, err)
		}
	}
	info, err := os.Stat(xdgRecordPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %o, want 600", perm)
	}
	if got := DataDir(); got != want.Data {
		t.Errorf("DataDir() = %q, want %q", got, want.Data)
	}

	if err := os.WriteFile(xdgRecordPath(), []byte("half a record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := XDGInitialized(); ok {
		t.Error("a malformed record must fall back to the legacy base")
	}
}

// TestInitXDGCommand drives the subcommand end to end over $XDG_*_HOME.
func TestInitXDGCommand(t *testing.T) {
	home := t.TempDir()
	defaultRootEnv(t, home)
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(xdg, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(xdg, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(xdg, "cache"))

	var stdout, stderr bytes.Buffer
	if code := InitXDGCommand(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	want := ResolveXDGRoots()
	if got := DataDir(); got != want.Data {
		t.Errorf("DataDir() = %q, want %q", got, want.Data)
	}
	if got := StateDir(); got != want.State {
		t.Errorf("StateDir() = %q, want %q", got, want.State)
	}
	if got := CacheDir(); got != want.Cache {
		t.Errorf("CacheDir() = %q, want %q", got, want.Cache)
	}
	for _, want := range []string{want.Data, want.State, want.Cache, xdgRecordPath(), "base precedence"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, stdout.String())
		}
	}
}

// TestSetProfileRejectsUnsafeNames: the name becomes a path element, so it
// is validated once at the boundary and never escapes the base dir.
func TestSetProfileRejectsUnsafeNames(t *testing.T) {
	home := t.TempDir()
	defaultRootEnv(t, home)
	base := DataDir()

	for _, bad := range []string{"../escape", "a/b", ".", "..", "sp ace", strings.Repeat("x", 65)} {
		if err := SetProfile(bad); err == nil {
			t.Errorf("SetProfile(%q) accepted", bad)
		}
		if got := DataDir(); got != base {
			t.Fatalf("a rejected profile moved the base to %q", got)
		}
	}
	if err := SetProfile("ok-1.2"); err != nil {
		t.Fatal(err)
	}
	if got, want := DataDir(), filepath.Join(base, "profiles", "ok-1.2"); got != want {
		t.Errorf("DataDir() = %q, want %q", got, want)
	}
}

// TestSetAlias: the alias is this session's agent identity name, validated
// like a profile name because it names the identity file.
func TestSetAlias(t *testing.T) {
	t.Cleanup(func() { _ = SetAlias("") })
	if err := SetAlias("worker-1"); err != nil {
		t.Fatal(err)
	}
	if got := Alias(); got != "worker-1" {
		t.Fatalf("Alias() = %q", got)
	}
	if err := SetAlias("bad name"); err == nil {
		t.Error("an unsafe alias was accepted")
	}
	if got := Alias(); got != "worker-1" {
		t.Errorf("a rejected alias replaced the current one: %q", got)
	}
	if err := SetAlias("  "); err != nil {
		t.Fatal(err)
	}
	if got := Alias(); got != "" {
		t.Errorf("Alias() = %q, want empty", got)
	}
}
