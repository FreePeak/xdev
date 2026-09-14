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
	"regexp"
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

// bashArgs mirrors pi's bash input schema. Timeout is a pointer so an
// EXPLICIT 0 ("no deadline", omp's semantic) is distinguishable from an
// absent field (the 120s default) — json cannot tell those apart on a plain
// int, and silently killing at 120s after promising no deadline is the
// worst kind of lie.
type bashArgs struct {
	Command         string            `json:"command"`
	Timeout         *int              `json:"timeout,omitempty"`
	Workdir         string            `json:"workdir,omitempty"`
	Cwd             string            `json:"cwd,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	RunInBackground bool              `json:"run_in_background,omitempty"`
	// Action addresses a background job instead of starting a command (#127):
	// status | logs | stop | write. Empty means run. These verbs reach only jobs
	// this session started, by registry id — they name no process.
	Action string `json:"action,omitempty"`
	// Job is the registry id an action applies to. A pointer so an explicit
	// job:0 is reported as a bad id rather than read as "no job given".
	Job   *int64 `json:"job,omitempty"`
	Input string `json:"input,omitempty"`
	Lines int    `json:"lines,omitempty"`
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

// resolveBashTimeout maps the parsed timeout field onto the run deadline:
// absent → the default (an omp-shaped call that omits timeout keeps the
// safety net); explicit 0 → no deadline (omp's documented semantic, only
// distinguishable from absent with a pointer); >0 → clamped to the max.
func resolveBashTimeout(t *int) time.Duration {
	if t == nil {
		return time.Duration(DefaultTimeoutSecs) * time.Second
	}
	if *t == 0 {
		return 0
	}
	return time.Duration(min(*t, MaxTimeoutSecs)) * time.Second
}

// envKeyRe is the valid environment-variable name shape the bash tool's
// env map accepts.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	// PID lets a user or a shell act on the job the harness started (kill -0,
	// lsof -p); the tool's own verbs address it by JobID, never by pid.
	PID int `json:"pid,omitempty"`
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
		"Set run_in_background to start a detached job that keeps running across " +
		"turns with a job id and a writable stdin; control those jobs with " +
		"action=status|logs|stop|write and job=<id>. A foreground command that " +
		"hits its timeout is handed to the same registry rather than killed."
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
      "description": "Optional timeout in seconds (default 120, max 600; 0 disables the deadline — the run is then bounded only by the session interrupt)"
    },
    "workdir": {
      "type": "string",
      "description": "Optional working directory (relative paths resolve against the session cwd)"
    },
    "cwd": {
      "type": "string",
      "description": "Alias of workdir (omp spelling). Giving both with different values is an error"
    },
    "env": {
      "type": "object",
      "description": "Extra KEY=VALUE environment entries for this command (keys must match [A-Za-z_][A-Za-z0-9_]*); merged over the hardened env — hardened keys (TERM, NO_COLOR, LC_ALL, ...) always win",
      "additionalProperties": {"type": "string"}
    },
    "run_in_background": {
      "type": "boolean",
      "description": "Start the command detached and return immediately with a job id, pid, output-file path and a writable stdin"
    },
    "action": {
      "type": "string",
      "enum": ["status", "logs", "stop", "write"],
      "description": "Control a background job instead of running a command (omit to run one): status lists jobs, or reports one; logs returns its output tail; stop terminates its process group (SIGTERM, then SIGKILL); write sends exact bytes to its stdin, which is how you answer a REPL, a watcher or a y/N prompt. Job ids are only ever this session's own background jobs"
    },
    "job": {
      "type": "integer",
      "description": "The background job id an action applies to (from a run_in_background result, or from action=status)"
    },
    "input": {
      "type": "string",
      "description": "For action=write: the bytes to send to the job's stdin, verbatim — spell a newline as \\n"
    },
    "lines": {
      "type": "integer",
      "description": "For action=logs: how many trailing lines to return (default 400)"
    }
  }
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
	// A job-control call has no command, so it is routed before the command
	// requirement is checked: the two are mutually exclusive, and an unknown
	// verb is an error rather than a silent no-op.
	if strings.TrimSpace(a.Action) != "" {
		return b.jobAction(a)
	}
	if strings.TrimSpace(a.Command) == "" {
		return Result{}, fmt.Errorf("bash: command is required (or pass action=status|logs|stop|write with job)")
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
	// omp spells the working directory cwd. Both spellings with different
	// values are an error: a silent winner is exactly the ambiguity that
	// makes an omp-shaped call run somewhere the caller did not mean.
	if a.Cwd != "" {
		cwd := a.Cwd
		if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(b.RootCwd, cwd)
		}
		cwd = filepath.Clean(cwd)
		if a.Workdir != "" && cwd != workdir {
			return Result{}, fmt.Errorf("bash: workdir %q and cwd %q disagree", a.Workdir, a.Cwd)
		}
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			return Result{}, fmt.Errorf("bash: cwd %q is not a directory", cwd)
		}
		workdir = cwd
	}
	// Extra per-call environment. Keys are validated here (not deep in the
	// spawn) so a malformed entry fails the call instead of being dropped.
	for k := range a.Env {
		if !envKeyRe.MatchString(k) {
			return Result{}, fmt.Errorf("bash: env key %q is not a valid environment name", k)
		}
	}
	childEnv := ApplyCallEnv(HardenedEnv(), a.Env)

	if a.RunInBackground {
		return b.startBackground(a.Command, workdir, childEnv)
	}

	// timeout: absent → the 120s default; explicit 0 → no deadline (the
	// run is bounded only by the session interrupt); >0 → clamped to the
	// 600s max.
	runCtx := ctx
	timeout := resolveBashTimeout(a.Timeout)
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	res, execErr := runShell(ctx, runCtx, a.Command, workdir, childEnv, b.jobs(), timeout)
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
//
// The job keeps a writable stdin (#127), which is what makes a detached REPL,
// watcher, or `y/N` prompt answerable from a later turn. It also means a command
// that reads stdin to EOF will wait for input instead of seeing an immediate
// end — so pipe your own input (`echo x | cmd`) or run it in the foreground when
// that is what you meant.
func (b *BashTool) startBackground(command, workdir string, env []string) (Result, error) {
	f, err := os.CreateTemp("", "xdev-bg-*.log")
	if err != nil {
		return Result{}, fmt.Errorf("bash: background output file: %w", err)
	}
	name, argv := shellCommand(command)
	cmd := exec.Command(name, argv...)
	cmd.Env = env
	cmd.Dir = workdir
	prepareProcessGroup(cmd) // no-op on windows
	cmd.Stdout = f
	cmd.Stderr = f
	stdin, err := cmd.StdinPipe()
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return Result{}, fmt.Errorf("bash: background stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		f.Close()
		os.Remove(f.Name())
		return Result{}, fmt.Errorf("bash: start: %w", err)
	}
	job, evicted := b.jobs().add(command, workdir, f.Name(), cmd.Process.Pid, stdin)
	go func() {
		waitErr := cmd.Wait()
		f.Close() // release the fd; the process has been reaped
		code, killed := exitStatus(waitErr)
		job.finish(code, killed)
	}()
	SharedFSCache().InvalidateAll()
	return Result{
		Text: fmt.Sprintf("Started background job #%d (pid %d): %s\nOutput file: %s\nIt keeps running across turns. Control it with bash action=status|logs|stop|write and job=%d; /tasks lists every job.%s",
			job.ID, cmd.Process.Pid, command, f.Name(), job.ID, evictedNote(evicted)),
		Details: &bashDetails{
			ExitCode:     -1,
			Workdir:      workdir,
			Backgrounded: true,
			JobID:        job.ID,
			OutputFile:   f.Name(),
			PID:          cmd.Process.Pid,
		},
	}, nil
}

// jobAction implements the #127 control verbs: status, logs, stop, write. Every
// one of them fails loudly on an id the registry does not hold, because a job
// the model believes is stopped or listening is a claim about side effects.
func (b *BashTool) jobAction(a bashArgs) (Result, error) {
	if strings.TrimSpace(a.Command) != "" {
		return Result{}, fmt.Errorf("bash: action %q and command are mutually exclusive", a.Action)
	}
	verb := strings.ToLower(strings.TrimSpace(a.Action))
	jobs := b.jobs()
	one := func() (*BashJob, error) {
		if a.Job == nil {
			return nil, fmt.Errorf("bash: action %q needs job=<id> (action=status lists them)", verb)
		}
		return jobs.job(*a.Job)
	}
	switch verb {
	case "status":
		if a.Job == nil {
			return Result{Text: jobs.Render()}, nil
		}
		j, err := one()
		if err != nil {
			return Result{}, err
		}
		return Result{Text: jobLine(j, bashJobTailLines)}, nil
	case "logs":
		j, err := one()
		if err != nil {
			return Result{}, err
		}
		text, _, err := jobs.Logs(j.ID, a.Lines)
		if err != nil {
			return Result{}, err
		}
		if text == "" {
			text = "(no output yet)"
		}
		return Result{Text: fmt.Sprintf("#%d %s — output:\n%s", j.ID, j.State(), text)}, nil
	case "stop":
		j, err := one()
		if err != nil {
			return Result{}, err
		}
		stopped, err := jobs.Stop(j.ID)
		if err != nil {
			return Result{}, err
		}
		SharedFSCache().InvalidateAll()
		return Result{Text: fmt.Sprintf("Stopped background job #%d: now %s.\n%s",
			stopped.ID, stopped.State(), jobLine(stopped, bashJobTailLines))}, nil
	case "write":
		// The argument is checked first: an empty write is a caller mistake
		// whatever the job id is, and answering "no such job" instead would
		// send the model looking for an id that was never the problem.
		if a.Input == "" {
			return Result{}, fmt.Errorf("bash: action=write needs input (the bytes to send; use input=\"\\n\" for a bare Enter)")
		}
		j, err := one()
		if err != nil {
			return Result{}, err
		}
		written, err := jobs.WriteInput(j.ID, a.Input)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: fmt.Sprintf("Sent %d byte(s) to background job #%d (%s).\n%s",
			len(a.Input), written.ID, written.State(), jobLine(written, bashJobTailLines))}, nil
	}
	return Result{}, fmt.Errorf("bash: unknown action %q (status|logs|stop|write)", a.Action)
}

// jobLine renders one job's current facts, with its newest output: what a
// caller needs to decide the next move without a second round-trip.
func jobLine(j *BashJob, tailLines int) string {
	s := fmt.Sprintf("#%d %s pid %d up %s — %s\noutput file: %s\nstdin: ",
		j.ID, j.State(), j.PID(), time.Since(j.Started).Round(time.Second), j.Command, j.OutPath)
	switch {
	case j.Done():
		s += "closed (finished)"
	case j.CanWriteInput():
		s += "writable"
	default:
		s += "not writable (handed off by a foreground timeout)"
	}
	if t := j.Tail(tailLines); t != "" {
		s += "\nlast output:\n"
		for _, line := range strings.Split(t, "\n") {
			s += "  | " + line + "\n"
		}
	}
	return strings.TrimRight(s, "\n")
}

// evictedNote reports a handle the registry had to drop at capacity. A running
// job with no id is worse than a full registry: the process keeps producing
// side effects nobody can stop, so the response says so.
func evictedNote(evicted *BashJob) string {
	if evicted == nil {
		return ""
	}
	if evicted.Done() {
		return fmt.Sprintf("\n(note: finished job #%d left the registry to make room.)", evicted.ID)
	}
	return fmt.Sprintf("\nWARNING: job #%d (%s) is still running but its handle was dropped to make room; it cannot be stopped or written to by id.",
		evicted.ID, evicted.Command)
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
func runShell(abortCtx, runCtx context.Context, command, workdir string, env []string, jobs *BashJobs, timeout time.Duration) (runOutcome, error) {
	start := time.Now()

	name, argv := shellCommand(command)
	cmd := exec.Command(name, argv...)
	cmd.Env = env
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
	// A handoff has no writable stdin: the child's input was the pipeline's and
	// is gone, so the registry holds nil and the write verb says so.
	job, evicted := jobs.add(command, workdir, f.Name(), cmd.Process.Pid, nil)
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
	text += fmt.Sprintf("\n[timed out after %s — still running as background job #%d; output: %s; stop it with bash action=stop job=%d]",
		timeout, job.ID, f.Name(), job.ID)
	text += evictedNote(evicted)
	return runOutcome{Text: text, Details: &bashDetails{
		ExitCode: -1, DurationMs: time.Since(start).Milliseconds(),
		Truncated:   outTrunc || errTrunc,
		StdoutBytes: stdoutSink.Total(), StderrBytes: stderrSink.Total(),
		Backgrounded: true, JobID: job.ID, OutputFile: f.Name(), PID: cmd.Process.Pid,
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
