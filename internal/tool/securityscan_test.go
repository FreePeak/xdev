package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Fixture output in each scanner's native format, so the parsers are exercised
// against what the tools really emit rather than a shape invented here:
// govulncheck's streaming JSON messages (golang.org/x/vuln/internal/govulncheck),
// semgrep's CliMatch report, gitleaks' report.Finding array, and vet's text
// diagnostics.
const (
	fixtureGovulncheck = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.1.4","db":"https://vuln.go.dev","scan_level":"symbol","scan_mode":"source"}}
{"progress":{"message":"Scanning your code and 3 packages against the Go vulnerability database..."}}
{"osv":{"id":"GO-2024-2611","summary":"Infinite loop in JSON unmarshaling in google.golang.org/protobuf"}}
{"finding":{"osv":"GO-2024-2611","fixed_version":"1.33.0","trace":[{"module":"google.golang.org/protobuf","version":"v1.31.0","package":"google.golang.org/protobuf/encoding/protojson","function":"Unmarshal","position":{"filename":"/Users/x/go/pkg/mod/google.golang.org/protobuf@v1.31.0/encoding/protojson/decode.go","line":279}},{"module":"example.com/demo","package":"example.com/demo/loader","function":"Load","position":{"filename":"loader/load.go","line":42}}]}}
{"finding":{"osv":"GO-2024-2611","trace":[{"module":"google.golang.org/protobuf","version":"v1.31.0"}]}}
{"osv":{"id":"GO-2023-1234","summary":"Denial of service in golang.org/x/net/http2"}}
{"finding":{"osv":"GO-2023-1234","trace":[{"module":"golang.org/x/net","version":"v0.10.0","package":"golang.org/x/net/http2"}]}}
`
	fixtureSemgrep  = `{"version":"1.99.0","results":[{"check_id":"go.lang.security.audit.crypto.bad-random","path":"internal/crypto/rand.go","start":{"line":11,"col":2,"offset":100},"end":{"line":11,"col":30,"offset":128},"extra":{"message":"math/rand is not cryptographically secure","severity":"ERROR","lines":"rand.Read(b)","metadata":{"category":"security"}}},{"check_id":"go.correctness.unused-result","path":"a.go","start":{"line":3,"col":1,"offset":10},"end":{"line":3,"col":9,"offset":18},"extra":{"message":"result of the call is not used","severity":"WARNING","metadata":{}}},{"check_id":"go.style.note","path":"docs/notes.md","start":{"line":7,"col":1,"offset":1},"end":{"line":7,"col":5,"offset":5},"extra":{"message":"consider a code fence","severity":"INFO","metadata":{}}}],"errors":[{"code":3,"level":"warn","type":"Syntax error","message":"Rule parse error at rules/x.yaml:7: could not parse"}],"paths":{"scanned":["a.go","docs/notes.md","internal/crypto/rand.go"]},"time":{"rules":[],"total_time":0.4}}`
	fixtureGitleaks = `[{"Description":"Detected a Generic API Key","StartLine":12,"EndLine":12,"StartColumn":13,"EndColumn":52,"Match":"REDACTED","Secret":"REDACTED","File":"internal/config/creds.go","SymlinkFile":"","Commit":"0000000000000000000000000000000000000000","Entropy":3.9,"Author":"unknown","Email":"unknown","Date":"","Message":"","Tags":[],"RuleID":"generic-api-key","Fingerprint":"internal/config/creds.go:generic-api-key:12"}]`
	fixtureVet      = `# example.com/demo/internal/crypto
internal/crypto/rand.go:11:2: non-constant format string in call to fmt.Printf
internal/agent/loop.go:88:5: unreachable code
# example.com/demo/internal/agent
internal/agent/loop.go:120:9: range var l copies lock: example.com/demo/internal/agent.Loop
`
)

// scannerStub answers scanner invocations with fixture output, so no scanner
// has to be installed for a test to run.
type scannerStub struct {
	outputs map[string]string // binary basename → stdout/stderr the scanner reports on
	exits   map[string]error  // binary basename → process failure
	// calls records every invocation, which is how the gitleaks legacy-command
	// retry is asserted.
	calls []string
}

func (s *scannerStub) run(_ context.Context, inv scanInvocation) scanExec {
	bin := filepath.Base(inv.argv[0])
	s.calls = append(s.calls, strings.Join(append([]string{bin}, inv.argv[1:]...), " "))
	out, ok := s.outputs[bin]
	if !ok {
		return scanExec{waitErr: fmt.Errorf("exec: %q: executable file not found in $PATH", bin)}
	}
	return scanExec{parseErr: inv.parse(strings.NewReader(out)), waitErr: s.exits[bin]}
}

// stubLookPath reports every named binary as installed.
func stubLookPath(names ...string) func(string) (string, error) {
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	return func(name string) (string, error) {
		if !have[name] {
			return "", errors.New("executable file not found in $PATH")
		}
		return "/stub/" + name, nil
	}
}

// newStubTool returns a tool over a temp Go module with the named scanners
// installed and answering from outputs.
func newStubTool(t *testing.T, outputs map[string]string, present ...string) (*SecurityScanTool, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	stub := &scannerStub{outputs: outputs}
	tool := &SecurityScanTool{
		CWD:      dir,
		Timeout:  5 * time.Second,
		lookPath: stubLookPath(present...),
		run:      stub.run,
	}
	return tool, dir
}

func executeScan(t *testing.T, tool *SecurityScanTool, args string) Result {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return res
}

func scanDetails(t *testing.T, res Result) securityScanDetails {
	t.Helper()
	d, ok := res.Details.(securityScanDetails)
	if !ok {
		t.Fatalf("Details type = %T, want securityScanDetails", res.Details)
	}
	return d
}

// rows renders findings as "scanner/severity/file:line" for exact order
// assertions.
func rows(findings []SecurityFinding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, fmt.Sprintf("%s/%s/%s:%d", f.Scanner, f.Severity, f.File, f.Line))
	}
	return out
}

func rowString(statuses []ScannerStatus) string {
	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, s.Scanner+"/"+s.Status)
	}
	return strings.Join(out, " ")
}

// TestSecurityScanMergesSortsAndReportsDetails is the end-to-end shape check:
// four native formats in, one severity-sorted list out, with the footer and the
// programmatic details carrying the same facts.
func TestSecurityScanMergesSortsAndReportsDetails(t *testing.T) {
	tool, dir := newStubTool(t,
		map[string]string{"go": fixtureVet, "govulncheck": fixtureGovulncheck, "semgrep": fixtureSemgrep, "gitleaks": fixtureGitleaks},
		"go", "govulncheck", "semgrep", "gitleaks")
	res := executeScan(t, tool, `{}`)
	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	d := scanDetails(t, res)

	want := []string{
		// high: a leaked secret, a called vulnerable symbol, a security ERROR.
		"gitleaks/high/internal/config/creds.go:12",
		"govulncheck/high/loader/load.go:42",
		"semgrep/high/internal/crypto/rand.go:11",
		// medium: an imported (but not called) vulnerable package, a warning.
		"govulncheck/medium/:0",
		"semgrep/medium/a.go:3",
		// low: vet's bug reports outrank a semgrep informational note.
		"vet/low/internal/agent/loop.go:88",
		"vet/low/internal/agent/loop.go:120",
		"vet/low/internal/crypto/rand.go:11",
		"semgrep/info/docs/notes.md:7",
	}
	// The govulncheck row carries the summary plus the module and fix, and the
	// repeated report of the same vulnerability collapsed into one row.
	var vuln SecurityFinding
	for _, f := range d.Findings {
		if f.Scanner == "govulncheck" && f.Severity == "high" {
			vuln = f
		}
	}
	const wantVuln = "GO-2024-2611: Infinite loop in JSON unmarshaling in google.golang.org/protobuf (module google.golang.org/protobuf@v1.31.0, fixed in 1.33.0)"
	if vuln.Message != wantVuln {
		t.Errorf("govulncheck message = %q, want %q", vuln.Message, wantVuln)
	}

	// The per-scanner footer reports every scanner, including semgrep's own
	// errors[] from the report body.
	if got := rowString(d.Scanners); got != "vet/ok govulncheck/ok semgrep/ok gitleaks/ok" {
		t.Errorf("scanner statuses = %q", got)
	}
	for _, want := range []string{"vet ok (3)", "govulncheck ok (2)", "gitleaks ok (1)", "semgrep ok (3): 1 error(s): Rule parse error"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("footer missing %q in:\n%s", want, res.Text)
		}
	}
	if !strings.Contains(res.Text, "security_scan: 9 finding(s) in "+dir) {
		t.Errorf("header missing:\n%s", res.Text)
	}
	if !strings.HasPrefix(res.Text, "security_scan: 9 finding(s) in "+dir+"\nHIGH     gitleaks") {
		t.Errorf("findings must be severity-sorted right after the header:\n%s", res.Text)
	}

	// Details must survive the JSON hop the session store performs.
	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal details: %v", err)
	}
	for _, key := range []string{"path", "findings", "scanners", "totalFindings", "truncated"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("details JSON missing %q: %s", key, blob)
		}
	}
	if n, _ := decoded["findings"].([]any); len(n) != len(want) {
		t.Errorf("details findings = %d, want %d", len(n), len(want))
	}
}

// TestSecurityScanAbsentBinaryIsANote covers the fail-open contract: a missing
// scanner is reported, never an error, and the scanners that do exist still run.
func TestSecurityScanAbsentBinaryIsANote(t *testing.T) {
	tool, dir := newStubTool(t, map[string]string{"go": fixtureVet}, "go")
	res := executeScan(t, tool, `{}`)
	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	d := scanDetails(t, res)
	if got := rowString(d.Scanners); got != "vet/ok govulncheck/skipped semgrep/skipped gitleaks/skipped" {
		t.Errorf("statuses = %q", got)
	}
	for _, s := range d.Scanners[1:] {
		if s.Detail != s.Scanner+" is not installed" {
			t.Errorf("%s detail = %q", s.Scanner, s.Detail)
		}
	}
	if !strings.Contains(res.Text, "govulncheck skipped: govulncheck is not installed") {
		t.Errorf("footer must name the missing binary:\n%s", res.Text)
	}
	if len(d.Findings) != 3 {
		t.Errorf("vet findings = %d, want 3 (the floor scanner must still run)", len(d.Findings))
	}
	if !strings.Contains(res.Text, dir) {
		t.Errorf("target path missing from header:\n%s", res.Text)
	}
}

// TestSecurityScanNoGoModule covers the module-dependent scanners: outside a Go
// module they are skipped with the reason, and the path scanners still run.
func TestSecurityScanNoGoModule(t *testing.T) {
	plain := t.TempDir()
	stub := &scannerStub{outputs: map[string]string{"semgrep": fixtureSemgrep, "gitleaks": fixtureGitleaks}}
	tool := &SecurityScanTool{CWD: plain, Timeout: 5 * time.Second, lookPath: stubLookPath("go", "govulncheck", "semgrep", "gitleaks"), run: stub.run}

	res := executeScan(t, tool, `{}`)
	d := scanDetails(t, res)
	if got := rowString(d.Scanners); got != "vet/skipped govulncheck/skipped semgrep/ok gitleaks/ok" {
		t.Errorf("statuses = %q", got)
	}
	if d.Scanners[0].Detail != "no go.mod above "+plain {
		t.Errorf("vet detail = %q", d.Scanners[0].Detail)
	}
	if !strings.Contains(res.Text, "no go.mod above") {
		t.Errorf("footer must explain the skip:\n%s", res.Text)
	}
}

// TestSecurityScanTimeout covers the bounded-time contract: a scanner that
// never answers is abandoned and reported, not fatal.
func TestSecurityScanTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	blocked := 0
	tool := &SecurityScanTool{
		CWD:      dir,
		Timeout:  30 * time.Millisecond,
		lookPath: stubLookPath("go"),
		run: func(ctx context.Context, inv scanInvocation) scanExec {
			blocked++
			<-ctx.Done()
			return scanExec{waitErr: ctx.Err()}
		},
	}
	res := executeScan(t, tool, `{}`)
	if res.IsError {
		t.Fatalf("IsError = true: %s", res.Text)
	}
	d := scanDetails(t, res)
	if got := rowString(d.Scanners); got != "vet/timeout govulncheck/skipped semgrep/skipped gitleaks/skipped" {
		t.Errorf("statuses = %q", got)
	}
	if d.Scanners[0].Detail != "no result within the per-scanner budget" {
		t.Errorf("timeout detail = %q", d.Scanners[0].Detail)
	}
	if blocked != 1 {
		t.Errorf("blocked scanners = %d, want 1 (the rest are skipped)", blocked)
	}
	if !strings.Contains(res.Text, "vet timeout") {
		t.Errorf("footer must report the timeout:\n%s", res.Text)
	}
}

// TestSecurityScanCapTruncates covers the bounded-work contract: the cap stops
// the scanner mid-stream, the marker says what was dropped, and the scanners
// that never ran say so instead of vanishing.
func TestSecurityScanCapTruncates(t *testing.T) {
	var vet strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&vet, "internal/gen/file.go:%d:1: unreachable code\n", i)
	}
	tool, _ := newStubTool(t,
		map[string]string{"go": vet.String(), "govulncheck": fixtureGovulncheck, "semgrep": fixtureSemgrep, "gitleaks": fixtureGitleaks},
		"go", "govulncheck", "semgrep", "gitleaks")

	res := executeScan(t, tool, `{"max_findings": 5}`)
	d := scanDetails(t, res)
	if len(d.Findings) != 5 {
		t.Fatalf("findings = %d, want 5", len(d.Findings))
	}
	if d.Total != 6 || !d.Truncated {
		t.Errorf("Total=%d Truncated=%v, want 6/true", d.Total, d.Truncated)
	}
	if d.Scanners[0].Status != statusTruncated {
		t.Errorf("vet status = %q, want truncated", d.Scanners[0].Status)
	}
	for _, s := range d.Scanners[1:] {
		if s.Status != statusSkipped || !strings.Contains(s.Detail, "finding cap") {
			t.Errorf("%s = %s/%q, want skipped at the cap", s.Scanner, s.Status, s.Detail)
		}
	}
	if !strings.Contains(res.Text, "[truncated: showing the top 5 of 6 findings") {
		t.Errorf("truncation marker missing:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "govulncheck skipped: not run: the finding cap was reached") {
		t.Errorf("footer must report the scanners that never ran:\n%s", res.Text)
	}
}

// TestSecurityScanFailuresAreReported covers the never-silent rule: a scanner
// that exits with an error, or that reports failure inside a successful
// response body, is surfaced in the footer with its own words.
func TestSecurityScanFailuresAreReported(t *testing.T) {
	semgrepBroken := `{"version":"1.99.0","results":[],"errors":[{"code":2,"level":"error","type":"RulesetError","message":"Failed to download config auto: no network"}],"paths":{}}`
	stub := &scannerStub{
		outputs: map[string]string{
			"semgrep":     semgrepBroken,
			"gitleaks":    "",
			"govulncheck": "not json at all\n",
		},
		exits: map[string]error{"gitleaks": errors.New("exit status 2")},
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	tool := &SecurityScanTool{CWD: dir, Timeout: 5 * time.Second, lookPath: stubLookPath("semgrep", "gitleaks", "govulncheck"), run: stub.run}

	res := executeScan(t, tool, `{"scanners": ["semgrep", "gitleaks", "govulncheck"]}`)
	if res.IsError {
		t.Fatalf("a failing scanner must not fail the whole tool: %s", res.Text)
	}
	d := scanDetails(t, res)
	if got := rowString(d.Scanners); got != "govulncheck/error semgrep/error gitleaks/error" {
		t.Fatalf("statuses = %q", got)
	}
	wantDetails := map[string]string{
		"semgrep":     "1 error(s): Failed to download config auto: no network",
		"gitleaks":    "exit status 2",
		"govulncheck": "govulncheck: unreadable output",
	}
	for _, s := range d.Scanners {
		if !strings.Contains(s.Detail, wantDetails[s.Scanner]) {
			t.Errorf("%s detail = %q, want it to contain %q", s.Scanner, s.Detail, wantDetails[s.Scanner])
		}
		if !strings.Contains(res.Text, s.Scanner+" error") {
			t.Errorf("footer must report %s as failed:\n%s", s.Scanner, res.Text)
		}
	}
	if !strings.Contains(res.Text, "security_scan: no findings in "+dir) {
		t.Errorf("header missing:\n%s", res.Text)
	}
}

// TestSecurityScanGitleaksLegacyCommand covers the pre-8.19 invocation: a
// binary that does not know `dir` is retried with `detect`, and only when it
// says the command is unknown.
func TestSecurityScanGitleaksLegacyCommand(t *testing.T) {
	var argvs []string
	tool := &SecurityScanTool{
		CWD:      t.TempDir(),
		Timeout:  5 * time.Second,
		lookPath: stubLookPath("gitleaks"),
		run: func(_ context.Context, inv scanInvocation) scanExec {
			argvs = append(argvs, strings.Join(inv.argv[1:], " "))
			if inv.argv[1] == "dir" {
				return scanExec{waitErr: errors.New("exit status 126"), stderr: `Error: unknown command "dir" for "gitleaks"`}
			}
			return scanExec{parseErr: inv.parse(strings.NewReader(fixtureGitleaks))}
		},
	}
	res := executeScan(t, tool, `{"scanners": ["gitleaks"]}`)
	d := scanDetails(t, res)
	if d.Scanners[0].Status != statusOK || len(d.Findings) != 1 {
		t.Fatalf("legacy retry failed: %s / %v", d.Scanners[0].Status, d.Scanners[0].Detail)
	}
	if len(argvs) != 2 {
		t.Fatalf("invocations = %q, want a `dir` attempt then a `detect --no-git` retry", argvs)
	}
	if !strings.HasPrefix(argvs[0], "dir ") || !strings.Contains(argvs[1], "detect --no-git") {
		t.Errorf("invocations = %q, want a `dir` attempt then a `detect --no-git` retry", argvs)
	}
	if !strings.Contains(argvs[0], "--redact=100") || !strings.Contains(argvs[1], "--redact=100") {
		t.Errorf("gitleaks must run with secrets redacted: %q", argvs)
	}
	if !strings.Contains(d.Findings[0].Message, "secret redacted") {
		t.Errorf("finding must say the secret was redacted: %q", d.Findings[0].Message)
	}
}

// TestSecurityScanRejectsBadArguments covers the trust boundary: malformed
// arguments, an unknown scanner name, and a missing path are the only ways this
// tool returns an error.
func TestSecurityScanRejectsBadArguments(t *testing.T) {
	tool, dir := newStubTool(t, map[string]string{"go": fixtureVet}, "go")
	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{"malformed", `{`, "malformed arguments"},
		{"unknown scanner", `{"scanners":["nmap"]}`, `unknown scanner "nmap" (known: vet, govulncheck, semgrep, gitleaks)`},
		{"missing path", `{"path":"/no/such/path/xdev"}`, "path /no/such/path/xdev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := executeScan(t, tool, tc.args)
			if !res.IsError {
				t.Fatalf("IsError = false: %s", res.Text)
			}
			if !strings.Contains(res.Text, tc.want) {
				t.Errorf("text = %q, want it to contain %q", res.Text, tc.want)
			}
		})
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("target disappeared: %v", err)
	}
}

// TestSecurityScanScannerSelectionAndClamps pins the argument plumbing: the
// requested subset runs in canonical order, and both numeric knobs are clamped.
func TestSecurityScanScannerSelectionAndClamps(t *testing.T) {
	tool, _ := newStubTool(t,
		map[string]string{"go": fixtureVet, "govulncheck": fixtureGovulncheck, "semgrep": fixtureSemgrep, "gitleaks": fixtureGitleaks},
		"go", "govulncheck", "semgrep", "gitleaks")

	// Requested out of order: the canonical order is what runs.
	res := executeScan(t, tool, `{"scanners": ["gitleaks","vet"]}`)
	if got := rowString(scanDetails(t, res).Scanners); got != "vet/ok gitleaks/ok" {
		t.Errorf("statuses = %q, want only the requested scanners in canonical order", got)
	}
	if tool.scannerTimeout(99_999) != MaxScannerTimeout || tool.scannerTimeout(3) != 3*time.Second || tool.scannerTimeout(0) != 5*time.Second {
		t.Errorf("timeout clamping wrong: %s/%s/%s", tool.scannerTimeout(99_999), tool.scannerTimeout(3), tool.scannerTimeout(0))
	}
	if tool.findingCap(99_999) != MaxSecurityScanFindings || tool.findingCap(7) != 7 {
		t.Errorf("finding cap clamping wrong")
	}
}

// TestSecurityScanResolveTarget pins the Go context resolution: the module root
// and package pattern decide what vet and govulncheck analyze, and a file target
// scans its directory.
func TestSecurityScanResolveTarget(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "internal", "tool")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "x.go"), []byte("package tool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &SecurityScanTool{CWD: nested}

	tg, err := tool.resolveTarget("")
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tg.modRoot != root || tg.pattern != "./internal/tool/..." {
		t.Errorf("dir target: modRoot=%q pattern=%q", tg.modRoot, tg.pattern)
	}
	tg, err = tool.resolveTarget("x.go")
	if err != nil {
		t.Fatalf("resolveTarget(file): %v", err)
	}
	if tg.path != filepath.Join(nested, "x.go") || tg.workDir != nested || tg.pattern != "./internal/tool/..." {
		t.Errorf("file target: %+v", tg)
	}
	// A nested module is its own module root.
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module example.com/nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tg, err = tool.resolveTarget(""); err != nil || tg.modRoot != nested || tg.pattern != "./..." {
		t.Errorf("nested module: %+v (%v)", tg, err)
	}
	// displayPath keeps in-tree paths relative and leaves outside paths alone.
	abs := filepath.Join(root, "internal", "tool", "x.go")
	if got := displayPath(scanTarget{path: root, workDir: root}, abs); got != filepath.Join("internal", "tool", "x.go") {
		t.Errorf("displayPath(in-tree) = %q", got)
	}
	if got := displayPath(scanTarget{path: root, workDir: root}, "/elsewhere/x.go"); got != "/elsewhere/x.go" {
		t.Errorf("displayPath(outside) = %q", got)
	}
}

// TestSecurityScanRealProcesses exercises the production runner end to end:
// real processes on PATH, real pipes, and the real kill path for both a timeout
// and a cap-stop. Shell stubs stand in for the scanners, so nothing needs to be
// installed (except a POSIX shell).
func TestSecurityScanRealProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs are POSIX-only; the injected-runner tests cover the parser contract everywhere")
	}
	stubDir := t.TempDir()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// write plants a stub scanner. toStderr mirrors where the real tool reports:
	// go vet writes its diagnostics to stderr, the others print their report on
	// stdout, so a wrong pipe routing loses findings here.
	write := func(name, output string, toStderr bool) {
		t.Helper()
		fixture := filepath.Join(stubDir, name+".out")
		if err := os.WriteFile(fixture, []byte(output), 0o644); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\ncat " + fixture + "\n"
		if toStderr {
			script = "#!/bin/sh\ncat " + fixture + " >&2\n"
		}
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("go", fixtureVet, true)
	write("govulncheck", fixtureGovulncheck, false)
	write("semgrep", fixtureSemgrep, false)
	write("gitleaks", fixtureGitleaks, false)
	// PATH keeps the real entries so the scripts' `cat`/`sleep` resolve; the
	// stub directory comes first so the stubs win over anything installed.
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tool := &SecurityScanTool{CWD: dir, Timeout: 10 * time.Second}
	res := executeScan(t, tool, `{}`)
	d := scanDetails(t, res)
	if got := rowString(d.Scanners); got != "vet/ok govulncheck/ok semgrep/ok gitleaks/ok" {
		t.Fatalf("statuses = %q\n%s", got, res.Text)
	}
	if len(d.Findings) != 9 {
		t.Errorf("findings = %d, want 9\n%s", len(d.Findings), res.Text)
	}

	// The cap must stop the scanner mid-stream, not just stop reading it.
	fast := &SecurityScanTool{CWD: dir, Timeout: 10 * time.Second, MaxFindings: 1}
	res = executeScan(t, fast, `{"scanners":["semgrep"],"max_findings":1}`)
	if d := scanDetails(t, res); d.Scanners[0].Status != statusTruncated || len(d.Findings) != 1 {
		t.Errorf("cap over a real process: %s / %d findings", d.Scanners[0].Status, len(d.Findings))
	}

	// A scanner that never answers is killed at its budget.
	slow := filepath.Join(stubDir, "semgrep")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	timeoutTool := &SecurityScanTool{CWD: dir, Timeout: 150 * time.Millisecond}
	start := time.Now()
	res = executeScan(t, timeoutTool, `{"scanners":["semgrep"]}`)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %s; the scanner was not killed", elapsed)
	}
	if d := scanDetails(t, res); d.Scanners[0].Status != statusTimeout {
		t.Errorf("status = %s, want timeout (detail %q)", d.Scanners[0].Status, d.Scanners[0].Detail)
	}
}

// TestSecurityScanGovulncheckFraming pins the framing tolerance the real
// binary forced: govulncheck documents "streaming JSON", but v1.1.4
// pretty-prints every message across several lines and interleaves SBOM
// messages — a line-splitting parser reads that as one unreadable blob (found
// by running the tool against the real binary). One message per line must keep
// working too.
func TestSecurityScanGovulncheckFraming(t *testing.T) {
	pretty := `{
  "config": {
    "protocol_version": "v1.0.0",
    "scanner_name": "govulncheck",
    "scanner_version": "v1.1.4",
    "scan_level": "symbol",
    "scan_mode": "source"
  }
}
{
  "SBOM": {
    "go_version": "go1.25.14",
    "modules": [
      {"path": "example.com/demo", "version": ""},
      {"path": "golang.org/x/text", "version": "v0.3.7"}
    ],
    "roots": ["./..."]
  }
}
{
  "osv": {
    "id": "GO-2022-0969",
    "summary": "Panic in golang.org/x/text/language.Parse"
  }
}
{
  "finding": {
    "osv": "GO-2022-0969",
    "fixed_version": "0.3.8",
    "trace": [
      {
        "module": "golang.org/x/text",
        "version": "v0.3.7",
        "package": "golang.org/x/text/language",
        "function": "Parse",
        "position": {"filename": "/root/go/pkg/mod/golang.org/text@v0.3.7/language/parse.go", "line": 1400}
      },
      {
        "module": "example.com/demo",
        "package": "example.com/demo",
        "function": "main",
        "position": {"filename": "main.go", "line": 9}
      }
    ]
  }
}
`
	oneline := `{"osv":{"id":"GO-2022-0969","summary":"Panic in golang.org/x/text/language.Parse"}}
{"finding":{"osv":"GO-2022-0969","trace":[{"module":"golang.org/x/text","version":"v0.3.7","package":"golang.org/x/text/language","position":{"filename":"main.go","line":9}}]}}
`
	for _, tc := range []struct{ name, stream string }{
		{"pretty printed", pretty},
		{"one message per line", oneline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, _ := newStubTool(t, map[string]string{"govulncheck": tc.stream}, "govulncheck")
			res := executeScan(t, tool, `{"scanners":["govulncheck"]}`)
			d := scanDetails(t, res)
			if d.Scanners[0].Status != statusOK {
				t.Fatalf("status = %s (%s)", d.Scanners[0].Status, d.Scanners[0].Detail)
			}
			if len(d.Findings) != 1 {
				t.Fatalf("findings = %d, want 1: %+v", len(d.Findings), d.Findings)
			}
			f := d.Findings[0]
			if f.File != "main.go" || f.Line != 9 || f.Severity != "high" {
				t.Errorf("finding = %+v, want main.go:9, high (called from the entry point)", f)
			}
			if !strings.Contains(f.Message, "GO-2022-0969: Panic in golang.org/x/text/language.Parse") {
				t.Errorf("message = %q", f.Message)
			}
		})
	}
}
