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

	"github.com/FreePeak/xdev/internal/logx"
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
	// CallID and Args ride a tool_call frame (#268): the kernel's own per-call
	// id (one cell issues many, so it is not a message index) and the cell's
	// arguments as raw JSON, forwarded to the bridge byte for byte.
	CallID int64           `json:"call_id,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// toolResult is one answer to a cell's tool_call frame (#268).
type toolResult struct {
	Text    string `json:"text,omitempty"`
	Details any    `json:"details,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Error   string `json:"error,omitempty"`
}

// pendingCall is one call a cell is blocked on. The slot has to be cancellable:
// a cell interrupted mid-call must raise a Python exception, never hang on a
// host answer that is never coming. cancelCell answers every outstanding call
// with an error instead of waiting for the dispatcher.
type pendingCall struct {
	cellID int64
	once   sync.Once
}

// take claims the slot for one answer. A cancelled call wins; a dispatcher
// that finishes afterwards is told so and writes nothing. The bool is what
// keeps a late answer from reaching a cell that has moved on.
func (p *pendingCall) take() bool {
	ok := false
	p.once.Do(func() { ok = true })
	return ok
}

type request struct {
	ID   int64  `json:"id"`
	Code string `json:"code,omitempty"`
	Cmd  string `json:"cmd,omitempty"`
	// CallID and Result answer a tool_call the cell is blocked on.
	CallID int64       `json:"call_id,omitempty"`
	Result *toolResult `json:"result,omitempty"`
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
	// ctx is cancelled with the cell, so a tool the cell is blocked on stops
	// when the cell stops instead of running to completion in the background.
	ctx      context.Context
	cancel   context.CancelFunc
	out      *tool.OutputSink
	errOut   *tool.OutputSink
	displays []displayValue
	result   string
	mimes    []string
	errFrame *Frame
	status   string // "" while running, then "ok" | "error" | "exited"
	done     chan struct{}
}

func newCell(id int64, code string) *cell {
	ctx, cancel := context.WithCancel(context.Background())
	return &cell{
		id:        id,
		code:      code,
		startedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
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
	// pending holds the calls the running cell is blocked on, keyed by the
	// kernel's call id (#268). route reads it while a cell runs and the
	// dispatcher writes answers back, so the map is the one piece of shared
	// state that lives outside k.mu.
	pending map[int64]*pendingCall
	// runner executes a tool for a cell on its own goroutine. nil = a cell
	// calling tools gets the "no runner installed" error, the same refusal
	// tool.Catalog makes: a bridge that executed tools itself would be a
	// policy bypass.
	runner tool.Runner
}

// SetRunner installs the harness call path for cells (#268). It is tool.Runner
// itself — the deferred-tool bridge's type — because the two bridges are the
// same contract: a callback, never direct execution, so the call takes the
// plan-mode gate, the approval policy and the hook chain. Without one the
// kernel refuses to run anything.
func (k *Kernel) SetRunner(r tool.Runner) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.runner = r
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
	if c := k.active; c != nil {
		c.cancel()
	}
}

// cancelCell interrupts the running cell and escalates to SIGKILL after
// KillGrace. timedOut only affects reporting (a timeout vs an agent abort).
func (k *Kernel) cancelCell(c *cell, timedOut bool) Outcome {
	k.mu.Lock()
	cmd := k.cmd
	k.mu.Unlock()
	c.cancel()
	if cmd != nil {
		interruptGroup(cmd)
	}
	// Every tool call the cell is blocked on gets its answer before the
	// interrupt lands: the cell raises KeyboardInterrupt on the next poll, and
	// a dispatcher that answers later hits once and drops its result (#268).
	k.failPending(c.id, "the cell was interrupted while waiting for this tool")
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
	if fr.Type == "started" && fr.ID == 0 {
		k.mu.Lock()
		k.version = fr.Python
		k.mu.Unlock()
		return
	}
	// A tool_call is the one frame route must NOT answer under k.mu: the
	// answer is a tool run, which re-enters the agent loop (hooks, approval,
	// persistence, other locks). Dispatching it from here would invert the
	// lock order with everything the loop holds. So route hands it to a
	// goroutine and returns, releasing k.mu first (#268).
	if fr.Type == "tool_call" {
		k.dispatchToolCall(fr)
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
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
	if k.dead && k.active == nil {
		k.mu.Unlock()
		return
	}
	k.dead = true
	k.cmd = nil
	k.stdin = nil
	c := k.active
	if c != nil {
		c.status = "exited"
		close(c.done)
		k.active = nil
	}
	k.mu.Unlock()
	// A kernel that died with the cell blocked on a host answer must not leave
	// the cell waiting for it. No write is possible (stdin is gone) — taking
	// the slot is the whole answer: the cell's next poll raises KeyboardInterrupt
	// or its NameError, never a silent hang.
	if c != nil {
		k.failPending(c.id, "the eval kernel exited while the cell waited for this tool")
	}
}

// dispatchToolCall answers one cell's tools.x({...}) call (#268). It runs the
// tool on its own goroutine and writes the answer back on stdin; the kernel
// lock is never held across either step.
//
// The three refusals are the same ones tool.Catalog makes, for the same reason
// (docs/decisions/programmatic-tool-calling.md §0.5): a refusal is the
// registry not having the name, where a counter is a number someone will raise.
func (k *Kernel) dispatchToolCall(fr Frame) {
	k.mu.Lock()
	c := k.active
	if k.closed || c == nil || c.id != fr.ID {
		k.mu.Unlock()
		return
	}
	run, ctx := k.runner, c.ctx
	if k.pending == nil {
		k.pending = map[int64]*pendingCall{}
	}
	p := &pendingCall{cellID: fr.ID}
	k.pending[fr.CallID] = p
	k.mu.Unlock()

	// No runner, or the bridge tool itself: refuse here rather than after the
	// cell has waited. eval is the one name that must never come back through
	// the bridge (a cell that spawns a cell is an unbounded recursion), and
	// tool_search/tool_describe/tool_call are refused in the catalog for the
	// same reason.
	if run == nil {
		k.reply(fr.CallID, p, toolResult{Error: "no runner installed — the harness must wire the kernel before a cell can call tools"})
		return
	}
	if fr.Name == "eval" || tool.IsBridgeTool(fr.Name) {
		k.reply(fr.CallID, p, toolResult{Error: fmt.Sprintf("%s cannot be called from a cell", fr.Name)})
		return
	}

	go func() {
		res, err := run(ctx, fr.Name, fr.Args)
		out := toolResult{Text: res.Text, Details: res.Details, IsError: res.IsError}
		if err != nil {
			out.Error = err.Error()
		}
		k.reply(fr.CallID, p, out)
	}()
}

// reply writes one answer back to the cell and drops the slot. The write takes
// k.mu, the same lock begin() writes a request under, because the kernel's
// stdin is one line-oriented stream: two writers would interleave halves of a
// frame. It is a short write of an already-computed value — no tool ever runs
// here.
func (k *Kernel) reply(callID int64, p *pendingCall, res toolResult) {
	if !p.take() {
		return // already answered by a cancel: a late answer is not delivered
	}
	k.mu.Lock()
	w, dead := k.stdin, k.dead
	if w != nil && !dead {
		if err := writeRequest(w, request{Cmd: "tool_result", CallID: callID, Result: &res}); err != nil {
			k.dead = true
			logx.Errorf("eval: kernel write failed: %v", err)
		}
	}
	delete(k.pending, callID)
	k.mu.Unlock()
}

// failPending answers every call the given cell is blocked on, so an
// interrupted cell raises instead of waiting on a host that will not answer.
func (k *Kernel) failPending(cellID int64, why string) {
	k.mu.Lock()
	var live []int64
	var slots []*pendingCall
	for id, p := range k.pending {
		if p.cellID == cellID {
			live = append(live, id)
			slots = append(slots, p)
		}
	}
	k.mu.Unlock()
	errRes := toolResult{Error: why}
	for i, id := range live {
		k.reply(id, slots[i], errRes)
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
