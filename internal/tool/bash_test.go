package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mustResolve canonicalizes a path so macOS /tmp → /private/tmp symlinks
// don't break prefix comparisons against `pwd`.
func mustResolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestBashEchoRoundTrip(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(context.Background(), args(t, map[string]any{"command": `printf 'hello xdev'`}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", res.Text)
	}
	if res.Text != "hello xdev" {
		t.Fatalf("got %q, want %q", res.Text, "hello xdev")
	}
	d := res.Details.(*bashDetails)
	if d.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", d.ExitCode)
	}
	if d.StdoutBytes != uint64(len("hello xdev")) {
		t.Fatalf("stdoutBytes = %d", d.StdoutBytes)
	}
	if d.DurationMs < 0 || d.Workdir == "" {
		t.Fatalf("bad details: %+v", d)
	}
}

func TestBashExitCodePropagation(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": `echo "about to fail" >&2; exit 3`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Non-zero exit stays IsError=false so the model sees its output.
	if res.IsError {
		t.Fatalf("non-zero exit must keep IsError=false, text=%q", res.Text)
	}
	d := res.Details.(*bashDetails)
	if d.ExitCode != 3 {
		t.Fatalf("exitCode = %d, want 3", d.ExitCode)
	}
	if !strings.Contains(res.Text, "[exit code 3]") {
		t.Fatalf("missing exit-code suffix in %q", res.Text)
	}
	if !strings.Contains(res.Text, "about to fail") || !strings.Contains(res.Text, "--- stderr ---") {
		t.Fatalf("stderr not captured in %q", res.Text)
	}
	if d.StderrBytes == 0 {
		t.Fatal("stderrBytes = 0")
	}
}

func TestBashStderrCaptureOnly(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(context.Background(), args(t, map[string]any{"command": `echo err-out 1>&2`}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "--- stderr ---\nerr-out") {
		t.Fatalf("stderr section malformed: %q", res.Text)
	}
}

func TestBashTimeoutMovesToBackground(t *testing.T) {
	// #16: a timeout no longer kills — the live process is handed to the
	// job registry, keeps running, and its later exit is observable.
	bt := NewBashTool(t.TempDir())
	bt.Jobs = NewBashJobs()
	start := time.Now()
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": "sleep 1; echo done-bg",
		"timeout": 1,
	}))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout handoff took %v, process not detached promptly", elapsed)
	}
	d := res.Details.(*bashDetails)
	if !d.Backgrounded || d.JobID == 0 || d.OutputFile == "" {
		t.Fatalf("timeout result must carry background metadata, got %+v", d)
	}
	if !strings.Contains(res.Text, "background job #") {
		t.Fatalf("missing background notice in %q", res.Text)
	}
	job, ok := bt.Jobs.Get(d.JobID)
	if !ok {
		t.Fatalf("job #%d not in the registry", d.JobID)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !job.Done() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if code, done := job.ExitCode(); !done || code != 0 {
		t.Fatalf("backgrounded job did not finish cleanly: code=%d done=%v state=%s", code, done, job.State())
	}
	if !strings.Contains(job.Tail(5), "done-bg") {
		t.Fatalf("background output file lost the continuation: %q", job.Tail(5))
	}
}

func TestBashTimeoutClampedToMax(t *testing.T) {
	// MaxTimeoutSecs clamping is observable only indirectly; assert the
	// clamp arithmetic through a tiny command rather than sleeping 600s.
	if clamp := min(9999, MaxTimeoutSecs); clamp != 600 {
		t.Fatalf("timeout clamp broken: %d", clamp)
	}
	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": "true",
		"timeout": 9999,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Details.(*bashDetails).ExitCode != 0 {
		t.Fatal("clamped timeout command failed")
	}
}

func TestBashEnvHardeningInProcess(t *testing.T) {
	t.Setenv("XDEV_SECRET", "1")
	t.Setenv("MY_API_TOKEN", "leak-me")
	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(context.Background(), args(t, map[string]any{"command": `env`}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "XDEV_SECRET") || strings.Contains(res.Text, "leak-me") {
		t.Fatalf("sensitive env leaked to child: %q", res.Text)
	}
	if !strings.Contains(res.Text, "TERM=dumb") || !strings.Contains(res.Text, "NO_COLOR=1") {
		t.Fatalf("always-set vars missing from child env: %q", res.Text)
	}
}

func TestBashWorkdirResolution(t *testing.T) {
	root := mustResolve(t, t.TempDir())
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	bt := NewBashTool(root)
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": "pwd",
		"workdir": "sub",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, sub) {
		t.Fatalf("pwd = %q, want prefix %q", res.Text, sub)
	}
	if res.Details.(*bashDetails).Workdir != sub {
		t.Fatalf("details workdir = %q", res.Details.(*bashDetails).Workdir)
	}

	// Absolute workdir outside root is allowed in the MVP.
	other := mustResolve(t, t.TempDir())
	res, err = bt.Execute(context.Background(), args(t, map[string]any{
		"command": "pwd", "workdir": other,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, other) {
		t.Fatalf("abs workdir: pwd = %q, want prefix %q", res.Text, other)
	}
}

func TestBashBadWorkdirErrors(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	if _, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command": "true", "workdir": "does/not/exist",
	})); err == nil {
		t.Fatal("nonexistent workdir must return a Go error")
	}
}

func TestBashMalformedArgsError(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	if _, err := bt.Execute(context.Background(), json.RawMessage(`{not json`)); err == nil {
		t.Fatal("malformed args must return a Go error")
	}
	if _, err := bt.Execute(context.Background(), args(t, map[string]any{"command": "  "})); err == nil {
		t.Fatal("empty command must return a Go error")
	}
}

func TestBashOutputTruncationWindows(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		// ~57KB of stdout — exceeds both 16KB windows.
		"command": fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'x'", 57*1024),
	}))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Details.(*bashDetails)
	if !d.Truncated {
		t.Fatal("expected truncated=true for >32KB output")
	}
	if d.StdoutBytes != uint64(57*1024) {
		t.Fatalf("stdoutBytes = %d, want %d", d.StdoutBytes, 57*1024)
	}
	if !strings.Contains(res.Text, "output truncated") {
		t.Fatal("missing truncation marker")
	}
	if len(res.Text) > StreamHeadLimit+StreamTailLimit+300 {
		t.Fatalf("text not windowed: %d bytes", len(res.Text))
	}
	if !strings.HasSuffix(res.Text, strings.Repeat("x", 10)) {
		t.Fatal("tail window does not end with newest bytes")
	}
}

func TestBashConcurrentCalls(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := bt.Execute(context.Background(), args(t, map[string]any{
				"command": fmt.Sprintf("echo n%d", i),
			}))
			if err != nil {
				errs[i] = err
				return
			}
			if got := strings.TrimRight(res.Text, "\n"); got != fmt.Sprintf("n%d", i) {
				errs[i] = fmt.Errorf("call %d got %q", i, got)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent call %d: %v", i, err)
		}
	}
}

func TestBashProcessGroupKill(t *testing.T) {
	// A background child spawned inside the shell must die with the group
	// when the agent aborts — this is the Setpgid + kill(-pgid) contract.
	// (A plain timeout backgrounds instead; see the handoff test.)
	bt := NewBashTool(t.TempDir())
	bt.Jobs = NewBashJobs()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := bt.Execute(ctx, args(t, map[string]any{
		"command": `sleep 30 & wait`,
		"timeout": 60,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("group kill not prompt: %v", time.Since(start))
	}
	if !res.IsError || !strings.Contains(res.Text, "[killed: signal]") {
		t.Fatalf("expected killed result, got IsError=%v text=%q", res.IsError, res.Text)
	}
}

func TestBashRunInBackgroundReturnsJob(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	bt.Jobs = NewBashJobs()
	res, err := bt.Execute(context.Background(), args(t, map[string]any{
		"command":           "sleep 0.2; exit 7",
		"run_in_background": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Details.(*bashDetails)
	if !d.Backgrounded || d.JobID == 0 || d.OutputFile == "" {
		t.Fatalf("background result must carry a job id and output path: %+v", d)
	}
	if !strings.Contains(res.Text, fmt.Sprintf("job #%d", d.JobID)) {
		t.Fatalf("result text must name the job: %q", res.Text)
	}
	job, ok := bt.Jobs.Get(d.JobID)
	if !ok {
		t.Fatalf("job #%d not registered", d.JobID)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !job.Done() && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	// The exit status lands later — that is the point of the registry.
	if code, done := job.ExitCode(); !done || code != 7 {
		t.Fatalf("exit status = (%d, done=%v), want (7, true); state=%s", code, done, job.State())
	}
}

func TestBashJobsBoundedEviction(t *testing.T) {
	jobs := NewBashJobs()
	for i := range bashJobsCap + 3 {
		jobs.add(fmt.Sprintf("job %d", i), "", "")
	}
	if jobs.Len() != bashJobsCap {
		t.Fatalf("registry cap: len=%d, want %d", jobs.Len(), bashJobsCap)
	}
	list := jobs.List()
	if list[0].ID != 4 || list[len(list)-1].ID != bashJobsCap+3 {
		t.Fatalf("eviction must drop oldest first: first=%d last=%d", list[0].ID, list[len(list)-1].ID)
	}
	if !strings.Contains(jobs.Render(), "Background bash jobs") {
		t.Fatalf("render missing header: %q", jobs.Render())
	}
}
