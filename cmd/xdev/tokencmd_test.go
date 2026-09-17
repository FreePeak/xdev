package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/serve"
)

// tokenHarness runs tokenCmd against a throwaway data dir. XDEV_AGENT_DIR is
// pinned as well, so no code path in the command can fall back to the
// operator's real install.
func tokenHarness(t *testing.T) (string, func(args ...string) (int, string, string)) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := tokenCmd(args, dir, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	return dir, run
}

// tokenValue pulls the 64-hex value a rotation printed for one service. It
// deliberately parses the human output: the value an operator copies must be
// the value on disk, so the test goes through the same text they read.
func tokenRow(t *testing.T, output, service string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == service {
			return line
		}
	}
	t.Fatalf("no %q row in output:\n%s", service, output)
	return ""
}

// tokenValue extracts the value a rotation printed for one service.
func tokenValue(t *testing.T, output, service string) string {
	t.Helper()
	if fields := strings.Fields(tokenRow(t, output, service)); len(fields) == 2 {
		return fields[1]
	}
	t.Fatalf("row for %q is not `service token`:\n%s", service, output)
	return ""
}

func tokenAssertHex(t *testing.T, tok string) {
	t.Helper()
	if len(tok) != 64 {
		t.Fatalf("token %q has %d chars, want 64 hex chars", tok, len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Fatalf("token %q is not hex: %v", tok, err)
	}
}

func tokenReadMinted(t *testing.T, dir, service string) string {
	t.Helper()
	b, err := os.ReadFile(serve.TokenPath(dir, service))
	if err != nil {
		t.Fatalf("read %s: %v", serve.TokenPath(dir, service), err)
	}
	return strings.TrimSpace(string(b))
}

// (a) Nothing minted: every service is reported unminted and listing must not
// mint a token as a side effect (it is the read-only verb).
func TestTokenListNothingMinted(t *testing.T) {
	dir, run := tokenHarness(t)

	code, out, errOut := run("list")
	if code != 0 {
		t.Fatalf("list exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	services := serve.Services()
	if got := strings.Count(out, "not minted"); got != len(services) {
		t.Errorf("list reported %d unminted rows, want %d:\n%s", got, len(services), out)
	}
	for _, s := range services {
		if !strings.Contains(out, s) {
			t.Errorf("list output lacks service %q:\n%s", s, out)
		}
		if !strings.Contains(out, serve.TokenPath(dir, s)) {
			t.Errorf("list output lacks token path for %q:\n%s", s, out)
		}
		if _, err := os.Stat(serve.TokenPath(dir, s)); !os.IsNotExist(err) {
			t.Errorf("list minted %s (stat err = %v)", serve.TokenPath(dir, s), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "serve")); !os.IsNotExist(err) {
		t.Errorf("list created the serve dir: %v", err)
	}

	// The bare verb defaults to list.
	if code, out2, _ := run(); code != 0 || out2 != out {
		t.Errorf("`token` (default verb) = exit %d output %q, want list output", code, out2)
	}
}

// (b) rotate mints a 64-hex token, prints exactly the value it wrote, and the
// file is private from creation (0600) — this is a bearer secret.
func TestTokenRotateMintsPrintedPrivateToken(t *testing.T) {
	dir, run := tokenHarness(t)

	code, out, errOut := run("rotate", "auth-gateway")
	if code != 0 {
		t.Fatalf("rotate exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	tok := tokenValue(t, out, "auth-gateway")
	tokenAssertHex(t, tok)

	path := serve.TokenPath(dir, "auth-gateway")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode = %o, want 600", path, perm)
	}
	if got := tokenReadMinted(t, dir, "auth-gateway"); got != tok {
		t.Errorf("file holds %q, printed %q", got, tok)
	}
	// Rotation is only effective on restart: the notice must name the service
	// to restart, otherwise the operator keeps the stale token in a daemon.
	if !strings.Contains(out, "keeps the token it loaded at startup") {
		t.Errorf("rotate output lacks the restart caveat:\n%s", out)
	}
	if !strings.Contains(out, "xdev serve auth-gateway") {
		t.Errorf("rotate output lacks the restart command:\n%s", out)
	}
	for _, other := range []string{"auth-broker", "browser-relay"} {
		if _, err := os.Stat(serve.TokenPath(dir, other)); !os.IsNotExist(err) {
			t.Errorf("rotate auth-gateway also touched %s: %v", other, err)
		}
	}
}

// (c) After a rotation the list reports the service minted and masks the
// secret: the full value must not be recoverable from listing output (in text
// or in --json), which is what makes `list` safe to paste around.
func TestTokenListMasksAfterRotate(t *testing.T) {
	dir, run := tokenHarness(t)

	if code, _, errOut := run("rotate", "auth-gateway"); code != 0 {
		t.Fatalf("rotate exit = %d (stderr: %s)", code, errOut)
	}
	tok := tokenReadMinted(t, dir, "auth-gateway")
	masked := tok[:4] + "…" + tok[len(tok)-4:]

	code, out, errOut := run("list")
	if code != 0 {
		t.Fatalf("list exit = %d (stderr: %s)", code, errOut)
	}
	if strings.Contains(out, tok) {
		t.Errorf("list leaked the full token:\n%s", out)
	}
	if !strings.Contains(out, masked) {
		t.Errorf("list lacks masked token %q:\n%s", masked, out)
	}
	row := tokenRow(t, out, "auth-gateway")
	if !strings.Contains(row, "minted") || strings.Contains(row, "not minted") {
		t.Errorf("auth-gateway row does not read as minted: %q", row)
	}
	if !strings.Contains(row, masked) {
		t.Errorf("auth-gateway row lacks the masked token: %q", row)
	}
	if got := strings.Count(out, "not minted"); got != len(serve.Services())-1 {
		t.Errorf("list reports %d unminted rows, want %d:\n%s", got, len(serve.Services())-1, out)
	}

	code, out, errOut = run("list", "--json")
	if code != 0 {
		t.Fatalf("list --json exit = %d (stderr: %s)", code, errOut)
	}
	if strings.Contains(out, tok) {
		t.Errorf("JSON list leaked the full token:\n%s", out)
	}
	var rows []struct {
		Service string `json:"service"`
		Listen  string `json:"listen"`
		Path    string `json:"path"`
		Minted  bool   `json:"minted"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out)
	}
	if len(rows) != len(serve.Services()) {
		t.Fatalf("JSON rows = %d, want %d", len(rows), len(serve.Services()))
	}
	seen := false
	for _, r := range rows {
		if r.Listen == "" || r.Path != serve.TokenPath(dir, r.Service) {
			t.Errorf("row %+v has wrong listen/path", r)
		}
		if r.Service != "auth-gateway" {
			if r.Minted || r.Token != "" {
				t.Errorf("unminted %s reported minted=%v token=%q", r.Service, r.Minted, r.Token)
			}
			continue
		}
		seen = true
		if !r.Minted || r.Token != masked {
			t.Errorf("auth-gateway row = minted %v token %q, want minted true %q", r.Minted, r.Token, masked)
		}
	}
	if !seen {
		t.Error("JSON list is missing the auth-gateway row")
	}
}

// (d) show prints the secret in full — that is its whole purpose — for a named
// service on a line scripts can capture.
func TestTokenShowPrintsFullToken(t *testing.T) {
	dir, run := tokenHarness(t)

	if code, _, errOut := run("rotate", "auth-gateway"); code != 0 {
		t.Fatalf("rotate exit = %d (stderr: %s)", code, errOut)
	}
	tok := tokenReadMinted(t, dir, "auth-gateway")

	code, out, errOut := run("show", "auth-gateway")
	if code != 0 {
		t.Fatalf("show exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if strings.TrimSpace(out) != tok {
		t.Errorf("show printed %q, want the bare token %q", out, tok)
	}

	// Naming every service prints each minted value with its owner.
	code, out, errOut = run("show")
	if code != 0 {
		t.Fatalf("show (all) exit = %d (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, tok) {
		t.Errorf("show (all) omits the minted token:\n%s", out)
	}
	if !strings.Contains(out, "auth-gateway") {
		t.Errorf("show (all) does not name the service:\n%s", out)
	}
}

// show must not invent a value, and it must not mint one either: the unminted
// case is an explanation of how to get a token.
func TestTokenShowUnmintedExplains(t *testing.T) {
	dir, run := tokenHarness(t)

	code, out, errOut := run("show", "browser-relay")
	if code != 0 {
		t.Fatalf("show exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	for _, want := range []string{"not minted", "xdev serve browser-relay", "xdev token rotate browser-relay"} {
		if !strings.Contains(out, want) {
			t.Errorf("unminted show output lacks %q:\n%s", want, out)
		}
	}

	code, out, errOut = run("show")
	if code != 0 {
		t.Fatalf("show (all) exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "No local gateway token is configured yet") {
		t.Errorf("show (all) does not explain the empty state:\n%s", out)
	}
	if !strings.Contains(out, "xdev token rotate") || !strings.Contains(out, tokenDir(dir)) {
		t.Errorf("show (all) does not say how to mint or where tokens live:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "serve")); !os.IsNotExist(err) {
		t.Errorf("show minted a token dir: %v", err)
	}
}

// (e) An unknown service is a usage error naming the services that do exist,
// and it must not touch the filesystem on the way out.
func TestTokenUnknownService(t *testing.T) {
	dir, run := tokenHarness(t)

	for _, verb := range []string{"show", "rotate"} {
		code, out, errOut := run(verb, "nope")
		if code != 2 {
			t.Errorf("%s nope exit = %d, want 2 (stdout: %s stderr: %s)", verb, code, out, errOut)
		}
		if !strings.Contains(errOut, `unknown service "nope"`) {
			t.Errorf("%s nope stderr lacks the error: %s", verb, errOut)
		}
		for _, s := range serve.Services() {
			if !strings.Contains(errOut, s) {
				t.Errorf("%s nope stderr does not name known service %q: %s", verb, s, errOut)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "serve")); !os.IsNotExist(err) {
			t.Errorf("%s nope created something in the data dir: %v", verb, err)
		}
	}
}

// (f) A rotation that returned the same secret would be worse than no
// rotation: the new value must differ and be the one on disk.
func TestTokenRotateTwiceDiffers(t *testing.T) {
	dir, run := tokenHarness(t)

	code, firstOut, errOut := run("rotate", "auth-gateway")
	if code != 0 {
		t.Fatalf("first rotate exit = %d (stderr: %s)", code, errOut)
	}
	first := tokenValue(t, firstOut, "auth-gateway")

	code, secondOut, errOut := run("rotate", "auth-gateway")
	if code != 0 {
		t.Fatalf("second rotate exit = %d (stderr: %s)", code, errOut)
	}
	second := tokenValue(t, secondOut, "auth-gateway")
	tokenAssertHex(t, second)

	if first == second {
		t.Fatalf("rotate returned the same token twice: %q", first)
	}
	if got := tokenReadMinted(t, dir, "auth-gateway"); got != second {
		t.Errorf("file holds %q, want the newest rotation %q", got, second)
	}
}

// `--all` (and the no-service default) rotates the whole fixed service table,
// each service getting its own distinct secret.
func TestTokenRotateAllServices(t *testing.T) {
	dir, run := tokenHarness(t)

	code, out, errOut := run("rotate", "--all")
	if code != 0 {
		t.Fatalf("rotate --all exit = %d (stderr: %s)", code, errOut)
	}
	tokens := map[string]string{}
	for _, s := range serve.Services() {
		tok := tokenValue(t, out, s)
		tokenAssertHex(t, tok)
		if got := tokenReadMinted(t, dir, s); got != tok {
			t.Errorf("%s file holds %q, printed %q", s, got, tok)
		}
		tokens[s] = tok
	}
	if len(tokens) != len(serve.Services()) {
		t.Errorf("rotated %d services, want %d", len(tokens), len(serve.Services()))
	}
	for a, ta := range tokens {
		for b, tb := range tokens {
			if a != b && ta == tb {
				t.Errorf("%s and %s share token %q", a, b, ta)
			}
		}
	}

	// Verb-without-service is the same operation.
	code, out, errOut = run("rotate")
	if code != 0 {
		t.Fatalf("rotate (no service) exit = %d (stderr: %s)", code, errOut)
	}
	for _, s := range serve.Services() {
		if tokenValue(t, out, s) == tokens[s] {
			t.Errorf("%s token unchanged by a second rotation", s)
		}
	}
}

// Flag/verb mismatches are usage errors, not silently ignored flags: a typo
// must never look like it worked.
func TestTokenUsageErrors(t *testing.T) {
	_, run := tokenHarness(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"json with show", []string{"show", "--json"}, "--json only applies to list"},
		{"json with rotate", []string{"rotate", "--json"}, "--json only applies to list"},
		{"all with list", []string{"list", "--all"}, "--all only applies to rotate"},
		{"all with show", []string{"show", "--all"}, "--all only applies to rotate"},
		{"unknown verb", []string{"frobnicate"}, `unknown verb "frobnicate"`},
		{"extra argument", []string{"show", "auth-gateway", "extra"}, "unexpected argument"},
		{"service with list", []string{"list", "auth-gateway"}, "list takes no service"},
	}
	for _, tc := range cases {
		code, out, errOut := run(tc.args...)
		if code != 2 {
			t.Errorf("%s: exit = %d, want 2 (stdout: %s)", tc.name, code, out)
		}
		if !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: stderr %q lacks %q", tc.name, errOut, tc.want)
		}
	}
}

// tokenMask is the only thing standing between a secret and a pasted table,
// so its boundary behaviour is pinned.
func TestTokenMask(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "-"},
		{"short", "…"},
		{"12345678", "…"},
		{"123456789", "1234…6789"},
		{"abcdef0123456789", "abcd…6789"},
	}
	for _, tc := range cases {
		if got := tokenMask(tc.in); got != tc.want {
			t.Errorf("tokenMask(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// `help` must succeed and describe every verb and flag: it is the only place
// an operator learns the verbs exist, so a verb that vanished from the help
// would be a verb nobody runs.
func TestTokenHelp(t *testing.T) {
	dir, run := tokenHarness(t)

	code, out, errOut := run("help")
	if code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("help wrote to stdout: %q", out)
	}
	for _, want := range []string{"usage: xdev token", "list", "show", "rotate", "-json", "-all", tokenDir(dir)} {
		if !strings.Contains(errOut, want) {
			t.Errorf("help lacks %q:\n%s", want, errOut)
		}
	}
}
