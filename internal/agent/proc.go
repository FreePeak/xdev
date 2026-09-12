package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strings"

	"github.com/FreePeak/xdev/internal/logx"
	"sync"
	"time"
)

// Named long-running child processes (M11 research §2, issue #37): a hub
// can launch services, tail their output into a capped ring, gate
// readiness on a log regex or TCP port, and stop/restart them by name.
// Stdlib only — no PTY dependency (stdin is a plain pipe, which covers
// every current consumer; a real PTY needs a dependency budget xdev does
// not carry).

// procRingCap bounds the stdout ring buffer (lines) per process.
const procRingCap = 1000

// ProcSpec configures one named process.
type ProcSpec struct {
	Name string
	App  string
	Args []string
	Env  map[string]string
	CWD  string
	// ReadyLog (regex over captured output) and/or ReadyPort (TCP probe);
	// both optional — when neither is set Start returns as soon as the
	// process is running.
	ReadyLog  string
	ReadyPort int
	// ReadyTimeout bounds the readiness wait (default 30s when a ready
	// condition is configured).
	ReadyTimeout time.Duration
}

// ProcInfo is the parent-visible snapshot of one named process.
type ProcInfo struct {
	Name   string `json:"name"`
	App    string `json:"app"`
	Status string `json:"status"` // running | exited | stopped
	// Ready is "" until readiness was observed, then "log" or "port".
	Ready string `json:"ready,omitempty"`
	PID   int    `json:"pid,omitempty"`
	Err   string `json:"err,omitempty"`
}

type proc struct {
	spec  ProcSpec
	mu    sync.Mutex
	info  ProcInfo
	cmd   *exec.Cmd
	stdin io.WriteCloser
	ring  []string // capped output ring, oldest first
}

func (p *proc) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info.Status == "running"
}

func (p *proc) snapshot() ProcInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
}

// ProcTable supervises named long-running processes for one Hub. All
// methods are safe for concurrent use; names are stable handles across
// restart.
type ProcTable struct {
	mu    sync.Mutex
	procs map[string]*proc
}

func NewProcTable() *ProcTable {
	return &ProcTable{procs: map[string]*proc{}}
}

// Start launches the named process and blocks until its readiness
// conditions pass (or immediately when none are configured). A name
// collision with a still-running process is refused; a settled one is
// replaced (restart re-launches by name).
func (t *ProcTable) Start(ctx context.Context, spec ProcSpec) (ProcInfo, error) {
	if spec.Name == "" || spec.App == "" {
		return ProcInfo{}, errors.New("proc: name and app are required")
	}
	if spec.ReadyLog != "" {
		if _, err := regexp.Compile(spec.ReadyLog); err != nil {
			return ProcInfo{}, fmt.Errorf("proc: ready log: %w", err)
		}
	}
	t.mu.Lock()
	if old, ok := t.procs[spec.Name]; ok && old.running() {
		t.mu.Unlock()
		return ProcInfo{}, fmt.Errorf("proc: %q is already running (stop or restart it first)", spec.Name)
	}
	t.mu.Unlock()

	pctx := ctx
	if pctx == nil {
		pctx = context.Background()
	}
	cmd := exec.CommandContext(pctx, spec.App, spec.Args...)
	cmd.Dir = spec.CWD
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Environ(), k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ProcInfo{}, err
	}
	p := &proc{spec: spec, stdin: stdin, info: ProcInfo{Name: spec.Name, App: spec.App, Status: "running"}}
	cmd.Stdout = ringWriter{p}
	cmd.Stderr = ringWriter{p}
	// Own process group: a shell-launched service can then be signalled as
	// a tree (killing the shell alone leaves the real process holding our
	// stdout pipe open and the reaper stalled).
	prepareProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return ProcInfo{}, err
	}
	p.mu.Lock()
	p.cmd = cmd
	p.info.PID = cmd.Process.Pid
	p.mu.Unlock()

	// Reap: Wait is safe to call concurrently with the ring writers (they
	// are plain io.Writers, not pipes the caller owns).
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.info.Status = "exited"
		if err != nil {
			p.info.Err = err.Error()
		}
		p.mu.Unlock()
	}()
	t.mu.Lock()
	t.procs[spec.Name] = p
	t.mu.Unlock()

	// Readiness: wait for the log regex or TCP port, bounded. Process
	// creation alone is not readiness.
	if spec.ReadyLog != "" || spec.ReadyPort > 0 {
		timeout := spec.ReadyTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		// ponytail: a 50ms poll sweep is plain and correct for a handful
		// of supervised processes; an event-driven wait buys nothing here.
		re := regexp.MustCompile(spec.ReadyLog)
		deadline := time.Now().Add(timeout)
		for {
			if info := p.snapshot(); info.Status != "running" {
				return info, fmt.Errorf("proc %s: %s before ready (%s)", spec.Name, info.Status, info.Err)
			}
			if spec.ReadyLog != "" && p.logMatches(re) {
				p.mu.Lock()
				p.info.Ready = "log"
				p.mu.Unlock()
				return p.snapshot(), nil
			}
			if spec.ReadyPort > 0 && probePort(spec.ReadyPort) {
				p.mu.Lock()
				p.info.Ready = "port"
				p.mu.Unlock()
				return p.snapshot(), nil
			}
			if time.Now().After(deadline) {
				return p.snapshot(), fmt.Errorf("proc %s: not ready within %s", spec.Name, timeout)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return p.snapshot(), nil
}

// logMatches reports whether any captured line matches the regex.
func (p *proc) logMatches(re *regexp.Regexp) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ln := range p.ring {
		if re.MatchString(ln) {
			return true
		}
	}
	return false
}

// probePort reports whether the TCP port accepts a connection.
func probePort(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ringWriter splits written bytes into lines and appends them to the
// process's output ring.
type ringWriter struct{ p *proc }

func (w ringWriter) Write(b []byte) (int, error) {
	p := w.p
	p.mu.Lock()
	for _, ln := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if ln == "" {
			continue
		}
		p.ring = append(p.ring, ln)
		if len(p.ring) > procRingCap {
			p.ring = append(p.ring[:0], p.ring[len(p.ring)-procRingCap:]...)
		}
	}
	p.mu.Unlock()
	return len(b), nil
}

// PS lists every process snapshot.
func (t *ProcTable) PS() []ProcInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ProcInfo, 0, len(t.procs))
	for _, p := range t.procs {
		out = append(out, p.snapshot())
	}
	return out
}

// Describe returns one process snapshot.
func (t *ProcTable) Describe(name string) (ProcInfo, bool) {
	t.mu.Lock()
	p, ok := t.procs[name]
	t.mu.Unlock()
	if !ok {
		return ProcInfo{}, false
	}
	return p.snapshot(), true
}

// Logs returns up to the last n lines of the output ring (n <= 0 = all),
// oldest first.
func (t *ProcTable) Logs(name string, n int) ([]string, bool) {
	t.mu.Lock()
	p, ok := t.procs[name]
	t.mu.Unlock()
	if !ok {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := p.ring
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append([]string(nil), lines...), true
}

// StopAll terminates every still-running supervised process (SIGTERM, then
// the escalation Stop performs). xdev has no persist/detach concept, so a
// hub-started process is session-scoped by definition; without this the
// children outlive the run as orphans (parity finding T3 #8). Called by every
// run mode on exit.
func (t *ProcTable) StopAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	names := make([]string, 0, len(t.procs))
	for name, p := range t.procs {
		if p.running() {
			names = append(names, name)
		}
	}
	t.mu.Unlock()
	for _, name := range names {
		if err := t.Stop(name, "SIGTERM"); err != nil {
			logx.Errorf("proc %s: stop on exit: %v", name, err)
		}
	}
}

// Stop terminates a running process: signal "" (or SIGKILL) kills it, while
// SIGTERM/SIGINT ask first and escalate to a kill after a grace period.
// Stopping an already-settled process is a no-op.
func (t *ProcTable) Stop(name, signal string) error {
	t.mu.Lock()
	p, ok := t.procs[name]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("proc: unknown process %q", name)
	}
	if !p.running() {
		return nil
	}
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid
	switch strings.ToUpper(signal) {
	case "SIGTERM":
		killProcessGroup(pgid, procSignalTerm)
		if !waitGone(p, 3*time.Second) {
			killProcessGroup(pgid, procSignalKill)
		}
	case "SIGINT":
		killProcessGroup(pgid, procSignalInt)
		if !waitGone(p, 3*time.Second) {
			killProcessGroup(pgid, procSignalKill)
		}
	default: // "" or SIGKILL
		killProcessGroup(pgid, procSignalKill)
	}
	// ponytail: bounded reap poll — the group kill is near-instant;
	// wait-channel plumbing is not worth it at this call rate.
	if !waitGone(p, 5*time.Second) {
		return fmt.Errorf("proc: %q did not exit within 5s", name)
	}
	p.mu.Lock()
	p.info.Status = "stopped"
	p.mu.Unlock()
	return nil
}

// waitGone polls until the process settles or the timeout elapses; it
// reports whether the process is gone.
func waitGone(p *proc, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for p.running() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// SpecOf returns the launch spec recorded for a process (zero value when
// unknown).
func (t *ProcTable) SpecOf(name string) ProcSpec {
	t.mu.Lock()
	p, ok := t.procs[name]
	t.mu.Unlock()
	if !ok {
		return ProcSpec{}
	}
	return p.spec
}

// Restart stops then relaunches a process under its original spec.
func (t *ProcTable) Restart(ctx context.Context, name string) (ProcInfo, error) {
	t.mu.Lock()
	p, ok := t.procs[name]
	t.mu.Unlock()
	if !ok {
		return ProcInfo{}, fmt.Errorf("proc: unknown process %q", name)
	}
	spec := p.spec
	if err := t.Stop(name, ""); err != nil {
		return ProcInfo{}, err
	}
	return t.Start(ctx, spec)
}

// WriteInput feeds stdin to a running process (PTY-free interactivity).
func (t *ProcTable) WriteInput(name, text string) error {
	t.mu.Lock()
	p, ok := t.procs[name]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("proc: unknown process %q", name)
	}
	p.mu.Lock()
	closed, stdin := p.info.Status != "running", p.stdin
	p.mu.Unlock()
	if closed {
		return fmt.Errorf("proc: %q is not running", name)
	}
	_, err := io.WriteString(stdin, text)
	return err
}
