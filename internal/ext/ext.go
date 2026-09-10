// Package ext implements xdev's extension surface as PROCESSES, not code
// (M7 #8, PRD §1.5): an extension is any executable speaking JSONL on
// stdio. The handshake announces capabilities (tools, commands, event
// subscriptions, declarative renderers); the host sends event frames that
// may block, revise, or patch, and extensions may request runtime actions.
// Every event carries a timeout and a SIGKILL, so a hung extension is
// killable — the one thing an in-process plugin VM cannot offer.
package ext

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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/tool"
)

// ProtocolVersion is the ext wire version.
const ProtocolVersion = 1

// DefaultEventTimeout bounds one event round-trip before SIGKILL.
const DefaultEventTimeout = 5 * time.Second

// DefaultHandshakeTimeout bounds the hello → capabilities exchange. It is
// separate from the event budget because process startup (interpreter
// boot, imports) is slow in a way that policy events must not be: tests
// and tight event budgets must not accidentally kill handshakes.
const DefaultHandshakeTimeout = 10 * time.Second

// Event names an extension may subscribe to.
const (
	EventSessionStart = "session_start"
	EventTurnEnd      = "turn_end"
	EventToolCall     = "tool_call"
	EventToolResult   = "tool_result"
)

// Frame is the shared envelope: host→ext events, ext→host responses,
// actions, and logs.
type Frame struct {
	Type    string          `json:"type"` // hello | capabilities | event | response | action | log
	ID      string          `json:"id,omitempty"`
	Event   string          `json:"event,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`

	// hello
	Protocol    int    `json:"protocol,omitempty"`
	XDevVersion string `json:"xdevVersion,omitempty"`

	// response
	Allow    bool            `json:"allow,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Revise   json.RawMessage `json:"revise,omitempty"` // replacement tool arguments
	Patch    json.RawMessage `json:"patch,omitempty"`  // replacement tool result
	Error    string          `json:"error,omitempty"`
	FailOpen bool            `json:"failOpen,omitempty"` // tolerate policy failures

	// action
	Action string          `json:"action,omitempty"` // steer | followUp | aside | register_provider
	Text   string          `json:"text,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`

	// log
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`
}

// Capabilities is the handshake answer.
type Capabilities struct {
	Tools     []ToolDef    `json:"tools,omitempty"`
	Commands  []CommandDef `json:"commands,omitempty"`
	Events    []string     `json:"events,omitempty"`
	Renderers []Renderer   `json:"renderers,omitempty"`
	// FailOpen opts into allowing tool calls when this extension cannot
	// answer. Default is fail-CLOSED: a dead policy extension denies the
	// call, which is the only safe reading of a security hook.
	FailOpen bool `json:"failOpen,omitempty"`
}

// ToolDef is one tool announced by an extension.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// CommandDef is one slash command announced by an extension.
type CommandDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Renderer is a DECLARATIVE render spec (card | table | tree). Custom
// in-process renderers are an accepted capability loss vs omp's TS
// extensions (PRD §IV.8): specs degrade gracefully.
type Renderer struct {
	Tool string          `json:"tool"`
	Kind string          `json:"kind"` // card | table | tree
	Spec json.RawMessage `json:"spec,omitempty"`
}

// Action is a runtime request from an extension to the host.
type Action struct {
	Action string          `json:"action"`
	Text   string          `json:"text,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Extension is one live child process.
type Extension struct {
	Name string
	Path string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	mu    sync.Mutex
	caps  Capabilities
	seq   int
	dead  bool
	host  func(Action)
	owner *Manager

	// sendMu spans a whole write → correlated-reply round-trip. stdout is
	// a single bufio.Reader and the agent dispatches up to MaxToolWorkers
	// tool calls concurrently, so without it two goroutines would race on
	// the same reader: torn frames, replies matched to the wrong waiter,
	// and a healthy extension SIGKILLed for a host-caused race.
	// ponytail: policy checks on one extension serialize (ordering is
	// arguably desirable); the upgrade path is one dedicated reader
	// goroutine dispatching to a map of waiters keyed by reply id.
	sendMu sync.Mutex

	timeout time.Duration
}

// ErrDead marks an extension that can no longer answer (killed, crashed,
// or timed out). Callers prune it from the chain rather than treating it
// as a policy decision.
var ErrDead = errors.New("ext: dead")

// Manager owns the running extensions.
type Manager struct {
	mu   sync.Mutex
	exts []*Extension
	host func(Action)
	// EventTimeout overrides DefaultEventTimeout.
	EventTimeout time.Duration
}

// NewManager returns an empty manager.
func NewManager() *Manager { return &Manager{} }

// BindHost wires the runtime-action callback (steer/followUp/aside).
func (m *Manager) BindHost(h func(Action)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.host = h
	for _, x := range m.exts {
		x.mu.Lock()
		x.host = h
		x.mu.Unlock()
	}
}

// Load launches every executable in dir (non-recursive) and handshakes
// with each. A failing extension is logged and skipped: a broken extension
// must never block a session.
func (m *Manager) Load(ctx context.Context, dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no extensions directory: nothing to do
		}
		return err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		if !isExecutable(path) {
			logx.Debugf("ext: skipping %s (not executable)", name)
			continue
		}
		x, err := m.start(ctx, name, path)
		if err != nil {
			logx.Errorf("ext: %s: %v", name, err)
			continue
		}
		m.mu.Lock()
		m.exts = append(m.exts, x)
		m.mu.Unlock()
		logx.Debugf("ext: loaded %s (events=%v tools=%d)", name, x.caps.Events, len(x.caps.Tools))
	}
	return nil
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	return st.Mode().Perm()&0o111 != 0
}

// start launches one extension and performs the hello → capabilities
// handshake under the event timeout.
func (m *Manager) start(ctx context.Context, name, path string) (*Extension, error) {
	timeout := m.EventTimeout
	if timeout <= 0 {
		timeout = DefaultEventTimeout
	}
	cmd := exec.Command(path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // extensions log to the terminal
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	x := &Extension{
		Name: name, Path: path, cmd: cmd, stdin: stdin,
		stdout: bufio.NewReader(stdout), timeout: timeout, owner: m,
	}
	if err := x.write(Frame{Type: "hello", Protocol: ProtocolVersion}); err != nil {
		x.kill()
		return nil, err
	}
	reply, err := x.await(ctx, "capabilities", handshakeTimeout(timeout))
	if err != nil {
		x.kill()
		return nil, err
	}
	var caps Capabilities
	if err := json.Unmarshal(reply.Payload, &caps); err != nil {
		x.kill()
		return nil, fmt.Errorf("ext: %s bad capabilities: %w", name, err)
	}
	x.mu.Lock()
	x.caps = caps
	x.host = m.host
	x.mu.Unlock()
	return x, nil
}

// handshakeTimeout gives startup at least the default budget, even when
// the caller runs a tight event timeout.
func handshakeTimeout(event time.Duration) time.Duration {
	if event >= DefaultHandshakeTimeout {
		return event
	}
	return DefaultHandshakeTimeout
}

// await reads frames until one of wantType arrives, forwarding actions
// and logs on the way. Bounded by timeout.
func (x *Extension) await(ctx context.Context, wantType string, timeout time.Duration) (Frame, error) {
	type result struct {
		f   Frame
		err error
	}
	done := make(chan result, 1)
	go func() {
		for {
			f, err := x.readLine()
			if err != nil {
				done <- result{err: err}
				return
			}
			switch f.Type {
			case wantType:
				done <- result{f: f}
				return
			case "action":
				x.forward(Action{Action: f.Action, Text: f.Text, Data: f.Data})
			case "log":
				logx.Debugf("ext[%s] %s: %s", x.Name, f.Level, f.Message)
			default:
				done <- result{err: fmt.Errorf("ext: expected %s, got %q", wantType, f.Type)}
				return
			}
		}
	}()
	select {
	case r := <-done:
		return r.f, r.err
	case <-time.After(timeout):
		return Frame{}, fmt.Errorf("ext: %s timed out after %s", x.Name, timeout)
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

// readLine decodes one JSONL frame from the child.
func (x *Extension) readLine() (Frame, error) {
	line, err := x.stdout.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}
		return Frame{}, err
	}
	var f Frame
	if uerr := json.Unmarshal(bytesTrim(line), &f); uerr != nil {
		return Frame{}, fmt.Errorf("ext: %s bad frame %q: %w", x.Name, truncate(string(line), 120), uerr)
	}
	return f, nil
}

func bytesTrim(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func (x *Extension) forward(a Action) {
	x.mu.Lock()
	h := x.host
	x.mu.Unlock()
	if h != nil {
		h(a)
	}
}

func (x *Extension) write(f Frame) error {
	enc, err := json.Marshal(f)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.dead {
		return errors.New("ext: dead")
	}
	_, err = x.stdin.Write(append(enc, '\n'))
	return err
}

// sendEvent delivers one event frame and waits for its correlated reply,
// killing the child on timeout. Returns the reply or an error.
func (x *Extension) sendEvent(ctx context.Context, event string, payload any) (Frame, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Frame{}, err
	}
	x.sendMu.Lock() // one round-trip at a time: the reader is shared
	defer x.sendMu.Unlock()
	x.mu.Lock()
	if x.dead {
		x.mu.Unlock()
		return Frame{}, ErrDead
	}
	x.seq++
	id := fmt.Sprintf("%s-%d", x.Name, x.seq)
	err = x.writeLocked(Frame{Type: "event", ID: id, Event: event, Payload: raw})
	x.mu.Unlock()
	if err != nil {
		x.markDead()
		return Frame{}, err
	}

	type result struct {
		f   Frame
		err error
	}
	done := make(chan result, 1)
	go func() {
		for {
			f, err := x.readLine()
			if err != nil {
				done <- result{err: err}
				return
			}
			if f.Type == "action" {
				x.forward(Action{Action: f.Action, Text: f.Text, Data: f.Data})
				continue
			}
			if f.Type == "log" {
				logx.Debugf("ext[%s] %s: %s", x.Name, f.Level, f.Message)
				continue
			}
			if f.Type != "response" {
				done <- result{err: fmt.Errorf("ext: expected response, got %q", f.Type)}
				return
			}
			if f.ID != id {
				// Stale reply from a previously killed event: drop it and
				// keep waiting for ours.
				continue
			}
			done <- result{f: f}
			return
		}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			x.markDead()
			return Frame{}, r.err
		}
		if r.f.Error != "" {
			// A well-formed response carrying `error` is the extension
			// ANSWERING (declining this one call), not a broken boundary:
			// honor it for this call and stay subscribed.
			return r.f, fmt.Errorf("ext: %s: %s", x.Name, r.f.Error)
		}
		return r.f, nil
	case <-time.After(x.timeout):
		// Per-event timeout + SIGKILL: the boundary's whole promise.
		x.markDead()
		return Frame{}, fmt.Errorf("ext: %s timed out after %s (killed)", x.Name, x.timeout)
	case <-ctx.Done():
		x.markDead()
		return Frame{}, ctx.Err()
	}
}

// writeLocked encodes one frame; callers hold x.mu.
func (x *Extension) writeLocked(f Frame) error {
	enc, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if x.dead {
		return errors.New("ext: dead")
	}
	_, err = x.stdin.Write(append(enc, '\n'))
	return err
}

func (x *Extension) markDead() {
	x.mu.Lock()
	already := x.dead
	x.dead = true
	x.mu.Unlock()
	if !already && x.owner != nil {
		// A dead extension leaves the routing chain: otherwise one
		// transient timeout would deny every tool call for the rest of
		// the session (fail-closed is per-call, not a one-way door).
		x.owner.retire(x)
	}
	x.kill()
}

// kill terminates the child: SIGTERM, then SIGKILL.
func (x *Extension) kill() {
	x.mu.Lock()
	proc := x.cmd.Process
	x.mu.Unlock()
	if proc == nil {
		return
	}
	go func() {
		_ = proc.Signal(syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		_ = proc.Kill() // SIGKILL: unrecoverable hangs die here
		_ = x.cmd.Wait()
	}()
}

// subscribed reports whether the extension listens for an event.
func (x *Extension) subscribed(event string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, e := range x.caps.Events {
		if e == event {
			return true
		}
	}
	return false
}

func (x *Extension) failOpen() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.caps.FailOpen
}

// Capabilities returns the announced capability set.
func (x *Extension) Capabilities() Capabilities {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.caps
}

// Close shuts every extension down.
func (m *Manager) Close() {
	m.mu.Lock()
	exts := m.exts
	m.exts = nil
	m.mu.Unlock()
	for _, x := range exts {
		_ = x.stdin.Close()
		x.kill()
	}
}

// Emit fires a fire-and-forget event (session_start, turn_end). Failures
// are logged only: non-policy events must never stall the loop.
func (m *Manager) Emit(ctx context.Context, event string, payload any) {
	for _, x := range m.list() {
		if !x.subscribed(event) {
			continue
		}
		if _, err := x.sendEvent(ctx, event, payload); err != nil {
			logx.Errorf("ext: %s %s: %v", x.Name, event, err)
		}
	}
}

// ToolCall runs the fail-closed policy chain over one call: each
// subscribed extension may allow, block, or revise the arguments. A
// timeout/crash DENIES the call unless that extension opted into failOpen.
// Returns the final arguments or a blocking error.
func (m *Manager) ToolCall(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	current := args
	for _, x := range m.list() {
		if !x.subscribed(EventToolCall) {
			continue
		}
		reply, err := x.sendEvent(ctx, EventToolCall, map[string]any{"tool": name, "arguments": current})
		if err != nil {
			if x.failOpen() {
				logx.Errorf("ext: %s tool_call failed (fail-open): %v", x.Name, err)
				continue
			}
			return nil, fmt.Errorf("blocked by extension %s: %w", x.Name, err)
		}
		if !reply.Allow {
			reason := reply.Reason
			if reason == "" {
				reason = "denied"
			}
			return nil, fmt.Errorf("blocked by extension %s: %s", x.Name, reason)
		}
		if len(reply.Revise) > 0 {
			current = reply.Revise
		}
	}
	return current, nil
}

// ToolResult lets extensions patch a result. The last patch wins;
// failures never block (the tool already ran).
func (m *Manager) ToolResult(ctx context.Context, name string, args, result json.RawMessage) json.RawMessage {
	out := result
	for _, x := range m.list() {
		if !x.subscribed(EventToolResult) {
			continue
		}
		reply, err := x.sendEvent(ctx, EventToolResult, map[string]any{"tool": name, "arguments": args, "result": out})
		if err != nil {
			logx.Errorf("ext: %s tool_result failed: %v", x.Name, err)
			continue
		}
		if len(reply.Patch) > 0 {
			out = reply.Patch
		}
	}
	return out
}

// Tools returns extension-registered tools as xdev tools.
func (m *Manager) Tools() []tool.Tool {
	var out []tool.Tool
	for _, x := range m.list() {
		x.mu.Lock()
		tools := append([]ToolDef(nil), x.caps.Tools...)
		x.mu.Unlock()
		for _, td := range tools {
			out = append(out, &extTool{x: x, def: td})
		}
	}
	return out
}

// RunCommand invokes one extension slash command ("/ext:cmd args"). The
// extension answers with a patch payload whose text is what the host
// should print. Names are "extension:command" as announced.
func (m *Manager) RunCommand(ctx context.Context, qualified, args string) (string, error) {
	server, cmd, found := strings.Cut(qualified, ":")
	if !found {
		return "", fmt.Errorf("ext: command %q is not extension-qualified", qualified)
	}
	for _, x := range m.list() {
		if x.Name != server {
			continue
		}
		reply, err := x.sendEvent(ctx, "command", map[string]any{"command": cmd, "arguments": args})
		if err != nil {
			return "", err
		}
		var payload struct {
			Text string `json:"text"`
		}
		if len(reply.Patch) > 0 {
			_ = json.Unmarshal(reply.Patch, &payload)
		}
		if payload.Text == "" {
			return "", fmt.Errorf("ext: %s returned no output for %s", qualified, cmd)
		}
		return payload.Text, nil
	}
	return "", fmt.Errorf("ext: no extension named %q", server)
}

// Commands returns extension-announced slash commands (extension:name).
func (m *Manager) Commands() map[string]string {
	out := map[string]string{}
	for _, x := range m.list() {
		x.mu.Lock()
		cmds := append([]CommandDef(nil), x.caps.Commands...)
		x.mu.Unlock()
		for _, c := range cmds {
			out[x.Name+":"+c.Name] = c.Description
		}
	}
	return out
}

// Renderers returns declarative render specs keyed by tool name.
func (m *Manager) Renderers() map[string]Renderer {
	out := map[string]Renderer{}
	for _, x := range m.list() {
		x.mu.Lock()
		rs := append([]Renderer(nil), x.caps.Renderers...)
		x.mu.Unlock()
		for _, r := range rs {
			out["ext_"+x.Name+"_"+r.Tool] = r
		}
	}
	return out
}

// Register adds every extension tool to a registry.
func Register(reg *tool.Registry, tools []tool.Tool) {
	for _, t := range tools {
		reg.Register(t)
	}
}

// retire drops a dead extension from the routing chain (idempotent).
func (m *Manager) retire(x *Extension) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.exts {
		if e == x {
			m.exts = append(m.exts[:i], m.exts[i+1:]...)
			logx.Errorf("ext: %s retired (unavailable for the rest of the session)", x.Name)
			return
		}
	}
}

func (m *Manager) list() []*Extension {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Extension(nil), m.exts...)
}

// extTool adapts an extension tool to tool.Tool.
type extTool struct {
	x   *Extension
	def ToolDef
}

func (e *extTool) Name() string { return "ext_" + e.x.Name + "_" + e.def.Name }

func (e *extTool) Description() string {
	if e.def.Description == "" {
		return "extension tool " + e.def.Name + " from " + e.x.Name
	}
	return e.def.Description
}

func (e *extTool) Parameters() json.RawMessage {
	if len(e.def.Parameters) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return e.def.Parameters
}

// Execute invokes the tool through the extension boundary; the reply's
// patch payload carries the result.
func (e *extTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	reply, err := e.x.sendEvent(ctx, "tool_invoke", map[string]any{"tool": e.def.Name, "arguments": args})
	if err != nil {
		return tool.Result{Text: "ext tool: " + err.Error(), IsError: true}, nil
	}
	var payload struct {
		Text    string `json:"text"`
		IsError bool   `json:"isError"`
	}
	if len(reply.Patch) > 0 {
		_ = json.Unmarshal(reply.Patch, &payload)
	}
	if payload.Text == "" {
		payload.Text = "(extension tool returned no text)"
	}
	return tool.Result{Text: payload.Text, IsError: payload.IsError, Details: map[string]any{"extension": e.x.Name}}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ = fmt.Sprint // keep fmt for Frame/ID helpers
