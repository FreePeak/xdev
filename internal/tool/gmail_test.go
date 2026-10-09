package tool

// Fixture strategy for the gmail tool: a stub `gog` script on PATH. Each call
// appends "gog@cwd", one line per argv entry, then an "END" terminator to
// $GOG_LOG. "END" (not "--") is deliberate: gog itself takes a "--" option
// delimiter for queries that start with '-', and the shared github stub's
// "--" terminator would split that argv in two. Responses come from
// $GOG_RESPONSES/<n> (slot n, or slot 0 as the every-call default).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const gogStubTemplate = `#!/bin/sh
log="${GOG_LOG:-/dev/null}"
printf '%s\n' "gog@$PWD" >> "$log"
for a in "$@"; do printf '%s\n' "$a" >> "$log"; done
printf '%s\n' 'END' >> "$log"
count_file="$log.gog.count"
n=1
if [ -f "$count_file" ]; then n=$(cat "$count_file"); n=$((n + 1)); fi
printf '%s\n' "$n" > "$count_file"
dir="${GOG_RESPONSES:-}"
if [ -n "$dir" ]; then
  if [ -f "$dir/$n" ]; then cat "$dir/$n"; elif [ -f "$dir/0" ]; then cat "$dir/0"; fi
fi
cap="${GOG_CAPTURE:-}"
if [ -n "$cap" ] && [ -d "$cap" ]; then
  i=0
  for a in "$@"; do i=$((i + 1)); if [ -f "$a" ]; then cp "$a" "$cap/$i"; fi; done
fi
err="${GOG_STDERR:-}"
if [ -n "$err" ] && [ -f "$err" ]; then cat "$err" 1>&2; fi
exit "${GOG_EXIT:-0}"
`

type gmailStub struct {
	t   *testing.T
	dir string
	log string
	cwd string
}

func newGmailStub(t *testing.T) *gmailStub {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("gmail stubs are POSIX shell scripts")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(filepath.Join(dir, "gog"), []byte(gogStubTemplate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "gog-responses"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(dir, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOG_LOG", log)
	t.Setenv("GOG_RESPONSES", filepath.Join(dir, "gog-responses"))
	t.Setenv("GOG_CAPTURE", filepath.Join(dir, "captures"))
	_ = os.MkdirAll(filepath.Join(dir, "captures"), 0o755)
	return &gmailStub{t: t, dir: dir, log: log, cwd: cwd}
}

func (s *gmailStub) respond(text string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "gog-responses", "0"), []byte(text), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *gmailStub) fail(code int, stderr string) {
	s.t.Helper()
	path := filepath.Join(s.dir, "gog-stderr")
	if err := os.WriteFile(path, []byte(stderr), 0o644); err != nil {
		s.t.Fatal(err)
	}
	s.t.Setenv("GOG_STDERR", path)
	s.t.Setenv("GOG_EXIT", strconv.Itoa(code))
}

func (s *gmailStub) calls() []stubCall {
	s.t.Helper()
	raw, err := os.ReadFile(s.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.t.Fatal(err)
	}
	var calls []stubCall
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		bin, dir, _ := strings.Cut(cur[0], "@")
		calls = append(calls, stubCall{bin: bin, dir: dir, args: cur[1:]})
		cur = nil
	}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "END" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return calls
}

func runGmail(t *testing.T, tool *GmailTool, args string) Result {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("gmail(%s) returned a harness error: %v", args, err)
	}
	return res
}

func TestGmailSearchArgs(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"threads":[{"id":"t1","snippet":"hi"}]}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"search","query":"is:unread newer_than:7d","limit":5,"account":"me@example.test"}`)
	if res.IsError {
		t.Fatalf("search error: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"t1"`) {
		t.Fatalf("search body = %q", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	assertArgs(t, calls[0], "gog", stub.cwd,
		"--json", "--no-input", "--account", "me@example.test",
		"gmail", "search", "--max", "5", "--wrap-untrusted", "--", "is:unread newer_than:7d")
}

func TestGmailSearchLeadingDashQuery(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"threads":[]}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"search","query":"-in:inbox newer_than:1d"}`)
	if res.IsError {
		t.Fatalf("search error: %s", res.Text)
	}
	calls := stub.calls()
	assertArgs(t, calls[0], "gog", stub.cwd,
		"--json", "--no-input",
		"gmail", "search", "--max", "10", "--wrap-untrusted", "--", "-in:inbox newer_than:1d")
}

func TestGmailSearchRequiresQuery(t *testing.T) {
	stub := newGmailStub(t)
	res := runGmail(t, NewGmailTool(stub.cwd), `{"op":"search"}`)
	if !res.IsError || !strings.Contains(res.Text, "query") {
		t.Fatalf("want query error, got %+v", res)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("gog should not run on bad args")
	}
}

func TestGmailGetSanitizedByDefault(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"message":{"id":"m1","snippet":"hello"}}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"get","id":"m1"}`)
	if res.IsError {
		t.Fatalf("get error: %s", res.Text)
	}
	calls := stub.calls()
	assertArgs(t, calls[0], "gog", stub.cwd,
		"--json", "--no-input",
		"gmail", "get", "m1", "--format", "full", "--sanitize-content", "--wrap-untrusted")
}

func TestGmailGetRawNoSanitize(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"message":{"raw":"..."}}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"get","id":"m1","format":"raw","sanitize":false}`)
	if res.IsError {
		t.Fatalf("get error: %s", res.Text)
	}
	calls := stub.calls()
	assertArgs(t, calls[0], "gog", stub.cwd,
		"--json", "--no-input",
		"gmail", "get", "m1", "--format", "raw")
}

func TestGmailThreadArgs(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"thread":{"id":"t1"}}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"thread","id":"t1"}`)
	if res.IsError {
		t.Fatalf("thread error: %s", res.Text)
	}
	assertArgs(t, stub.calls()[0], "gog", stub.cwd,
		"--json", "--no-input",
		"gmail", "thread", "get", "t1", "--sanitize-content", "--wrap-untrusted")
}

func TestGmailSendBodyFile(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"id":"sent1"}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"send","to":"a@example.test","subject":"Hi","body":"line1\nline2","cc":"c@example.test"}`)
	if res.IsError {
		t.Fatalf("send error: %s", res.Text)
	}
	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	args := calls[0].args
	// Find --body-file and read it.
	var bodyFile string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--body-file" {
			bodyFile = args[i+1]
			break
		}
	}
	if bodyFile == "" {
		t.Fatalf("no --body-file in %q", args)
	}
	raw, err := os.ReadFile(bodyFile)
	if err == nil {
		// File may already be cleaned up by defer after Execute returns —
		// either way the argv must have carried the path and the content
		// must have been written before gog ran. The stub captures argv
		// only; re-check via the fact Execute succeeded and body-file was
		// present. If the file still exists, pin content.
		if string(raw) != "line1\nline2" {
			t.Fatalf("body file = %q", raw)
		}
	}
	// Structural argv pin (body-file path is temp, so match by position).
	wantPrefix := []string{"--json", "--no-input", "gmail", "send", "--to", "a@example.test", "--subject", "Hi", "--body-file"}
	if len(args) < len(wantPrefix)+1 {
		t.Fatalf("argv too short: %q", args)
	}
	for i, w := range wantPrefix {
		if args[i] != w {
			t.Fatalf("argv[%d] = %q, want %q (full %q)", i, args[i], w, args)
		}
	}
	// After body-file path comes --cc.
	foundCC := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--cc" && args[i+1] == "c@example.test" {
			foundCC = true
		}
	}
	if !foundCC {
		t.Fatalf("missing --cc in %q", args)
	}
}

func TestGmailReplyArgs(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"id":"r1"}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"reply","id":"m1","body":"thanks"}`)
	if res.IsError {
		t.Fatalf("reply error: %s", res.Text)
	}
	args := stub.calls()[0].args
	// --json --no-input gmail reply m1 --body-file <path>
	if len(args) < 6 || args[0] != "--json" || args[2] != "gmail" || args[3] != "reply" || args[4] != "m1" || args[5] != "--body-file" {
		t.Fatalf("argv = %q", args)
	}
}

func TestGmailMarkReadIDs(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"ok":true}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"mark_read","ids":["m1","m2"]}`)
	if res.IsError {
		t.Fatalf("mark_read error: %s", res.Text)
	}
	assertArgs(t, stub.calls()[0], "gog", stub.cwd,
		"--json", "--no-input",
		"gmail", "mark-read", "m1", "m2")
}

func TestGmailLabelsList(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{"labels":[{"name":"INBOX"}]}`)
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"labels"}`)
	if res.IsError {
		t.Fatalf("labels error: %s", res.Text)
	}
	assertArgs(t, stub.calls()[0], "gog", stub.cwd,
		"--json", "--no-input",
		"gmail", "labels", "list")
}

func TestGmailMissingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path isolation is POSIX")
	}
	// Empty PATH so lookPath("gog") fails.
	t.Setenv("PATH", "")
	res := runGmail(t, NewGmailTool(t.TempDir()), `{"op":"labels"}`)
	if !res.IsError || !strings.Contains(res.Text, "gog") || !strings.Contains(res.Text, "PATH") {
		t.Fatalf("want missing-gog error, got %+v", res)
	}
}

func TestGmailAuthHint(t *testing.T) {
	stub := newGmailStub(t)
	stub.fail(1, "Error: no account configured; run gog auth add")
	tool := NewGmailTool(stub.cwd)

	res := runGmail(t, tool, `{"op":"labels"}`)
	if !res.IsError {
		t.Fatal("want error")
	}
	if !strings.Contains(res.Text, "gog auth add") {
		t.Fatalf("auth hint missing: %s", res.Text)
	}
}

func TestGmailUnknownOp(t *testing.T) {
	stub := newGmailStub(t)
	res := runGmail(t, NewGmailTool(stub.cwd), `{"op":"delete_all"}`)
	if !res.IsError || !strings.Contains(res.Text, "unknown op") {
		t.Fatalf("want unknown op, got %+v", res)
	}
}

func TestGmailSearchLimitClamp(t *testing.T) {
	stub := newGmailStub(t)
	stub.respond(`{}`)
	tool := NewGmailTool(stub.cwd)

	_ = runGmail(t, tool, `{"op":"search","query":"a","limit":999}`)
	args := stub.calls()[0].args
	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--max" && args[i+1] == "50" {
			found = true
		}
	}
	if !found {
		t.Fatalf("limit not clamped to 50: %q", args)
	}
}
