package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	DefaultTimeoutSecs = 120
	MaxTimeoutSecs     = 600
	// Per-stream sink windows: 16KB head + 16KB tail.
	StreamHeadLimit = 16 * 1024
	StreamTailLimit = 16 * 1024
	// Combined output byte cap across both streams; exceeding it kills the
	// child (backpressure — bounded everything).
	CombinedOutputCap = 8 << 20 // 8MB
	// Grace period between SIGTERM and SIGKILL of the process group.
	KillGrace = 2 * time.Second
)

// bashArgs mirrors pi's bash input schema.
type bashArgs struct {
	Command         string `json:"command"`
	Timeout         int    `json:"timeout,omitempty"`
	Workdir         string `json:"workdir,omitempty"`
	RunInBackground bool   `json:"run_in_background,omitempty"`
}

// RewriteBashCommand replaces the command in bash arguments, preserving the
// other fields (timeout, workdir, run_in_background) so an interceptor's
// rewrite runs under the same call settings (M13 #56). It returns nil when
// the arguments cannot be parsed or re-encoded, which the caller treats as a
// fail-closed denial rather than as "no rewrite".
func RewriteBashCommand(args json.RawMessage, command string) json.RawMessage {
	var v bashArgs
	if err := json.Unmarshal(args, &v); err != nil {
		return nil
	}
	v.Command = command
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return out
}

// bashDetails is persisted in Result.Details.
type bashDetails struct {
	ExitCode    int    `json:"exitCode"`
	DurationMs  int64  `json:"durationMs"`
	Truncated   bool   `json:"truncated"`
	StdoutBytes uint64 `json:"stdoutBytes"`
	StderrBytes uint64 `json:"stderrBytes"`
	Workdir     string `json:"workdir"`
	// Backgrounded marks a result whose process continues in the job
	// registry (explicit run_in_background, or a timeout handoff).
	Backgrounded bool   `json:"backgrounded,omitempty"`
	JobID        int64  `json:"jobId,omitempty"`
	OutputFile   string `json:"outputFile,omitempty"`
}

// BashTool runs one-shot shell commands in a hardened environment.
// Concurrent calls are allowed; every call spawns its own process, so no
// global lock is needed.
type BashTool struct {
	// RootCwd resolves relative workdir arguments.
	RootCwd string
	// Jobs is the background registry; nil → the process-wide shared one.
	Jobs *BashJobs
}

// jobs returns the registry background work is recorded in.
func (b *BashTool) jobs() *BashJobs {
	if b.Jobs == nil {
		return SharedBashJobs()
	}
	return b.Jobs
}

// NewBashTool returns a BashTool rooted at cwd (empty → process cwd).
func NewBashTool(cwd string) *BashTool {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return &BashTool{RootCwd: cwd}
}

func (b *BashTool) Name() string { return "bash" }

func (b *BashTool) Description() string {
	return "Run a shell command and get its output. " +
		"Per-stream output is windowed to the first and last 16KB. " +
		"Set run_in_background to start a detached job instead of waiting; " +
		"/tasks lists background jobs, and a foreground command that hits " +
		"its timeout is handed to the same registry rather than killed."
}

func (b *BashTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "The shell command to run"
    },
    "timeout": {
      "type": "integer",
      "description": "Optional timeout in seconds (default 120, max 600)"
    },
    "workdir": {
      "type": "string",
      "description": "Optional working directory (relative paths resolve against the session cwd)"
    },
    "run_in_background": {
      "type": "boolean",
      "description": "Start the command detached and return immediately with a job id and output-file path"
    }
  },
  "required": ["command"]
}`)
}

// Execute implements tool.Tool. Harness-level failures (malformed args,
// unresolvable workdir, spawn error) return a Go error; command failures
// are encoded in the Result: a non-zero exit keeps IsError=false so the
// model still sees its output, a signal kill sets IsError=true.
func (b *BashTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a bashArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("bash: invalid args: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return Result{}, fmt.Errorf("bash: command is required")
	}

	workdir := b.RootCwd
	if a.Workdir != "" {
		if filepath.IsAbs(a.Workdir) {
			workdir = filepath.Clean(a.Workdir)
		} else {
			workdir = filepath.Join(b.RootCwd, a.Workdir)
		}
		if st, err := os.Stat(workdir); err != nil || !st.IsDir() {
			return Result{}, fmt.Errorf("bash: workdir %q is not a directory", workdir)
		}
	}

	if a.RunInBackground {
		return b.startBackground(a.Command, workdir)
	}

	timeout := DefaultTimeoutSecs
	if a.Timeout > 0 {
		timeout = min(a.Timeout, MaxTimeoutSecs)
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	res, execErr := runShell(ctx, runCtx, a.Command, workdir, b.jobs(), time.Duration(timeout)*time.Second)
	if execErr != nil {
		return Result{}, fmt.Errorf("bash: %w", execErr)
	}
	res.Details.Workdir = workdir
	// A command can create, delete, or rename anything below the cwd, so
	// the scan cache cannot be updated incrementally — drop it wholesale.
	SharedFSCache().InvalidateAll()
	return Result{Text: res.Text, Details: res.Details, IsError: res.IsError}, nil
}

// startBackground launches the command detached and returns immediately: a
// janitor reaps it, the job registry reports status/exit code/output tail,
// and combined output streams to a temp file under the system temp dir.
func (b *BashTool) startBackground(command, workdir string) (Result, error) {
	f, err := os.CreateTemp("", "xdev-bg-*.log")
	if err != nil {
		return Result{}, fmt.Errorf("bash: background output file: %w", err)
	}
	name, argv := shellCommand(command)
	cmd := exec.Command(name, argv...)
	cmd.Env = HardenedEnv()
	cmd.Dir = workdir
	prepareProcessGroup(cmd) // no-op on windows
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return Result{}, fmt.Errorf("bash: start: %w", err)
	}
	job := b.jobs().add(command, workdir, f.Name())
	go func() {
		waitErr := cmd.Wait()
		f.Close() // release the fd; the process has been reaped
		code, killed := exitStatus(waitErr)
		job.finish(code, killed)
	}()
	SharedFSCache().InvalidateAll()
	return Result{
		Text: fmt.Sprintf("Started background job #%d: %s\nOutput file: %s\nIt keeps running across turns — /tasks lists jobs and the output file shows progress.", job.ID, command, f.Name()),
		Details: &bashDetails{
			ExitCode:     -1,
			Workdir:      workdir,
			Backgrounded: true,
			JobID:        job.ID,
			OutputFile:   f.Name(),
		},
	}, nil
}

// shellCommand returns the platform shell argv for a command string.
func shellCommand(command string) (name string, argv []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", command}
	}
	return "/bin/bash", []string{"-c", command}
}

// runOutcome carries what runShell observed about one execution.
type runOutcome struct {
	Text    string
	Details *bashDetails
	IsError bool
}

// runShell spawns the command and streams output into windowed sinks. abort
// cancellation (agent stop, Ctrl+C) kills the process group; a runCtx
// timeout does not — the live process is handed to the job registry with an
// explicit notice, and its remaining output tees into a temp file.
func runShell(abortCtx, runCtx context.Context, command, workdir string, jobs *BashJobs, timeout time.Duration) (runOutcome, error) {
	start := time.Now()

	name, argv := shellCommand(command)
	cmd := exec.Command(name, argv...)
	cmd.Env = HardenedEnv()
	cmd.Dir = workdir
	prepareProcessGroup(cmd) // no-op on windows (see kill_windows.go)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runOutcome{}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return runOutcome{}, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return runOutcome{}, fmt.Errorf("start: %w", err)
	}
	pgid := cmd.Process.Pid

	// Combined 8MB cap: crossing it SIGKILLs the process group once.
	var combined atomic.Int64
	var killOnce sync.Once
	killGroup := func() { killOnce.Do(func() { killProcessGroup(pgid, signalKill) }) }
	stdoutSink := NewOutputSink(StreamHeadLimit, StreamTailLimit)
	stderrSink := NewOutputSink(StreamHeadLimit, StreamTailLimit)
	// Output targets are switchable so a timed-out run can keep streaming
	// into a file while the sinks hold what the model already got.
	stdoutW := &switchWriter{w: stdoutSink}
	stderrW := &switchWriter{w: stderrSink}

	// Watchdog: abort only. A timeout is not a kill anymore (see below).
	watch := make(chan struct{})
	go func() {
		select {
		case <-abortCtx.Done():
			killProcessGroup(pgid, signalTerm)
			timer := time.NewTimer(KillGrace)
			defer timer.Stop()
			select {
			case <-timer.C:
				killProcessGroup(pgid, signalKill)
			case <-watch:
			}
		case <-watch:
		}
	}()

	capWriter := func(w io.Writer) io.Writer {
		return &cappedWriter{sink: w, combined: &combined, cap: CombinedOutputCap, kill: killGroup}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for _, r := range []struct {
		src io.Reader
		w   io.Writer
	}{
		{stdout, capWriter(stdoutW)},
		{stderr, capWriter(stderrW)},
	} {
		go func(r io.Reader, w io.Writer) {
			defer wg.Done()
			_, _ = io.Copy(w, r)
		}(r.src, r.w)
	}
	// Order matters: os/exec closes StdoutPipe/StderrPipe handles inside
	// Wait, so Wait must run only AFTER the copiers have seen EOF —
	// otherwise a concurrent batch truncates mid-flight reads to "".
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var waitErr error
	select {
	case <-done:
		close(watch)
		waitErr = cmd.Wait()
	case <-runCtx.Done():
		if abortCtx.Err() == nil {
			return backgroundRun(jobs, cmd, stdoutW, stderrW, stdoutSink, stderrSink, command, workdir, timeout, done, watch, start)
		}
		// Abort raced the timeout: the watchdog is killing; reap normally.
		<-done
		close(watch)
		waitErr = cmd.Wait()
	}

	durationMs := time.Since(start).Milliseconds()
	exitCode, killed := exitStatus(waitErr)
	outText, outTrunc := stdoutSink.Result()
	errText, errTrunc := stderrSink.Result()
	text := joinShellText(outText, errText)
	switch {
	case killed:
		text += "\n[killed: signal]"
		return runOutcome{Text: text, Details: &bashDetails{
			ExitCode: exitCode, DurationMs: durationMs,
			Truncated:   outTrunc || errTrunc,
			StdoutBytes: stdoutSink.Total(), StderrBytes: stderrSink.Total(),
		}, IsError: true}, nil
	case exitCode != 0:
		text += fmt.Sprintf("\n[exit code %d]", exitCode)
	}

	return runOutcome{Text: text, Details: &bashDetails{
		ExitCode: exitCode, DurationMs: durationMs,
		Truncated:   outTrunc || errTrunc,
		StdoutBytes: stdoutSink.Total(), StderrBytes: stderrSink.Total(),
	}, IsError: false}, nil
}

// backgroundRun hands a process that outlived its timeout to the job
// registry: remaining output tees into a temp file, a janitor reaps it, and
// the model gets the partial output plus an explicit notice.
func backgroundRun(jobs *BashJobs, cmd *exec.Cmd, stdoutW, stderrW *switchWriter, stdoutSink, stderrSink *OutputSink, command, workdir string, timeout time.Duration, done <-chan struct{}, watch chan struct{}, start time.Time) (runOutcome, error) {
	f, err := os.CreateTemp("", "xdev-bg-*.log")
	if err != nil {
		// No file to own the continuation: kill rather than leave an
		// unobservable orphan.
		killProcessGroup(cmd.Process.Pid, signalKill)
		<-done
		close(watch)
		waitErr := cmd.Wait()
		exitCode, killed := exitStatus(waitErr)
		outText, outTrunc := stdoutSink.Result()
		errText, errTrunc := stderrSink.Result()
		text := joinShellText(outText, errText)
		if killed {
			text += "\n[killed: signal]"
		}
		text += fmt.Sprintf("\n[timeout after %s, and backgrounding failed: %v]", timeout, err)
		return runOutcome{Text: text, Details: &bashDetails{
			ExitCode: exitCode, DurationMs: time.Since(start).Milliseconds(),
			Truncated:   outTrunc || errTrunc,
			StdoutBytes: stdoutSink.Total(), StderrBytes: stderrSink.Total(),
		}, IsError: true}, nil
	}

	stdoutW.set(io.MultiWriter(stdoutSink, f))
	stderrW.set(io.MultiWriter(stderrSink, f))
	job := jobs.add(command, workdir, f.Name())
	go func() {
		<-done
		close(watch)
		waitErr := cmd.Wait()
		f.Close()
		code, killed := exitStatus(waitErr)
		job.finish(code, killed)
	}()

	outText, outTrunc := stdoutSink.Result()
	errText, errTrunc := stderrSink.Result()
	text := joinShellText(outText, errText)
	text += fmt.Sprintf("\n[timed out after %s — still running as background job #%d; output: %s]", timeout, job.ID, f.Name())
	return runOutcome{Text: text, Details: &bashDetails{
		ExitCode: -1, DurationMs: time.Since(start).Milliseconds(),
		Truncated:   outTrunc || errTrunc,
		StdoutBytes: stdoutSink.Total(), StderrBytes: stderrSink.Total(),
		Backgrounded: true, JobID: job.ID, OutputFile: f.Name(),
	}, IsError: false}, nil
}

// joinShellText combines stdout and stderr the way the tool has always
// reported them.
func joinShellText(outText, errText string) string {
	text := outText
	if errText != "" {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "--- stderr ---\n" + errText
	}
	return text
}

// switchWriter forwards writes to a swappable target (sink → sink+file when
// a timed-out run is backgrounded).
type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *switchWriter) set(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w = w
}

// cappedWriter counts combined bytes across both streams and fires the
// kill callback once the cap is exceeded; the sink underneath stays
// bounded by its windows regardless.
type cappedWriter struct {
	sink     io.Writer
	combined *atomic.Int64
	cap      int64
	kill     func()
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n, _ := w.sink.Write(p)
	if total := w.combined.Add(int64(n)); total > w.cap {
		w.kill()
	}
	return n, nil
}

// exitStatus maps a cmd.Wait error to (exit code, killed-by-signal).
func exitStatus(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1, false
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal()), true
	}
	return exitErr.ExitCode(), false
}

func isWindows() bool { return runtime.GOOS == "windows" }
