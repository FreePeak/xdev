// Package eval implements the persistent Python eval kernel exposed by the
// `eval` tool (M13 #47): one long-lived CPython subprocess speaking NDJSON on
// stdin/stdout, with a namespace that survives between cells.
package eval

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

//go:embed runner.py
var runnerSource string

const (
	// DefaultCellTimeout mirrors omp's eval default; 0 disables the cell clock.
	DefaultCellTimeout = 30 * time.Second
	// MaxCellTimeout is the clamp for a user-supplied timeout.
	MaxCellTimeout = 3600 * time.Second
	// KillGrace is the window between the interrupt and the SIGKILL of the
	// kernel's process group.
	KillGrace = tool.KillGrace
	// Per-stream capture windows: 16KB head + 16KB tail.
	streamHeadLimit = tool.StreamHeadLimit
	streamTailLimit = tool.StreamTailLimit
	// maxDisplayBytes caps one display payload (omp MAX_DISPLAY_TEXT_BYTES).
	maxDisplayBytes = 8000
	// maxFrameBytes bounds a single NDJSON frame the reader will accept; the
	// runner chunks long writes well below this.
	maxFrameBytes = 1 << 20
)

// ErrBusy is returned when a cell is already running: the kernel is
// exclusive, one cell at a time.
var ErrBusy = errors.New("eval: kernel busy: a cell is still running")

// ErrClosed is returned once the kernel has been shut down.
var ErrClosed = errors.New("eval: kernel closed")

// Frame is one NDJSON line from the kernel.
type Frame struct {
	Type      string   `json:"type"`
	ID        int64    `json:"id"`
	Text      string   `json:"text,omitempty"`
	Mime      string   `json:"mime,omitempty"`
	Mimes     []string `json:"mimes,omitempty"`
	Data      string   `json:"data,omitempty"`
	Enc       string   `json:"enc,omitempty"`
	Name      string   `json:"name,omitempty"`
	Value     string   `json:"value,omitempty"`
	Traceback string   `json:"traceback,omitempty"`
	Status    string   `json:"status,omitempty"`
	Python    string   `json:"python,omitempty"`
}

type request struct {
	ID   int64  `json:"id"`
	Code string `json:"code,omitempty"`
	Cmd  string `json:"cmd,omitempty"`
}

type displayValue struct {
	mime   string
	data   string
	binary bool
	bytes  int
}

type cell struct {
	id        int64
	code      string
	startedAt time.Time
	out       *tool.OutputSink
	errOut    *tool.OutputSink
	displays  []displayValue
	result    string
	mimes     []string
	errFrame  *Frame
	status    string // "" while running, then "ok" | "error" | "exited"
	done      chan struct{}
}

func newCell(id int64, code string) *cell {
	return &cell{
		id:        id,
		code:      code,
		startedAt: time.Now(),
		out:       tool.NewOutputSink(streamHeadLimit, streamTailLimit),
		errOut:    tool.NewOutputSink(streamHeadLimit, streamTailLimit),
		done:      make(chan struct{}),
	}
}

func (c *cell) addDisplay(fr Frame) {
	if fr.Enc == "base64" {
		c.displays = append(c.displays, displayValue{mime: fr.Mime, binary: true, bytes: base64Len(fr.Data)})
		return
	}
	data := fr.Data
	if len(data) > maxDisplayBytes {
		data = data[:maxDisplayBytes] + "\n[display truncated]"
	}
	c.displays = append(c.displays, displayValue{mime: fr.Mime, data: data})
}

// render is the model-facing text of the cell: stdout, display payloads, the
// trailing-expression value, stderr and the traceback.
func (c *cell) render() string {
	var b strings.Builder
	if text, _ := c.out.Result(); text != "" {
		b.WriteString(text)
	}
	for _, d := range c.displays {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[display %s]\n", d.mime)
		if d.binary {
			fmt.Fprintf(&b, "[binary payload: %d bytes]", d.bytes)
		} else {
			b.WriteString(d.data)
		}
	}
	if c.result != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(c.result)
	}
	if text, _ := c.errOut.Result(); text != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("--- stderr ---\n")
		b.WriteString(text)
	}
	if c.errFrame != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		tb := c.errFrame.Traceback
		if tb == "" {
			tb = c.errFrame.Name + ": " + c.errFrame.Value
		}
		b.WriteString(strings.TrimRight(tb, "\n"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Outcome is the result of one cell.
type Outcome struct {
	ID          int64
	Text        string
	Status      string // "ok" | "error" | "exited"
	TimedOut    bool
	Interrupted bool
	Duration    time.Duration
}

// Kernel is a long-lived python subprocess. One cell runs at a time.
type Kernel struct {
	dir string

	mu      sync.Mutex
	python  string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	procErr *tool.OutputSink
	version string
	active  *cell
	dead    bool
	closed  bool
	nextID  int64
}

// NewKernel returns a kernel whose interpreter runs in dir.
func NewKernel(dir string) *Kernel {
	if dir == "" {
		dir = "."
	}
	// dead means "no live interpreter": a fresh kernel starts one on first use.
	return &Kernel{dir: dir, dead: true}
}

// Version is the kernel's python version ("3.14.7"), known after boot.
func (k *Kernel) Version() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.version
}

// ResolvePython returns the interpreter the kernel will run: $VIRTUAL_ENV's
// python3 when it exists, else python3 from PATH.
func ResolvePython() (string, error) {
	if venv := os.Getenv("VIRTUAL_ENV"); venv != "" {
		if p := filepath.Join(venv, "bin", "python3"); fileExists(p) {
			return p, nil
		}
	}
	p, err := exec.LookPath("python3")
	if err != nil {
		return "", errors.New("eval: python3 not found on PATH (install Python 3.10+, or set VIRTUAL_ENV)")
	}
	return p, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// RunCell executes one cell and returns when the cell finishes, the cell
// clock expires or ctx is cancelled. A timeout interrupts the running cell
// (SIGINT → KeyboardInterrupt) and escalates to SIGKILL of the process group
// after KillGrace, so the kernel can still serve later cells when it responds
// to the interrupt.
func (k *Kernel) RunCell(ctx context.Context, code string, timeout time.Duration) (Outcome, error) {
	c, err := k.begin(code, "")
	if err != nil {
		return Outcome{}, err
	}

	var clock <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		clock = timer.C
	}

	select {
	case <-c.done:
		return k.outcome(c), nil
	case <-clock:
		return k.cancelCell(c, true), nil
	case <-ctx.Done():
		out := k.cancelCell(c, false)
		out.Interrupted = true
		return out, nil
	}
}

// Interrupt cancels the running cell: SIGINT first, SIGKILL of the process
// group after KillGrace. The kernel stays usable when it responds to the
// interrupt. It is a no-op without an active cell.
func (k *Kernel) Interrupt() (Outcome, bool) {
	k.mu.Lock()
	c := k.active
	k.mu.Unlock()
	if c == nil {
		return Outcome{}, false
	}
	return k.cancelCell(c, false), true
}

// Reset wipes the kernel namespace. A dead kernel simply starts fresh on the
// next cell.
func (k *Kernel) Reset(ctx context.Context) error {
	k.mu.Lock()
	dead := k.dead
	k.mu.Unlock()
	if dead {
		return nil
	}
	c, err := k.begin("", "reset")
	if err != nil {
		return err
	}
	timer := time.NewTimer(DefaultCellTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
		return nil
	case <-timer.C:
		k.cancelCell(c, true)
		return errors.New("eval: kernel did not answer a reset request")
	case <-ctx.Done():
		k.cancelCell(c, false)
		return ctx.Err()
	}
}

// Close kills the kernel process group. The embedded runner also exits on
// stdin EOF, so an xdev process that dies without calling Close leaves no
// orphan behind.
func (k *Kernel) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closed = true
	k.stopLocked()
	return nil
}

func (k *Kernel) begin(code, cmd string) (*cell, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return nil, ErrClosed
	}
	if k.active != nil {
		return nil, ErrBusy
	}
	if k.dead {
		if err := k.startLocked(); err != nil {
			return nil, err
		}
	}
	k.nextID++
	c := newCell(k.nextID, code)
	k.active = c
	if err := writeRequest(k.stdin, request{ID: c.id, Code: code, Cmd: cmd}); err != nil {
		k.active = nil
		k.stopLocked()
		return nil, fmt.Errorf("eval: kernel write: %w", err)
	}
	return c, nil
}

func (k *Kernel) startLocked() error {
	python, err := ResolvePython()
	if err != nil {
		return err
	}
	proc := exec.Command(python, "-u", "-c", runnerSource)
	proc.Dir = k.dir
	proc.Env = tool.HardenedEnv()
	prepareProcessGroup(proc)
	stdin, err := proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("eval: kernel stdin: %w", err)
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		return fmt.Errorf("eval: kernel stdout: %w", err)
	}
	errSink := tool.NewOutputSink(streamHeadLimit, streamTailLimit)
	proc.Stderr = errSink
	if err := proc.Start(); err != nil {
		return fmt.Errorf("eval: start %s: %w", python, err)
	}
	k.python, k.cmd, k.stdin, k.procErr = python, proc, stdin, errSink
	k.dead = false
	k.version = ""
	go k.readLoop(stdout)
	go func() { _ = proc.Wait() }()
	return nil
}

// stopLocked kills the kernel group and drops the pipes. The reader goroutine
// observes EOF and marks the kernel dead.
func (k *Kernel) stopLocked() {
	if k.cmd != nil {
		killGroup(k.cmd)
	}
	if k.stdin != nil {
		_ = k.stdin.Close()
		k.stdin = nil
	}
	k.cmd = nil
	k.dead = true
}

// cancelCell interrupts the running cell and escalates to SIGKILL after
// KillGrace. timedOut only affects reporting (a timeout vs an agent abort).
func (k *Kernel) cancelCell(c *cell, timedOut bool) Outcome {
	k.mu.Lock()
	cmd := k.cmd
	k.mu.Unlock()
	if cmd != nil {
		interruptGroup(cmd)
	}
	select {
	case <-c.done:
	case <-time.After(KillGrace):
		k.mu.Lock()
		if k.cmd != nil {
			killGroup(k.cmd)
		}
		k.mu.Unlock()
		select {
		case <-c.done:
		case <-time.After(KillGrace):
		}
	}
	out := k.outcome(c)
	out.TimedOut = timedOut
	return out
}

func (k *Kernel) outcome(c *cell) Outcome {
	status := c.status
	if status == "" {
		status = "exited"
	}
	out := Outcome{
		ID:       c.id,
		Text:     c.render(),
		Status:   status,
		Duration: time.Since(c.startedAt),
	}
	if status == "exited" {
		if text, _ := k.procErrText(); text != "" {
			out.Text = strings.TrimRight(out.Text+"\n--- kernel stderr ---\n"+text, "\n")
		}
	}
	return out
}

func (k *Kernel) procErrText() (string, bool) {
	k.mu.Lock()
	sink := k.procErr
	k.mu.Unlock()
	if sink == nil {
		return "", false
	}
	return sink.Result()
}

func (k *Kernel) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		var fr Frame
		if err := json.Unmarshal(line, &fr); err != nil {
			continue
		}
		k.route(fr)
	}
	if err := scanner.Err(); err != nil {
		k.mu.Lock()
		if k.procErr != nil {
			fmt.Fprintf(k.procErr, "\nkernel protocol error: %v\n", err)
		}
		k.mu.Unlock()
	}
	k.markExited()
}

func (k *Kernel) route(fr Frame) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if fr.Type == "started" && fr.ID == 0 {
		k.version = fr.Python
		return
	}
	c := k.active
	if c == nil || fr.ID != c.id {
		return
	}
	switch fr.Type {
	case "stdout":
		_, _ = c.out.Write([]byte(fr.Text))
	case "stderr":
		_, _ = c.errOut.Write([]byte(fr.Text))
	case "display":
		c.addDisplay(fr)
	case "result":
		c.result, c.mimes = fr.Value, fr.Mimes
	case "error":
		frame := fr
		c.errFrame = &frame
	case "done":
		c.status = fr.Status
		if c.status == "" {
			c.status = "ok"
		}
		k.active = nil
		close(c.done)
	}
}

func (k *Kernel) markExited() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.dead && k.active == nil {
		return
	}
	k.dead = true
	k.cmd = nil
	k.stdin = nil
	if k.active != nil {
		k.active.status = "exited"
		close(k.active.done)
		k.active = nil
	}
}

func writeRequest(w io.Writer, req request) error {
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = w.Write(append(line, '\n'))
	return err
}

// base64Len is the decoded size of a base64 payload.
func base64Len(s string) int {
	pad := 0
	for i := len(s) - 1; i >= 0 && s[i] == '='; i-- {
		pad++
	}
	n := len(s)/4*3 - pad
	if n < 0 {
		return 0
	}
	return n
}
