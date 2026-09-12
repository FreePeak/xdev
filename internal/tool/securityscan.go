package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// security_scan (M15 #67): run whichever security scanners the host has and
// merge their findings into one severity-sorted list.
//
// Three properties drive the design:
//
//   - Every real scanner is an external program (govulncheck, semgrep and
//     gitleaks have no embeddable Go API), so this is a shell-out tool like
//     grep's ripgrep fast path. `go vet` is the floor: it ships with the
//     toolchain the user already builds with, so the tool still answers on a
//     machine with nothing else installed.
//   - Availability is fail-open AND always visible. A scanner that is not
//     installed, times out, exits non-zero, or emits output we cannot parse
//     contributes a status line to the footer; it never turns the whole
//     result into an error, and it is never silently dropped.
//   - Everything is bounded: one process at a time, a per-scanner wall-clock
//     budget, a streaming parse (a repo-sized semgrep report never lands in
//     memory whole), and a cap on the merged list.
const (
	// DefaultScannerTimeout bounds ONE scanner.
	DefaultScannerTimeout = 120 * time.Second
	// MaxScannerTimeout clamps the timeout_seconds argument.
	MaxScannerTimeout = 600 * time.Second
	// DefaultSecurityScanMaxFindings caps the merged finding list.
	DefaultSecurityScanMaxFindings = 200
	// MaxSecurityScanFindings clamps the max_findings argument.
	MaxSecurityScanFindings = 2000
	// scanDetailLimit bounds the scanner stderr quoted in the footer.
	scanDetailLimit = 400
	// scanMessageLimit bounds one finding's message.
	scanMessageLimit = 300
	// scanLineLimit bounds one JSON line of a streaming scanner.
	scanLineLimit = 1 << 20
)

// Scanner status vocabulary, as reported in the footer and in Details.
const (
	statusOK        = "ok"
	statusSkipped   = "skipped"
	statusError     = "error"
	statusTimeout   = "timeout"
	statusTruncated = "truncated"
)

// errScanCap stops a streaming parser once the merged list is full. The
// caller kills the scanner and records the run as truncated, so the dropped
// tail is reported instead of silently discarded.
var errScanCap = errors.New("security_scan: finding cap reached")

// severityRank orders the merged list. "unknown" outranks "info" on purpose:
// a finding nobody rated still outranks a style note.
var severityRank = map[string]int{
	"critical": 5,
	"high":     4,
	"medium":   3,
	"low":      2,
	"unknown":  1,
	"info":     0,
}

// normalizeSeverity folds every scanner's own vocabulary into ours: semgrep
// uses ERROR/WARNING/INFO plus the secrets-rated CRITICAL..LOW, OSV spells
// medium as MODERATE.
func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "high", "medium", "low", "info", "unknown":
		return strings.ToLower(strings.TrimSpace(s))
	case "error":
		return "high"
	case "warning", "moderate":
		return "medium"
	}
	return "unknown"
}

// SecurityFinding is one merged scanner finding. Line is 0 when the scanner
// reported no location (a vulnerable module, not a call site).
type SecurityFinding struct {
	Scanner  string `json:"scanner"`
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// ScannerStatus is one line of the status footer.
type ScannerStatus struct {
	Scanner    string `json:"scanner"`
	Status     string `json:"status"`
	Findings   int    `json:"findings"`
	DurationMs int64  `json:"durationMs"`
	// Detail carries why a scanner did not run cleanly (missing binary,
	// timeout, stderr tail, parse error, semgrep's own errors[]).
	Detail string `json:"detail,omitempty"`
}

// securityScanDetails is the programmatic result persisted in Result.Details.
type securityScanDetails struct {
	Path      string            `json:"path"`
	Findings  []SecurityFinding `json:"findings"`
	Scanners  []ScannerStatus   `json:"scanners"`
	Total     int               `json:"totalFindings"`
	Truncated bool              `json:"truncated"`
}

// SecurityScanTool runs the installed security scanners over a target path.
type SecurityScanTool struct {
	CWD string
	// Timeout bounds each scanner; 0 → DefaultScannerTimeout.
	Timeout time.Duration
	// MaxFindings caps the merged list; 0 → DefaultSecurityScanMaxFindings.
	MaxFindings int

	// run executes one scanner process; nil → runScannerProcess. Tests inject
	// fixture output here so no scanner has to be installed.
	run scanRunner
	// lookPath resolves a scanner binary; nil → the package's lookPath.
	lookPath func(string) (string, error)
}

// NewSecurityScanTool returns a security_scan tool rooted at cwd.
func NewSecurityScanTool(cwd string) *SecurityScanTool { return &SecurityScanTool{CWD: cwd} }

func (t *SecurityScanTool) Name() string { return "security_scan" }

func (t *SecurityScanTool) Description() string {
	// One terse line: the system prompt carries a budgeted tool recap, so the
	// operator detail belongs in the schema (same reasoning as web_search).
	return "run the installed security scanners (go vet, govulncheck, semgrep, gitleaks) over a path; returns merged severity-sorted findings, with uninstalled or failing scanners noted in the footer"
}

func (t *SecurityScanTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "file or directory to scan (default: cwd)"},
    "scanners": {"type": "array", "items": {"type": "string", "enum": ["vet", "govulncheck", "semgrep", "gitleaks"]}, "description": "scanners to run (default: all available); a missing binary is reported, never an error"},
    "timeout_seconds": {"type": "integer", "description": "wall-clock budget per scanner (default 120, max 600)"},
    "max_findings": {"type": "integer", "description": "cap on the merged finding list (default 200, max 2000)"}
  }
}`)
}

type securityScanArgs struct {
	Path           string   `json:"path"`
	Scanner        []string `json:"scanners"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	MaxFindings    int      `json:"max_findings"`
}

// scanTarget is the resolved subject of a scan.
type scanTarget struct {
	// path is the absolute target exactly as requested.
	path string
	// workDir is where a path scanner runs; for a file target it is the
	// containing directory.
	workDir string
	// modRoot is the enclosing Go module root, "" when there is none.
	modRoot string
	// pattern is the go package pattern relative to modRoot ("./...").
	pattern string
}

func (t *SecurityScanTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a securityScanArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: "security_scan: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	tg, err := t.resolveTarget(a.Path)
	if err != nil {
		return Result{Text: "security_scan: " + err.Error(), IsError: true}, nil
	}
	specs, err := selectScanners(a.Scanner)
	if err != nil {
		return Result{Text: "security_scan: " + err.Error(), IsError: true}, nil
	}
	budget := t.scannerTimeout(a.TimeoutSeconds)
	cap := t.findingCap(a.MaxFindings)

	var (
		findings []SecurityFinding
		statuses []ScannerStatus
		total    int
	)
	for _, spec := range specs {
		if total >= cap {
			// The cap is a work budget, not just a display limit: the
			// scanners that never ran say so instead of vanishing.
			statuses = append(statuses, ScannerStatus{Scanner: spec.name, Status: statusSkipped, Detail: "not run: the finding cap was reached"})
			continue
		}
		run := t.runOne(ctx, spec, tg, budget, cap-total)
		findings = append(findings, run.findings...)
		statuses = append(statuses, run.status)
		total += run.total
	}
	sortFindings(findings)
	details := securityScanDetails{
		Path:      tg.path,
		Findings:  findings,
		Scanners:  statuses,
		Total:     total,
		Truncated: total > len(findings),
	}
	return Result{Text: renderSecurityScan(tg, findings, total, statuses), Details: details}, nil
}

// resolveTarget resolves the requested path and the Go context around it.
func (t *SecurityScanTool) resolveTarget(raw string) (scanTarget, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		path = t.CWD
	}
	if path == "" {
		path = "."
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.CWD, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return scanTarget{}, fmt.Errorf("path %s: %w", path, err)
	}
	tg := scanTarget{path: filepath.Clean(path), workDir: filepath.Clean(path)}
	if !info.IsDir() {
		tg.workDir = filepath.Dir(tg.path)
	}
	tg.modRoot = findModuleRoot(tg.workDir)
	if tg.modRoot != "" {
		rel, rerr := filepath.Rel(tg.modRoot, tg.workDir)
		switch {
		case rerr != nil:
			tg.modRoot = ""
		case rel == ".":
			tg.pattern = "./..."
		default:
			tg.pattern = "./" + filepath.ToSlash(rel) + "/..."
		}
	}
	return tg, nil
}

func (t *SecurityScanTool) scannerTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		if t.Timeout > 0 {
			return t.Timeout
		}
		return DefaultScannerTimeout
	}
	d := time.Duration(seconds) * time.Second
	if d > MaxScannerTimeout {
		return MaxScannerTimeout
	}
	return d
}

func (t *SecurityScanTool) findingCap(n int) int {
	if n <= 0 {
		if t.MaxFindings > 0 {
			return t.MaxFindings
		}
		return DefaultSecurityScanMaxFindings
	}
	if n > MaxSecurityScanFindings {
		return MaxSecurityScanFindings
	}
	return n
}

// selectScanners validates the requested names and returns the specs in their
// canonical order.
func selectScanners(names []string) ([]scannerSpec, error) {
	all := scannerSpecs()
	if len(names) == 0 {
		return all, nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var out []scannerSpec
	for _, spec := range all {
		if want[spec.name] {
			out = append(out, spec)
			delete(want, spec.name)
		}
	}
	if len(want) > 0 {
		unknown := make([]string, 0, len(want))
		for n := range want {
			unknown = append(unknown, n)
		}
		sort.Strings(unknown)
		known := make([]string, 0, len(all))
		for _, spec := range all {
			known = append(known, spec.name)
		}
		return nil, fmt.Errorf("unknown scanner %q (known: %s)", strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	return out, nil
}

// findModuleRoot walks up from dir looking for go.mod. The nearest module
// wins, so a nested module is scanned as itself rather than through its parent.
func findModuleRoot(dir string) string {
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// scannerSpec is one external scanner: where its binary lives, how it is
// invoked, and how its output is read.
type scannerSpec struct {
	name   string
	binary string
	// goModule marks scanners that analyze Go packages: they run in the
	// enclosing module root, receive the package pattern, and are skipped
	// with a note when the target has no go.mod.
	goModule bool
	// argv builds the arguments after the binary.
	argv func(tg scanTarget) []string
	// legacyArgv is the pre-8.19 invocation of the same binary; it is only
	// retried when the first attempt reports the command as unknown.
	legacyArgv func(tg scanTarget) []string
	// parse reads the scanner's native output. It returns errScanCap when
	// the cap stopped the stream early.
	parse func(r io.Reader, tg scanTarget, c *collector) error
	// stream names the pipe this scanner reports on (vet prints diagnostics
	// on stderr, the JSON scanners print their report on stdout).
	stream scanStream
}

// dir is where the scanner runs: the module root for Go package scanners
// (their pattern is relative to it), the target directory otherwise.
func (s scannerSpec) dir(tg scanTarget) string {
	if s.goModule {
		return tg.modRoot
	}
	return tg.workDir
}

func scannerSpecs() []scannerSpec {
	return []scannerSpec{
		{
			name: "vet", binary: "go", goModule: true,
			argv:   func(tg scanTarget) []string { return []string{"vet", tg.pattern} },
			parse:  parseVetText,
			stream: scanStreamStderr,
		},
		{
			name: "govulncheck", binary: "govulncheck", goModule: true,
			argv:  func(tg scanTarget) []string { return []string{"-json", tg.pattern} },
			parse: parseGovulncheckJSON,
		},
		{
			name: "semgrep", binary: "semgrep",
			argv: func(tg scanTarget) []string {
				return []string{"scan", "--json", "--quiet", "--metrics=off", "--disable-version-check", "--config", semgrepConfig(tg), tg.path}
			},
			parse: parseSemgrepJSON,
		},
		{
			name: "gitleaks", binary: "gitleaks",
			// Flags before the positional target, matching the documented
			// `gitleaks dir -v <path>` shape.
			argv: func(tg scanTarget) []string {
				return append(append([]string{"dir"}, gitleaksFlags()...), tg.path)
			},
			legacyArgv: func(tg scanTarget) []string {
				return append(append([]string{"detect", "--no-git"}, gitleaksFlags()...), "--source", tg.path)
			},
			parse: parseGitleaksJSON,
		},
	}
}

// gitleaksFlags are shared by the current (`dir`) and legacy (`detect`)
// invocations. --redact=100 is not cosmetic: without it the report carries the
// secret itself, which is exactly what xdev's redaction seam exists to keep
// out of the model's context. --report-path=- writes the report to stdout
// (gitleaks' documented spelling), and --exit-code=0 keeps "leaks found" from
// looking like a tool failure.
func gitleaksFlags() []string {
	return []string{
		"--report-format", "json", "--report-path", "-",
		"--no-banner", "--no-color", "--log-level", "warn",
		"--redact=100", "--exit-code", "0",
	}
}

// semgrepConfig names the ruleset: a ruleset the repository already carries
// wins (offline, and the rules the project actually chose), otherwise the
// registry's curated "auto" set.
func semgrepConfig(tg scanTarget) string {
	for _, name := range []string{".semgrep.yml", ".semgrep.yaml", "semgrep.yml", ".semgrep"} {
		if _, err := os.Stat(filepath.Join(tg.workDir, name)); err == nil {
			return filepath.Join(tg.workDir, name)
		}
	}
	return "auto"
}

// collector accumulates one scanner's findings under the shared cap.
type collector struct {
	cap      int
	findings []SecurityFinding
	// total counts every finding the scanner produced as far as we read it,
	// including the one that did not fit. It is what the footer reports, so a
	// truncated run is never mistaken for a clean one.
	total   int
	stopped bool
	// note is a parser-level remark surfaced in the footer (semgrep reports
	// rule-fetch failures in the report body, not through its exit code).
	note string
}

// add records one finding, returning errScanCap once the merged list is full.
func (c *collector) add(f SecurityFinding) error {
	if len(c.findings) >= c.cap {
		c.stopped = true
		c.total++
		return errScanCap
	}
	c.findings = append(c.findings, f)
	c.total++
	return nil
}

// scannerRun is one scanner's contribution.
type scannerRun struct {
	findings []SecurityFinding
	total    int
	status   ScannerStatus
}

// scanStream names the pipe a scanner reports on.
type scanStream int

const (
	scanStreamStdout scanStream = iota
	scanStreamStderr
)

// scanInvocation is one scanner process about to run.
type scanInvocation struct {
	dir   string
	argv  []string
	parse func(io.Reader) error
	// stream is the pipe parse consumes; the other stream is bounded into the
	// footer's detail.
	stream scanStream
}

// scanExec is what one scanner process yielded.
type scanExec struct {
	// parseErr is parse's verdict: nil, errScanCap (stopped at the cap), or a
	// format error.
	parseErr error
	// waitErr is the process error (nil, *exec.ExitError, or the ctx error).
	waitErr error
	// stderr is a bounded, single-line tail of the scanner's stderr.
	stderr string
}

// scanRunner runs one scanner process. It is a seam so tests can feed fixture
// output without installing anything.
type scanRunner func(ctx context.Context, inv scanInvocation) scanExec

// runOne runs a single spec within the shared budget and maps the outcome to
// a status. Every exit path here produces a status: the footer is the record.
func (t *SecurityScanTool) runOne(ctx context.Context, spec scannerSpec, tg scanTarget, budget time.Duration, remaining int) scannerRun {
	if spec.goModule && tg.modRoot == "" {
		return scannerRun{status: ScannerStatus{Scanner: spec.name, Status: statusSkipped, Detail: "no go.mod above " + tg.workDir}}
	}
	look := t.lookPath
	if look == nil {
		look = lookPath
	}
	bin, err := look(spec.binary)
	if err != nil {
		return scannerRun{status: ScannerStatus{Scanner: spec.name, Status: statusSkipped, Detail: spec.binary + " is not installed"}}
	}
	argv := append([]string{bin}, spec.argv(tg)...)
	var legacy []string
	if spec.legacyArgv != nil {
		legacy = append([]string{bin}, spec.legacyArgv(tg)...)
	}

	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	out, stderr := t.attempt(runCtx, spec, tg, argv, remaining)
	// gitleaks renamed `detect` to `dir` in v8.19 (the old command still
	// exists, hidden). Only a binary that says it does not know the new
	// command gets the legacy invocation, so a real failure is not retried.
	if legacy != nil && out.status.Status == statusError && isUnknownCommand(stderr) {
		out, stderr = t.attempt(runCtx, spec, tg, legacy, remaining)
	}
	out.status.DurationMs = time.Since(start).Milliseconds()
	return out
}

// attempt runs one argv variant and derives its status.
func (t *SecurityScanTool) attempt(runCtx context.Context, spec scannerSpec, tg scanTarget, argv []string, remaining int) (scannerRun, string) {
	c := &collector{cap: remaining}
	run := t.run
	if run == nil {
		run = runScannerProcess
	}
	exec := run(runCtx, scanInvocation{
		dir:    spec.dir(tg),
		argv:   argv,
		stream: spec.stream,
		parse:  func(r io.Reader) error { return spec.parse(r, tg, c) },
	})

	status := ScannerStatus{Scanner: spec.name, Findings: c.total}
	switch {
	case c.stopped || errors.Is(exec.parseErr, errScanCap):
		status.Status = statusTruncated
		// No count here: `remaining` is what fit, the global cap is what
		// stopped the run, and the truncation marker in the body already
		// spells out "top N of M".
		status.Detail = "stopped: the merged finding cap is full"
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		status.Status = statusTimeout
		status.Detail = "no result within the per-scanner budget"
	case errors.Is(runCtx.Err(), context.Canceled):
		status.Status = statusError
		status.Detail = "cancelled"
	case exec.waitErr != nil && c.total == 0:
		// A scanner that produced nothing and failed says why in its exit
		// status and stderr; that beats the parser's "EOF" for the same run.
		status.Status = statusError
		status.Detail = oneLine(strings.TrimSpace(exec.waitErr.Error()+" "+exec.stderr), scanDetailLimit)
	case c.note != "" && c.total == 0:
		// Nothing parsed and the scanner said why: a clean "0 findings" run
		// that actually failed must not read as success.
		status.Status = statusError
		status.Detail = c.note
	case exec.parseErr != nil:
		status.Status = statusError
		status.Detail = oneLine(exec.parseErr.Error(), scanDetailLimit)
	default:
		status.Status = statusOK
		status.Detail = c.note
	}
	return scannerRun{findings: c.findings, total: c.total, status: status}, exec.stderr
}

// runScannerProcess is the production runner: one process at a time, its own
// process group (so a timeout or a cap-stop reaches the scanner's children —
// semgrep-core is a separate process), a hardened environment (no API keys
// leak into a scanner), and /dev/null on stdin so nothing can block waiting
// for input.
func runScannerProcess(ctx context.Context, inv scanInvocation) scanExec {
	cmd := exec.CommandContext(ctx, inv.argv[0], inv.argv[1:]...)
	cmd.Dir = inv.dir
	cmd.Env = HardenedEnv()
	prepareProcessGroup(cmd) // no-op on windows (see kill_windows.go)

	stderrSink := NewOutputSink(8<<10, 8<<10)
	var (
		parseReader io.Reader
		pipeErr     error
	)
	if inv.stream == scanStreamStderr {
		cmd.Stdout = io.Discard
		var sp io.ReadCloser
		if sp, pipeErr = cmd.StderrPipe(); pipeErr == nil {
			// Tee so the footer keeps a bounded copy of what parse read.
			parseReader = io.TeeReader(sp, stderrSink)
		}
	} else {
		cmd.Stderr = stderrSink
		var sp io.ReadCloser
		if sp, pipeErr = cmd.StdoutPipe(); pipeErr == nil {
			parseReader = sp
		}
	}
	if pipeErr != nil {
		return scanExec{waitErr: pipeErr}
	}
	if err := cmd.Start(); err != nil {
		return scanExec{waitErr: err}
	}

	// Watchdog: the per-scanner deadline must reach the scanner's whole
	// process group. CommandContext alone kills only the direct child, and a
	// surviving grandchild keeps the output pipe open — the run would then
	// block until that grandchild exits on its own, blowing the budget.
	watch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessGroup(cmd.Process.Pid, signalTerm)
			timer := time.NewTimer(KillGrace)
			defer timer.Stop()
			select {
			case <-timer.C:
				killProcessGroup(cmd.Process.Pid, signalKill)
			case <-watch:
			}
		case <-watch:
		}
	}()

	parseErr := inv.parse(parseReader)
	if parseErr != nil {
		// Either the output is unusable or the finding cap says we have
		// enough: stop the scanner rather than let it write into a pipe
		// nobody reads until its whole budget expires. Windows cannot signal
		// the group (see kill_windows.go), where the deadline is the bound.
		killProcessGroup(cmd.Process.Pid, signalKill)
	}
	// Drained only after parse is done with the stream, because cmd.Wait
	// closes the pipe handles (the bash tool learned this the hard way).
	_, _ = io.Copy(io.Discard, parseReader)
	waitErr := cmd.Wait()
	close(watch)
	stderr, _ := stderrSink.Result()
	return scanExec{parseErr: parseErr, waitErr: waitErr, stderr: stderr}
}

// isUnknownCommand detects cobra's "unknown command" bail-out, which is how a
// pre-8.19 gitleaks answers `dir`.
func isUnknownCommand(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "unknown command")
}

// parseVetText reads `go vet` diagnostics ("path:line[:col]: message"). vet
// has no severity field, and the analyzers do not name themselves in the
// message (verified against the x/tools sources: "non-constant format string
// in call to fmt.Printf", "unreachable code", …), so every diagnostic ranks as
// low: they are high-precision bug reports, ranked above informational notes
// and below anything a security scanner rated.
func parseVetText(r io.Reader, tg scanTarget, c *collector) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), scanLineLimit)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// Package headers ("# pkg") and vet's own failures ("vet: ...") are
		// not findings; the latter stay visible through the stderr tail the
		// footer quotes when vet fails.
		if line == "" || strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "vet: ") {
			continue
		}
		m := vetDiagRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lineNo, _ := strconv.Atoi(m[2])
		if err := c.add(SecurityFinding{
			Scanner:  "vet",
			Severity: "low",
			File:     displayPath(tg, m[1]),
			Line:     lineNo,
			Message:  oneLine(m[3], scanMessageLimit),
		}); err != nil {
			return err
		}
	}
	return sc.Err()
}

// vetDiagRe matches one vet diagnostic. The path is lazy so a Windows drive
// letter ("C:\src\x.go:12:3: ...") is not mistaken for line 12 of "C".
var vetDiagRe = regexp.MustCompile(`^(.+?):(\d+)(?::\d+)?: (.+)$`)

// govulncheckMessage is one message of govulncheck's streaming JSON
// (golang.org/x/vuln/internal/govulncheck): exactly one field is set.
type govulncheckMessage struct {
	OSV     *govulncheckOSV     `json:"osv"`
	Finding *govulncheckFinding `json:"finding"`
}

type govulncheckOSV struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

type govulncheckFinding struct {
	OSV          string             `json:"osv"`
	FixedVersion string             `json:"fixed_version"`
	Trace        []govulncheckFrame `json:"trace"`
}

type govulncheckFrame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
	Position *struct {
		Filename string `json:"filename"`
		Line     int    `json:"line"`
	} `json:"position"`
}

// parseGovulncheckJSON reads govulncheck's newline-delimited JSON stream.
//
// Three properties of that stream shape the code:
//
//   - The same vulnerability is emitted once per scan level (module, package,
//     symbol), so findings are deduplicated on (osv, file, line).
//   - Message order is unspecified, so a summary arriving after its finding
//     patches the finding it belongs to.
//   - Non-JSON stdout (progress noise, `go:` lines) is skipped, but a stream
//     with no decodable message at all is an error, not a clean zero.
//
// Severity: the Go vulnerability database carries no ratings, so reachability
// is the signal — a trace frame with a source position means the vulnerable
// symbol is called from this repo (high); package-only is imported (medium);
// module-only is required (low).
func parseGovulncheckJSON(r io.Reader, tg scanTarget, c *collector) error {
	st := &govulncheckStream{
		tg:        tg,
		c:         c,
		summaries: map[string]string{},
		osvIndex:  map[string]int{},
	}
	// Message framing is read by a JSON decoder, not by splitting lines:
	// govulncheck documents "streaming JSON" but v1.1.4 pretty-prints every
	// message across several lines (verified against the real binary), and a
	// decoder accepts that concatenation and one-object-per-line output alike.
	dec := json.NewDecoder(r)
	var (
		decoded  int
		firstErr error
	)
	for {
		var m govulncheckMessage
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The decoder cannot resync mid-stream, so the rest of the
			// output is lost — that has to be said out loud.
			firstErr = err
			break
		}
		decoded++
		if rerr := st.record(m); rerr != nil {
			return rerr
		}
	}
	switch {
	case decoded == 0 && firstErr != nil:
		return fmt.Errorf("govulncheck: unreadable output: %w", firstErr)
	case decoded == 0:
		return errors.New("govulncheck: no JSON messages in output")
	case firstErr != nil:
		c.note = oneLine(fmt.Sprintf("output cut short after %d messages: %v", decoded, firstErr), 200)
	}
	return nil
}

// govulncheckStream is the state one streaming parse carries: summaries arrive
// in their own messages, and one vulnerability is reported once per scan level
// (module/package/symbol).
type govulncheckStream struct {
	tg        scanTarget
	c         *collector
	summaries map[string]string
	// osvIndex maps a vulnerability to the row already recorded for it, so
	// repeated reports merge into it and a late summary still reaches it.
	// Bounded by the finding cap.
	osvIndex map[string]int
}

// record folds one message into the collector.
func (st *govulncheckStream) record(m govulncheckMessage) error {
	if m.OSV != nil && m.OSV.ID != "" {
		st.summaries[m.OSV.ID] = m.OSV.Summary
		if i, ok := st.osvIndex[m.OSV.ID]; ok {
			f := st.c.findings[i]
			f.Message = patchSummary(f.Message, m.OSV.ID, m.OSV.Summary)
			st.c.findings[i] = f
		}
		return nil
	}
	if m.Finding == nil || m.Finding.OSV == "" {
		return nil
	}
	file, line, called, imported, module, version := govulncheckTrace(m.Finding.Trace)
	severity := "low"
	switch {
	case called:
		severity = "high"
	case imported:
		severity = "medium"
	}
	f := SecurityFinding{
		Scanner:  "govulncheck",
		Severity: severity,
		File:     displayPath(st.tg, file),
		Line:     line,
		Message:  oneLine(govulncheckText(m.Finding.OSV, st.summaries[m.Finding.OSV], module, version, m.Finding.FixedVersion), scanMessageLimit),
	}
	// One row per vulnerability: the raw stream repeats a vulnerability at
	// every scan level and for every call site, which would drown the merged
	// list for no extra information.
	if i, ok := st.osvIndex[m.Finding.OSV]; ok {
		st.c.findings[i] = mergeGovulncheck(st.c.findings[i], f)
		return nil
	}
	if err := st.c.add(f); err != nil {
		return err
	}
	st.osvIndex[m.Finding.OSV] = len(st.c.findings) - 1
	return nil
}

// mergeGovulncheck folds a repeat report into the row already recorded: a
// located report (the vulnerable symbol is called from this repo) replaces a
// module-level mention, and the higher severity wins.
func mergeGovulncheck(old, next SecurityFinding) SecurityFinding {
	if old.Line == 0 && next.Line > 0 {
		old.File, old.Line, old.Message = next.File, next.Line, next.Message
	}
	if severityRank[next.Severity] > severityRank[old.Severity] {
		old.Severity = next.Severity
	}
	return old
}

// govulncheckTrace reduces a trace to the actionable location plus what the
// severity needs. The last frame with a position is the entry point in the
// caller's own code (frames run from the vulnerable symbol to the entry
// point), which is more useful than the dependency's module-cache path.
func govulncheckTrace(trace []govulncheckFrame) (file string, line int, called, imported bool, module, version string) {
	if len(trace) > 0 {
		module, version = trace[0].Module, trace[0].Version
	}
	for _, fr := range trace {
		if fr.Package != "" {
			imported = true
		}
		if fr.Position != nil && fr.Position.Filename != "" && fr.Position.Line > 0 {
			called = true
			file, line = fr.Position.Filename, fr.Position.Line
		}
	}
	return file, line, called, imported, module, version
}

// govulncheckText renders a finding's one-line message.
func govulncheckText(id, summary, module, version, fixed string) string {
	head := id
	if summary != "" {
		head += ": " + summary
	}
	var parts []string
	if module != "" {
		if version != "" {
			module += "@" + version
		}
		parts = append(parts, "module "+module)
	}
	if fixed != "" {
		parts = append(parts, "fixed in "+fixed)
	}
	if len(parts) > 0 {
		head += " (" + strings.Join(parts, ", ") + ")"
	}
	return head
}

// patchSummary upgrades a message that only carries the vulnerability id to
// the full "id: summary" form once the summary arrives.
func patchSummary(msg, id, summary string) string {
	if summary == "" || strings.HasPrefix(msg, id+":") {
		return msg
	}
	return strings.Replace(msg, id, id+": "+summary, 1)
}

// parseSemgrepJSON reads semgrep's JSON report. `results` is streamed one
// element at a time so a repo-wide report never lands in memory whole, and
// `errors` is surfaced: semgrep reports rule-fetch and parse failures in the
// body rather than through its exit code, so a "0 findings" run that actually
// failed must not read as success.
func parseSemgrepJSON(r io.Reader, tg scanTarget, c *collector) error {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("semgrep: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("semgrep: expected a JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("semgrep: %w", err)
		}
		key, _ := keyTok.(string)
		switch key {
		case "results":
			if err := streamJSONArray(dec, func(raw json.RawMessage) error {
				var m semgrepMatch
				if jerr := json.Unmarshal(raw, &m); jerr != nil {
					return fmt.Errorf("semgrep: result: %w", jerr)
				}
				msg := oneLine(m.Extra.Message, scanMessageLimit)
				if m.CheckID != "" {
					msg = oneLine(m.CheckID+": "+m.Extra.Message, scanMessageLimit)
				}
				return c.add(SecurityFinding{
					Scanner:  "semgrep",
					Severity: normalizeSeverity(m.Extra.Severity),
					File:     displayPath(tg, m.Path),
					Line:     m.Start.Line,
					Message:  msg,
				})
			}); err != nil {
				return err
			}
		case "errors":
			var errs []struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if derr := dec.Decode(&errs); derr != nil {
				return fmt.Errorf("semgrep: errors: %w", derr)
			}
			if len(errs) > 0 {
				msg := errs[0].Message
				if msg == "" {
					msg = errs[0].Type
				}
				c.note = fmt.Sprintf("%d error(s): %s", len(errs), oneLine(msg, 200))
			}
		default:
			// paths/time/version/explanations are not needed.
			var skip json.RawMessage
			if derr := dec.Decode(&skip); derr != nil {
				return fmt.Errorf("semgrep: %s: %w", key, derr)
			}
		}
	}
	return nil
}

type semgrepMatch struct {
	CheckID string `json:"check_id"`
	Path    string `json:"path"`
	Start   struct {
		Line int `json:"line"`
	} `json:"start"`
	Extra struct {
		Message  string `json:"message"`
		Severity string `json:"severity"`
	} `json:"extra"`
}

// parseGitleaksJSON reads gitleaks' report (a JSON array of findings, streamed
// for the same reason as semgrep's). Secrets are redacted by the invocation
// flags, and the secret value is never echoed into the result.
func parseGitleaksJSON(r io.Reader, tg scanTarget, c *collector) error {
	dec := json.NewDecoder(r)
	return streamJSONArray(dec, func(raw json.RawMessage) error {
		var f gitleaksFinding
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("gitleaks: finding: %w", err)
		}
		msg := f.RuleID
		if f.Description != "" {
			if msg != "" {
				msg += ": "
			}
			msg += f.Description
		}
		if msg == "" {
			msg = "secret detected"
		}
		msg = oneLine(msg+" (secret redacted)", scanMessageLimit)
		return c.add(SecurityFinding{
			Scanner: "gitleaks",
			// gitleaks findings carry no severity; a leak is always worth
			// acting on, so every one of them is high.
			Severity: "high",
			File:     displayPath(tg, f.File),
			Line:     f.StartLine,
			Message:  msg,
		})
	})
}

type gitleaksFinding struct {
	RuleID      string `json:"RuleID"`
	Description string `json:"Description"`
	File        string `json:"File"`
	StartLine   int    `json:"StartLine"`
}

// streamJSONArray hands the elements of a top-level JSON array to fn one at a
// time: each element is decoded lazily, so a report with a million results
// costs one result of memory, and errScanCap stops the stream without reading
// the rest.
func streamJSONArray(dec *json.Decoder, fn func(json.RawMessage) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errors.New("expected a JSON array")
	}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if err := fn(raw); err != nil {
			return err
		}
	}
	// The closing ']' must be consumed, or the caller's object walk resumes
	// on a stray delimiter and silently skips every later key (semgrep's
	// errors[] are the reason that would hurt).
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// displayPath makes a scanner-reported path readable and stable: paths inside
// the scanned tree are shown relative to it, everything else is left alone.
func displayPath(tg scanTarget, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(tg.workDir, p)
	}
	if rel, err := filepath.Rel(tg.path, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if rel == "." {
			return filepath.Base(abs)
		}
		return rel
	}
	return p
}

// sortFindings orders the merged list by severity (highest first, with
// unrated findings above informational ones), then by scanner, file and line,
// so repeat runs diff cleanly.
func sortFindings(findings []SecurityFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if ra, rb := severityRank[a.Severity], severityRank[b.Severity]; ra != rb {
			return ra > rb
		}
		if a.Scanner != b.Scanner {
			return a.Scanner < b.Scanner
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
}

// renderSecurityScan builds the model-facing text: the merged findings, a
// truncation marker when the cap cut the list, and one status line per
// scanner — skips, timeouts and failures included, because the footer is the
// record that nothing was silently dropped.
//
// ponytail: no artifact spill — the merged list is capped, every parser
// streams, and stderr is kept in a bounded sink, so nothing large is buffered
// or written to disk. Ceiling: when a scanner fails we quote its stderr tail
// rather than the full raw output; the upgrade path is teeing the raw stream
// to a temp file whenever status != ok.
func renderSecurityScan(tg scanTarget, findings []SecurityFinding, total int, statuses []ScannerStatus) string {
	var b strings.Builder
	switch len(findings) {
	case 0:
		fmt.Fprintf(&b, "security_scan: no findings in %s\n", tg.path)
	default:
		fmt.Fprintf(&b, "security_scan: %d finding(s) in %s\n", len(findings), tg.path)
	}
	for _, f := range findings {
		loc := "-"
		switch {
		case f.File != "" && f.Line > 0:
			loc = f.File + ":" + strconv.Itoa(f.Line)
		case f.File != "":
			loc = f.File
		}
		fmt.Fprintf(&b, "%-8s %-12s %s  %s\n", strings.ToUpper(f.Severity), f.Scanner, loc, f.Message)
	}
	if total > len(findings) {
		fmt.Fprintf(&b, "[truncated: showing the top %d of %d findings — raise max_findings for more]\n", len(findings), total)
	}
	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		p := s.Scanner + " " + s.Status
		if s.Findings > 0 {
			p += fmt.Sprintf(" (%d)", s.Findings)
		}
		if s.DurationMs >= 1000 {
			p += fmt.Sprintf(" in %.1fs", float64(s.DurationMs)/1000)
		}
		if s.Detail != "" {
			p += ": " + s.Detail
		}
		parts = append(parts, p)
	}
	b.WriteString("scanners: " + strings.Join(parts, " | ") + "\n")
	return b.String()
}

// oneLine collapses whitespace and truncates, so a multi-line scanner message
// cannot break the result layout.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	// Byte length is the fast reject; the cut itself is by rune so a scanner
	// message with non-ASCII text cannot end in a half-encoded byte.
	if max > 0 && len(s) > max {
		if r := []rune(s); len(r) > max {
			s = string(r[:max]) + "…"
		}
	}
	return s
}
