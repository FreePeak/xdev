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
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	Workdir string `json:"workdir,omitempty"`
}

// bashDetails is persisted in Result.Details.
type bashDetails struct {
	ExitCode    int    `json:"exitCode"`
	DurationMs  int64  `json:"durationMs"`
	Truncated   bool   `json:"truncated"`
	StdoutBytes uint64 `json:"stdoutBytes"`
	StderrBytes uint64 `json:"stderrBytes"`
	Workdir     string `json:"workdir"`
}

// BashTool runs one-shot shell commands in a hardened environment.
// Concurrent calls are allowed; every call spawns its own process, so no
// global lock is needed.
type BashTool struct {
	// RootCwd resolves relative workdir arguments.
	RootCwd string
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
		"Per-stream output is windowed to the first and last 16KB."
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

	timeout := DefaultTimeoutSecs
	if a.Timeout > 0 {
		timeout = min(a.Timeout, MaxTimeoutSecs)
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	res, execErr := runShell(runCtx, a.Command, workdir)
	if execErr != nil {
		return Result{}, fmt.Errorf("bash: %w", execErr)
	}
	res.Details.Workdir = workdir
	return Result{Text: res.Text, Details: res.Details, IsError: res.IsError}, nil
}

// runOutcome carries what runShell observed about one execution.
type runOutcome struct {
	Text    string
	Details *bashDetails
	IsError bool
}

// runShell spawns the command, streams output into windowed sinks, and
// kills the process group on timeout/cap overflow.
func runShell(ctx context.Context, command, workdir string) (runOutcome, error) {
	start := time.Now()

	var name string
	var argv []string
	if runtime.GOOS == "windows" {
		name, argv = "cmd", []string{"/c", command}
	} else {
		name, argv = "/bin/bash", []string{"-c", command}
	}

	cmd := exec.Command(name, argv...)
	cmd.Env = HardenedEnv()
	cmd.Dir = workdir
	if !isWindows() {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
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
	killGroup := func() { killOnce.Do(func() { killProcessGroup(pgid, syscall.SIGKILL) }) }
	stdoutSink := NewOutputSink(StreamHeadLimit, StreamTailLimit)
	stderrSink := NewOutputSink(StreamHeadLimit, StreamTailLimit)

	// Watchdog: ctx done (timeout/abort) → SIGTERM, 2s grace, SIGKILL.
	watch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessGroup(pgid, syscall.SIGTERM)
			timer := time.NewTimer(KillGrace)
			defer timer.Stop()
			select {
			case <-timer.C:
				killProcessGroup(pgid, syscall.SIGKILL)
			case <-watch:
			}
		case <-watch:
		}
	}()

	capWriter := func(sink *OutputSink) io.Writer {
		return &cappedWriter{sink: sink, combined: &combined, cap: CombinedOutputCap, kill: killGroup}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for _, r := range []struct {
		src io.Reader
		w   io.Writer
	}{
		{stdout, capWriter(stdoutSink)},
		{stderr, capWriter(stderrSink)},
	} {
		go func(r io.Reader, w io.Writer) {
			defer wg.Done()
			_, _ = io.Copy(w, r)
		}(r.src, r.w)
	}

	waitErr := cmd.Wait()
	close(watch)
	wg.Wait()

	durationMs := time.Since(start).Milliseconds()
	exitCode, killed := exitStatus(waitErr)

	outText, outTrunc := stdoutSink.Result()
	errText, errTrunc := stderrSink.Result()

	text := outText
	if errText != "" {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "--- stderr ---\n" + errText
	}
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

// cappedWriter counts combined bytes across both streams and fires the
// kill callback once the cap is exceeded; the sink underneath stays
// bounded by its windows regardless.
type cappedWriter struct {
	sink     *OutputSink
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
