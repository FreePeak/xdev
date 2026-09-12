package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/serve"
)

// psFixture is a frozen `ps -axo pid=,ppid=,etime=,stat=,command=` capture: an
// xdev daemon, a TUI, a one-shot print run, and unrelated host processes. It is
// fixed text (not a live probe) so the classification assertions describe this
// code rather than whatever happens to be running on the test machine. The
// daemon deliberately passes a NON-default --listen: the table must report the
// service's default bind, which only the serve catalog knows, so a test that
// could read the flag by accident would not prove anything.
const psFixture = `    1     0 21-04:05:06 Ss   /sbin/launchd
  501   501 02:03:04 S    /Applications/Visual Studio Code.app/MacOS/Electron
 4812  4811 01:02:03 S    vim README.md
 7331     1 12:00:00 S    /Users/dev/.local/bin/xdev serve auth-gateway --listen 127.0.0.1:9999
 7332  7331 11:59:00 S    /Users/dev/.local/bin/xdev tui
 7444  7331 00:12:34 S    /Users/dev/.local/bin/xdev print "summarize the diff"
`

// psInject swaps the host probe for the duration of one test. Every test that
// touches psHostProcs must use this so the real `ps` is never executed and the
// package-level seam cannot leak into the next test.
func psInject(t *testing.T, out string, err error) {
	t.Helper()
	old := psHostProcs
	psHostProcs = func() (string, error) { return out, err }
	t.Cleanup(func() { psHostProcs = old })
}

// psRun drives psCmd end to end, capturing both streams — the split matters
// because --json must keep stdout pure JSON.
func psRun(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = psCmd(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// TestPSListsXdevProcessesOnly is the core contract: unrelated host processes
// are dropped, each xdev row is classified, and a serve row carries its
// service's default listen address and token fact.
func TestPSListsXdevProcessesOnly(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	// Mint the gateway token so the serve row can prove the present case.
	if _, err := serve.EnsureToken(dataDir, "auth-gateway"); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}

	psInject(t, psFixture, nil)
	stdout, stderr, code := psRun(t, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "not visible from here") {
		t.Errorf("--json must still state the ceiling on stderr; got %q", stderr)
	}

	var rows []psRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("--json output is not a JSON array: %v\n%s", err, stdout)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (the xdev processes only): %+v", len(rows), rows)
	}

	byPID := map[int]psRow{}
	for _, r := range rows {
		byPID[r.PID] = r
	}

	daemon, ok := byPID[7331]
	if !ok {
		t.Fatalf("serve row missing: %+v", rows)
	}
	if daemon.Kind != psKindDaemon {
		t.Errorf("serve kind = %q, want %q", daemon.Kind, psKindDaemon)
	}
	if daemon.PPID != 1 || daemon.Elapsed != "12:00:00" {
		t.Errorf("serve ppid/elapsed = %d/%q, want 1/12:00:00", daemon.PPID, daemon.Elapsed)
	}
	if daemon.Service != "auth-gateway" {
		t.Errorf("serve service = %q, want auth-gateway", daemon.Service)
	}
	// The default bind comes from the serve catalog, not from the --listen the
	// process was started with — that is the point of reporting it.
	if daemon.Listen != "127.0.0.1:4000" {
		t.Errorf("serve listen = %q, want the gateway default 127.0.0.1:4000", daemon.Listen)
	}
	if !daemon.TokenMinted {
		t.Error("serve row must report the minted token")
	}
	if !strings.Contains(daemon.Command, "serve auth-gateway") {
		t.Errorf("serve command = %q, want the invocation", daemon.Command)
	}

	if got := byPID[7332].Kind; got != psKindInteractive {
		t.Errorf("tui kind = %q, want %q", got, psKindInteractive)
	}
	// `xdev print "..."` carries an explicit subcommand, so it is labelled with
	// it — (prompt) is reserved for an invocation with no subcommand.
	if got := byPID[7444].Kind; got != "print" {
		t.Errorf("print kind = %q, want print", got)
	}
	if got := byPID[7444].Command; !strings.Contains(got, "summarize the diff") {
		t.Errorf("print row command = %q, want the full invocation", got)
	}

	for _, r := range rows {
		if strings.Contains(r.Command, "vim") || strings.Contains(r.Command, "launchd") {
			t.Errorf("non-xdev process leaked into the listing: %+v", r)
		}
	}
}

// TestPSTableRendersRows proves the human table carries the columns and the
// daemon detail block, not just that a string was produced.
func TestPSTableRendersRows(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)

	psInject(t, psFixture, nil)
	stdout, _, code := psRun(t)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"PID", "PPID", "ELAPSED", "KIND", "COMMAND",
		"daemon", "interactive", "print",
		"3 xdev process(es)",
		// The table shows the real command line, trimmed and capped.
		"serve auth-gateway --listen 127.0.0.1:9999",
		// ...and the detail block shows the service's DEFAULT bind, which is
		// not the flag above.
		"listen   127.0.0.1:4000",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}
	// The token was never minted in this fresh data dir, so the daemon must say
	// so rather than silently omitting the fact.
	if !strings.Contains(stdout, "token file absent") {
		t.Errorf("table must report the unminted token:\n%s", stdout)
	}
	if strings.Contains(stdout, "vim") {
		t.Errorf("vim leaked into the table:\n%s", stdout)
	}
}

// TestPSNoProcessesSaysSo pins the honest-empty case: no rows must print the
// explanatory line, never a bare header.
func TestPSNoProcessesSaysSo(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())

	psInject(t, "    1     0 21-04:05:06 Ss   /sbin/launchd\n  501   501 02:03:04 S    vim notes.md\n", nil)
	stdout, stderr, code := psRun(t)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "no xdev process on this host") {
		t.Errorf("stdout must say no xdev process was found:\n%s", stdout)
	}
	if strings.Contains(stdout, "vim") {
		t.Errorf("non-xdev rows must not reach the table:\n%s", stdout)
	}
	if !strings.Contains(stderr, "not visible from here") {
		t.Errorf("the ceiling note must still print; got %q", stderr)
	}
}

// TestPSJSONShape pins the --json contract a script depends on: an array of
// objects with pid/kind/command, empty (not null) when nothing matches.
func TestPSJSONShape(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())

	psInject(t, " 7331     1 12:00:00 S    /opt/xdev serve auth-broker\n", nil)
	stdout, _, code := psRun(t, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "[") {
		t.Fatalf("--json stdout must be a JSON array:\n%s", stdout)
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if _, ok := rows[0]["pid"].(float64); !ok {
		t.Errorf("pid field missing or not a number: %+v", rows[0])
	}
	if got := rows[0]["kind"]; got != psKindDaemon {
		t.Errorf("kind = %v, want %q", got, psKindDaemon)
	}
	if got, _ := rows[0]["command"].(string); !strings.Contains(got, "serve auth-broker") {
		t.Errorf("command = %v, want the invocation", rows[0]["command"])
	}
	if got := rows[0]["listen"]; got != "127.0.0.1:8765" {
		t.Errorf("listen = %v, want the broker default 127.0.0.1:8765", got)
	}

	psInject(t, "", nil)
	stdout, _, code = psRun(t, "--json")
	if code != 0 {
		t.Fatalf("empty exit = %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("empty listing must encode as [], got %q", strings.TrimSpace(stdout))
	}
}

// TestPSSkipsSelfProcess pins that the `xdev ps` reading the table is not a row
// in it — otherwise every run reports at least one process forever.
func TestPSSkipsSelfProcess(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())

	// 999999 stands in for a PID the fixture cannot know, so the row survives.
	raw := " 4812 1 00:00:01 S /usr/bin/xdev ps --json\n"
	rows := psParseTable(raw, 999999, config.DataDir())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 without a matching self pid", len(rows))
	}
	if got := rows[0].Kind; got != "ps" {
		t.Errorf("kind = %q, want ps", got)
	}
	// Re-parse with the reader's own PID: the row must disappear.
	if got := psParseTable(raw, rows[0].PID, config.DataDir()); len(got) != 0 {
		t.Errorf("the reading process must be skipped, got %+v", got)
	}

	// End to end the reader's PID is os.Getpid() (the test binary), which the
	// fixture cannot name, so inject only the row that must survive.
	psInject(t, " 4813     1 00:00:02 S    /usr/bin/xdev tui\n", nil)
	stdout, _, code := psRun(t, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var parsed []psRow
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed) != 1 || parsed[0].PID != 4813 {
		t.Fatalf("only the tui row should survive: %+v", parsed)
	}
	if got := parsed[0].Kind; got != psKindInteractive {
		t.Errorf("kind = %q, want %q", got, psKindInteractive)
	}
	if parsed[0].PID == os.Getpid() {
		t.Error("the fixture must not reuse this process's pid")
	}
}

// TestPSProbeFailureExitsOne pins the runtime-error branch: when the host probe
// fails the command reports it instead of printing a plausible empty list.
func TestPSProbeFailureExitsOne(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())

	psInject(t, "", os.ErrPermission)
	stdout, stderr, code := psRun(t)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on probe failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "xdev ps:") {
		t.Errorf("stderr must name the command: %q", stderr)
	}
}

// TestPSClassifyCoversModes pins the classification table itself: the kind a
// user reads is the whole point of the command, so each documented mode gets a
// row instead of being inferred from one end-to-end fixture.
func TestPSClassifyCoversModes(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := serve.EnsureToken(dataDir, "browser-relay"); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	tests := []struct {
		name     string
		argv     []string
		wantKind string
	}{
		{"serve known", []string{"/usr/local/bin/xdev", "serve", "browser-relay"}, psKindDaemon},
		{"serve missing name", []string{"xdev", "serve"}, psKindDaemon},
		{"tui", []string{"xdev", "tui"}, psKindInteractive},
		{"rpc", []string{"xdev", "rpc", "stdio"}, psKindWire},
		{"rpc bare", []string{"xdev", "rpc"}, psKindWire},
		{"acp", []string{"xdev", "acp"}, psKindWire},
		{"bare", []string{"xdev"}, psBareArg},
		{"prompt", []string{"xdev", "fix the tests"}, psPrompt},
		{"unknown first arg is a prompt", []string{"xdev", "fix", "the", "tests"}, psPrompt},
		{"config", []string{"xdev", "config", "list"}, "config"},
		{"ps itself", []string{"xdev", "ps", "--json"}, "ps"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, service, listen, hasToken := psClassify(tc.argv, dataDir)
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if tc.name != "serve known" {
				if service != "" || listen != "" || hasToken {
					t.Errorf("non-serve %s reported serve facts: %q %q %v", tc.name, service, listen, hasToken)
				}
				return
			}
			// The service's facts must be real: the listen address is the
			// catalog default and the token was minted above.
			if service != "browser-relay" || listen != "127.0.0.1:9223" || !hasToken {
				t.Errorf("serve facts = service %q listen %q token %v", service, listen, hasToken)
			}
		})
	}

	// An unknown service is a usage error, not a daemon: no invented facts.
	kind, service, listen, hasToken := psClassify([]string{"xdev", "serve", "nope"}, dataDir)
	if kind != "serve:nope" || service != "" || listen != "" || hasToken {
		t.Errorf("unknown service = kind %q service %q listen %q token %v", kind, service, listen, hasToken)
	}
}

// TestPSMatchesXdevBinaryPaths pins the filter that decides what counts as an
// xdev process: PATH lookups, absolute paths and ./ relative invocations.
func TestPSMatchesXdevBinaryPaths(t *testing.T) {
	tests := []struct {
		argv []string
		want bool
	}{
		{[]string{"xdev", "tui"}, true},
		{[]string{"/Users/dev/.local/bin/xdev", "serve", "auth-broker"}, true},
		{[]string{"./xdev", "print", "hi"}, true},
		{[]string{"/usr/bin/xdev-helper", "run"}, false},
		{[]string{"vim", "xdev.go"}, false},
		{nil, false},
	}
	for _, tc := range tests {
		if got := psIsXdevArgv(tc.argv); got != tc.want {
			t.Errorf("psIsXdevArgv(%v) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

// TestPSTruncateKeepsRunesIntact guards the table cell cap: a multi-byte
// command must be cut on a rune boundary, not mid-encoding.
func TestPSTruncateKeepsRunesIntact(t *testing.T) {
	if got := psTruncate("print", 10); got != "print" {
		t.Errorf("short cell = %q, want unchanged", got)
	}
	if got := psTruncate("0123456789", 5); got != "0123…" {
		t.Errorf("capped cell = %q, want 0123…", got)
	}
	// Four multi-byte runes capped at 3 runes must stay valid UTF-8.
	if got, want := psTruncate("日本語版", 3), "日本…"; got != want {
		t.Errorf("runes = %q, want %q", got, want)
	}
}

// TestPSParseDropsGarbledLines pins the trust boundary on foreign input: `ps`
// output is not ours, so rows with non-numeric pids or missing commands must be
// dropped rather than silently becoming PID 0 rows.
func TestPSParseDropsGarbledLines(t *testing.T) {
	dataDir := t.TempDir()
	raw := strings.Join([]string{
		"",
		"not-a-pid 1 00:01 S xdev tui",
		"12 not-a-pid 00:01 S xdev tui",
		"13 1 00:01 S",
		"14 1 00:01 S xdev tui",
	}, "\n")
	rows := psParseTable(raw, 999999, dataDir)
	if len(rows) != 1 || rows[0].PID != 14 {
		t.Fatalf("rows = %+v, want only pid 14", rows)
	}
	if rows[0].Command != "xdev tui" {
		t.Errorf("command = %q, want the trimmed command line", rows[0].Command)
	}
}

// TestPSAgentDirSandboxIsRespected documents that the token lookup follows
// XDEV_AGENT_DIR, so a test (or a profile) never reads the real install's
// token files.
func TestPSAgentDirSandboxIsRespected(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	if got := config.DataDir(); got != dataDir {
		t.Fatalf("DataDir = %q, want the sandbox %q", got, dataDir)
	}
	if _, err := serve.EnsureToken(dataDir, "auth-broker"); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	line := psTokenStateLine(dataDir, psRow{Service: "auth-broker", TokenMinted: true})
	if !strings.Contains(line, filepath.Join(dataDir, "serve", "auth-broker.token")) {
		t.Errorf("token line %q must name the sandboxed path", line)
	}
	if absent := psTokenStateLine(dataDir, psRow{Service: "auth-broker"}); !strings.Contains(absent, "absent") {
		t.Errorf("unminted token line = %q, want it to say absent", absent)
	}
}
