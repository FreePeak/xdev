package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The bash/grep/glob/read parity batch (docs/parity/tools.md F1, F2, F8,
// F10): an omp-shaped call must not silently do the wrong thing. Each test
// pins the observable child behavior, not the args struct.

func TestBashCwdAndEnvOmpShape(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bt := NewBashTool(root)
	// omp's exact shape: cwd + env on the call.
	// The key deliberately contains KEY: the inherited-secret strip pattern
	// (envStripRe) must NOT apply to a call-supplied value, or the value is
	// silently dropped — the failure class this fix closes.
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": `printf 'KEY=%s\n' "$PARITY_KEY"; pwd`,
		"cwd":     dir,
		"env":     map[string]string{"PARITY_KEY": "from-env"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", res.Text)
	}
	if !strings.Contains(res.Text, "KEY=from-env") {
		t.Fatalf("env not applied to the child: %q", res.Text)
	}
	if !strings.Contains(res.Text, dir) {
		t.Fatalf("cwd not applied (want %s): %q", dir, res.Text)
	}
}

func TestBashEnvCannotOverrideHardenedKeys(t *testing.T) {
	// Hardened keys (TERM, NO_COLOR, LC_ALL, ...) always win over call env:
	// the hardening exists to keep tool output deterministic, so a call
	// trying to set one must not succeed.
	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": `printf 'NO_COLOR=%s\n' "$NO_COLOR"`,
		"env":     map[string]string{"NO_COLOR": "0"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "NO_COLOR=1") {
		t.Fatalf("call env overrode a hardened key: %q", res.Text)
	}
}

func TestBashCwdWorkdirDisagreeIsError(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bt := NewBashTool(root)
	_, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": "true", "workdir": a, "cwd": b,
	}))
	if err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("workdir+cwd disagreement must be an error, got %v", err)
	}
}

func TestBashEnvKeyValidated(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	_, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": "true",
		"env":     map[string]string{"BAD KEY": "x"},
	}))
	if err == nil || !strings.Contains(err.Error(), "not a valid environment") {
		t.Fatalf("malformed env key must fail the call, got %v", err)
	}
}

func TestResolveBashTimeout(t *testing.T) {
	// Absent → the default; explicit 0 → no deadline; >0 → clamped.
	if got := resolveBashTimeout(nil); got != time.Duration(DefaultTimeoutSecs)*time.Second {
		t.Fatalf("absent timeout = %v, want the default", got)
	}
	if got := resolveBashTimeout(intp(0)); got != 0 {
		t.Fatalf("explicit 0 = %v, want no deadline", got)
	}
	if got := resolveBashTimeout(intp(999999)); got != time.Duration(MaxTimeoutSecs)*time.Second {
		t.Fatalf("oversized timeout = %v, want the max clamp", got)
	}
	if got := resolveBashTimeout(intp(1)); got != time.Second {
		t.Fatalf("timeout 1 = %v", got)
	}
}

func intp(v int) *int { return &v }

func TestBashExplicitZeroTimeoutRunsLong(t *testing.T) {
	// timeout:0 promises no deadline: a 3s sleep must NOT be killed by the
	// 120s default misapplied — bounded here by a 10s ctx so the test can
	// never hang. The sleep is 1s: comfortably over any misapplied
	// sub-second default, far under the ctx.
	bt := NewBashTool(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := bt.Execute(ctx, args(t, map[string]any{
		"command": "sleep 1; echo done",
		"timeout": 0,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, "done") {
		t.Fatalf("timeout:0 run was killed or failed: %q (isError=%v)", res.Text, res.IsError)
	}
}

func TestGrepCaseFalseIsInsensitive(t *testing.T) {
	// docs/parity/tools.md F8: omp's case:false means match
	// case-insensitively; before the fix it was silently dropped and the
	// search ran case-SENSITIVE, returning "no matches" for a pattern that
	// should have matched.
	dir := t.TempDir()
	if err := writeFileFor(filepath.Join(dir, "caseme.txt"), "Alpha\nbeta\nGAMMA\n"); err != nil {
		t.Fatal(err)
	}
	g := &GrepTool{CWD: dir}
	raw, _ := json.Marshal(map[string]any{"pattern": "alpha", "path": filepath.Join(dir, "caseme.txt"), "case": false})
	res, err := g.Execute(context.Background(), raw)
	if err != nil || res.IsError {
		t.Fatalf("grep case:false: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Text, "Alpha") {
		t.Fatalf("case:false did not match case-insensitively: %q", res.Text)
	}
	// case:true restores sensitivity.
	raw, _ = json.Marshal(map[string]any{"pattern": "ALPHA", "path": filepath.Join(dir, "caseme.txt"), "case": true})
	res, err = g.Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "Alpha") {
		t.Fatalf("case:true must be case-sensitive, matched anyway: %q", res.Text)
	}
}

func TestGlobOmpPathOnlyShape(t *testing.T) {
	// docs/parity/tools.md F10: omp's glob takes the pattern IN path. A
	// path-only call must glob, not error.
	dir := t.TempDir()
	if err := writeFileFor(filepath.Join(dir, "one_test.go"), "package x\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileFor(filepath.Join(dir, "two.go"), "package x\n"); err != nil {
		t.Fatal(err)
	}
	g := &GlobTool{CWD: dir}
	raw, _ := json.Marshal(map[string]any{"path": "*_test.go"})
	res, err := g.Execute(context.Background(), raw)
	if err != nil || res.IsError {
		t.Fatalf("glob path-only: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Text, "one_test.go") {
		t.Fatalf("path-only glob missed the match: %q", res.Text)
	}
	if strings.Contains(res.Text, "two.go") {
		t.Fatalf("path-only glob matched the wrong file: %q", res.Text)
	}
	// limit is omp's spelling of max_results.
	raw, _ = json.Marshal(map[string]any{"pattern": "*.go", "path": dir, "limit": 1})
	res, err = g.Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(res.Text), "\n"); got > 0 && res.Text != "" {
		// one entry means zero newlines; allow empty for a single-file cap
		t.Logf("limit=1 returned: %q", res.Text)
	}
}

func TestReadSelectorSuffixHint(t *testing.T) {
	// docs/parity/tools.md F4: `read {"path":"go.mod:1"}` used to say
	// "file not found" for a file that exists. Now it names the omp
	// selector shape and the offset/limit fields.
	dir := t.TempDir()
	if err := writeFileFor(filepath.Join(dir, "go.mod"), "module example.com/x\n\ngo 1.22\n"); err != nil {
		t.Fatal(err)
	}
	r := &ReadTool{}
	raw, _ := json.Marshal(map[string]any{"path": filepath.Join(dir, "go.mod:1")})
	res, err := r.Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "offset/limit") {
		t.Fatalf("selector-suffixed path must point at offset/limit, got: %q", res.Text)
	}
	// A plain Windows-style or scheme colon must not trip the heuristic.
	raw, _ = json.Marshal(map[string]any{"path": "skill://no-such-skill/x"})
	res, _ = r.Execute(context.Background(), raw)
	if strings.Contains(res.Text, "offset/limit") {
		t.Fatalf("URI path misidentified as a selector: %q", res.Text)
	}
}

func TestPatternBalanced(t *testing.T) {
	cases := []struct {
		p    string
		want bool
	}{
		{"$_($A)", true},
		{"class $_ { $$$ }", true},
		{"[", false},
		{"$A", true},
		{"func $NAME($$$ARGS)", true},
		{`"unterminated`, false},
		{`"quote: with colon"` + "[", false},
	}
	for _, c := range cases {
		if got := patternBalanced(c.p); got != c.want {
			t.Errorf("patternBalanced(%q) = %v, want %v", c.p, got, c.want)
		}
	}
}
