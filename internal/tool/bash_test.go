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
		jobs.add(fmt.Sprintf("job %d", i), "", "", 0, nil)
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

// TestBashEvictionKeepsRunningHandles pins which entry a full registry may
// drop: a finished job's id is a record, a running job's id is the only way to
// stop it (#127), so the running one must survive.
func TestBashEvictionKeepsRunningHandles(t *testing.T) {
	jobs := NewBashJobs()
	running, _ := jobs.add("sleep forever", "", "", 4242, nil)
	for i := range bashJobsCap + 1 {
		j, _ := jobs.add(fmt.Sprintf("done %d", i), "", "", 0, nil)
		j.finish(0, false)
	}
	if _, ok := jobs.Get(running.ID); !ok {
		t.Fatal("a running job lost its handle while finished ones held slots")
	}
	list := jobs.List()
	if list[0].ID != running.ID {
		t.Fatalf("the surviving running job must be first, got #%d", list[0].ID)
	}
	if running.Done() {
		t.Fatal("the survivor must still be running")
	}
}

// waitFor polls a condition through the tool surface itself, so the test reads
// the job the way a later turn would.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (b *BashTool) call(t *testing.T, ctx context.Context, fields map[string]any) (Result, error) {
	t.Helper()
	return b.Execute(ctx, args(t, fields))
}

// TestBashJobControlAcrossCalls is #127's acceptance, through the model-facing
// tool surface and not the TUI: start a detached job that waits for input, then
// in later calls observe it, answer its prompt, read the reply, and stop it.
func TestBashJobControlAcrossCalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell loop and signals")
	}
	bt := NewBashTool(t.TempDir())
	bt.Jobs = NewBashJobs()
	ctx := context.Background()

	res, err := bt.call(t, ctx, map[string]any{
		"command":           "printf ready\\n; while IFS= read -r l; do printf \"got:%s\\n\" \"$l\"; done",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Details.(*bashDetails)
	id := d.JobID

	// status with no job lists; status with the job reports the facts a later
	// turn needs: state, pid, and whether its input is reachable.
	live, err := bt.call(t, ctx, map[string]any{"action": "status"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(live.Text, fmt.Sprintf("#%d", id)) || !strings.Contains(live.Text, "Background bash jobs (1)") {
		t.Fatalf("status listing = %q", live.Text)
	}
	one, err := bt.call(t, ctx, map[string]any{"action": "status", "job": id})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(one.Text, "pid "+fmt.Sprint(d.PID)) || !strings.Contains(one.Text, "stdin: writable") {
		t.Fatalf("status(#%d) = %q", id, one.Text)
	}
	waitFor(t, "the job's own ready line", func() bool {
		r, err := bt.call(t, ctx, map[string]any{"action": "logs", "job": id})
		return err == nil && strings.Contains(r.Text, "ready")
	})

	// write: exact bytes to a detached stdin — the thing that makes a REPL or a
	// y/N prompt reachable at all.
	wr, err := bt.call(t, ctx, map[string]any{"action": "write", "job": id, "input": "hello\n"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(wr.Text, "Sent 6 byte(s)") {
		t.Fatalf("write = %q", wr.Text)
	}
	waitFor(t, "the job to answer on stdin", func() bool {
		r, err := bt.call(t, ctx, map[string]any{"action": "logs", "job": id})
		return err == nil && strings.Contains(r.Text, "got:hello")
	})

	// stop: the process group is gone, and the reply says which state it ended
	// in rather than claiming success unconditionally.
	st, err := bt.call(t, ctx, map[string]any{"action": "stop", "job": id})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !strings.Contains(st.Text, "Stopped background job") || !strings.Contains(st.Text, "now signal") {
		t.Fatalf("stop = %q", st.Text)
	}
	job, ok := bt.Jobs.Get(id)
	if !ok || !job.Done() {
		t.Fatalf("job #%d must be reaped after stop (ok=%v)", id, ok)
	}
	// A stopped job stays addressable (its exit is the answer to a later
	// question), but its input is closed.
	after, err := bt.call(t, ctx, map[string]any{"action": "write", "job": id, "input": "more\n"})
	if err == nil {
		t.Fatalf("writing a finished job must fail, got %q", after.Text)
	}
	if !strings.Contains(err.Error(), "already finished") {
		t.Fatalf("err = %v", err)
	}
	// Stopping it twice is not an error worth a model turn: it is already true.
	if _, err := bt.call(t, ctx, map[string]any{"action": "stop", "job": id}); err != nil {
		t.Fatalf("re-stop: %v", err)
	}
}

// TestBashJobControlRefusesUnknownIds is the loop-level assertion #127 asks
// for: an id the registry does not hold is refused at the seam, so a model can
// never read "stopped" about a process it never touched.
func TestBashJobControlRefusesUnknownIds(t *testing.T) {
	bt := NewBashTool(t.TempDir())
	bt.Jobs = NewBashJobs()
	ctx := context.Background()
	for _, verb := range []string{"logs", "stop", "write"} {
		fields := map[string]any{"action": verb, "job": int64(4242)}
		if verb == "write" {
			fields["input"] = "x\n"
		}
		_, err := bt.call(t, ctx, fields)
		if err == nil || !errors.Is(err, ErrUnknownJob) {
			t.Errorf("action=%s unknown id: err = %v, want ErrUnknownJob", verb, err)
			continue
		}
		if !strings.Contains(err.Error(), "4242") {
			t.Errorf("action=%s must name the refused id: %v", verb, err)
		}
	}
	// A control verb with no job id says how to find one; an unknown verb and a
	// command+action collision are errors, not silent no-ops.
	if _, err := bt.call(t, ctx, map[string]any{"action": "stop"}); err == nil || !strings.Contains(err.Error(), "action=status lists") {
		t.Errorf("missing job: err = %v", err)
	}
	if _, err := bt.call(t, ctx, map[string]any{"action": "destroy", "job": int64(1)}); err == nil || !strings.Contains(err.Error(), `unknown action "destroy"`) {
		t.Errorf("unknown verb: err = %v", err)
	}
	if _, err := bt.call(t, ctx, map[string]any{"action": "status", "command": "echo x"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("action+command: err = %v", err)
	}
	if _, err := bt.call(t, ctx, map[string]any{"action": "write", "job": int64(1)}); err == nil || !strings.Contains(err.Error(), "needs input") {
		t.Errorf("write with no input: err = %v", err)
	}
	// A plain call still needs a command, and the message names the verbs.
	if _, err := bt.call(t, ctx, map[string]any{"command": "  "}); err == nil || !strings.Contains(err.Error(), "action=status|logs|stop|write") {
		t.Errorf("empty command: err = %v", err)
	}
}

// TestBashHandedOffJobReportsNoInput: a job the timeout handed to the registry
// can be stopped and read, but its stdin was the pipeline's and is gone — the
// write verb must say that instead of blocking on a pipe nobody answers.
func TestBashHandedOffJobReportsNoInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sleep and signals")
	}
	bt := NewBashTool(t.TempDir())
	bt.Jobs = NewBashJobs()
	ctx := context.Background()
	res, err := bt.call(t, ctx, map[string]any{"command": "sleep 30", "timeout": 1})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Details.(*bashDetails)
	if !d.Backgrounded {
		t.Fatalf("the timed-out run must hand off: %+v", d)
	}
	if !strings.Contains(res.Text, "action=stop") {
		t.Fatalf("the handoff notice must name the control: %q", res.Text)
	}
	status, err := bt.call(t, ctx, map[string]any{"action": "status", "job": d.JobID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Text, "not writable") {
		t.Fatalf("status = %q", status.Text)
	}
	if _, err := bt.call(t, ctx, map[string]any{"action": "write", "job": d.JobID, "input": "y\n"}); err == nil || !strings.Contains(err.Error(), "foreground timeout") {
		t.Fatalf("write on a handoff: err = %v", err)
	}
	if _, err := bt.call(t, ctx, map[string]any{"action": "stop", "job": d.JobID}); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
