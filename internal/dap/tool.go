package dap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// Output bounds: a debugger query must never dump a whole heap graph or a
// megabyte of program output into the model's context.
const (
	MaxThreads     = 50
	MaxFrames      = 30
	MaxVariables   = 100
	MaxValueBytes  = 512
	MaxTextBytes   = 8 << 10
	MaxOutputBytes = 4 << 10

	minTimeout = 5 * time.Second
	maxTimeout = 600 * time.Second
)

// opNames is the accepted op set, in schema order.
const opNames = "launch|attach|status|breakpoints|continue|step_over|step_in|step_out|pause|threads|stack_trace|scopes|variables|evaluate|terminate|custom"

// Tool is the `debug` tool: a DAP client over the adapters in debug.adapters
// (dlv, debugpy, lldb-dap by default), started lazily on the first launch or
// attach. At most one debug session is active per process.
//
// ponytail: the adapter is never detached, so when xdev exits its stdio pipes
// close and the adapter shuts itself down; op=terminate does it eagerly and
// kills the process group if the adapter lingers. Ceiling: an adapter that
// ignores stdin EOF outlives xdev; the upgrade path is a harness exit hook
// next to the MCP/extension Close calls in cmd/xdev.
type Tool struct {
	CWD string

	cfg Config

	// spawn launches an adapter process; nil means the real subprocess
	// launcher. Tests substitute an in-process fake so launch/attach run
	// without a subprocess.
	spawn func(name string, spec AdapterSpec, root string) (*Client, error)

	mu      sync.Mutex
	client  *Client
	adapter string
	program string
	// threadID is the thread of the last stop, used when a request omits one.
	threadID int
}

// NewTool builds the tool from the layered settings (nil = built-in defaults).
func NewTool(cwd string, settings *config.Settings) *Tool {
	return NewToolWithConfig(ConfigFromSettings(settings), cwd)
}

// NewToolWithConfig builds the tool from an explicit configuration.
func NewToolWithConfig(cfg Config, cwd string) *Tool {
	if cfg.Adapters == nil {
		cfg.Adapters = DefaultAdapters()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Tool{CWD: cwd, cfg: cfg}
}

func (t *Tool) Name() string { return "debug" }

func (t *Tool) Description() string {
	return "Debug Adapter Protocol client: launch or attach a program through a configured adapter (dlv, debugpy, lldb-dap), set breakpoints, step, inspect threads/stack/scopes/variables, evaluate expressions. One debug session per process; adapters start lazily and a missing adapter binary is reported, not fatal."
}

func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {"type": "string", "enum": ["launch", "attach", "status", "breakpoints", "continue", "step_over", "step_in", "step_out", "pause", "threads", "stack_trace", "scopes", "variables", "evaluate", "terminate", "custom"], "description": "action to run"},
    "adapter": {"type": "string", "description": "configured adapter id (dlv, debugpy, lldb-dap, or a debug.adapters entry); default: picked from the program/file extension"},
    "program": {"type": "string", "description": "debug target: a binary or a Go package directory (op=launch); also used to pick the adapter"},
    "args": {"type": "array", "items": {"type": "string"}, "description": "program arguments (op=launch)"},
    "mode": {"type": "string", "description": "adapter launch mode (dlv: debug|exec|test, default debug)"},
    "stop_on_entry": {"type": "boolean", "description": "stop at the first line of the program (op=launch)"},
    "pid": {"type": "integer", "description": "process id to attach to (op=attach)"},
    "file": {"type": "string", "description": "source file (op=breakpoints)"},
    "line": {"type": "integer", "description": "1-based breakpoint line (op=breakpoints)"},
    "function": {"type": "string", "description": "function name; op=breakpoints sets a function breakpoint instead of a file/line one"},
    "condition": {"type": "string", "description": "conditional breakpoint expression"},
    "hit_condition": {"type": "string", "description": "hit count condition, e.g. \">= 3\""},
    "clear": {"type": "boolean", "description": "remove the file's / function's breakpoints instead of setting one"},
    "thread_id": {"type": "integer", "description": "thread for continue/step/pause/stack_trace (default: the thread of the last stop, else the first)"},
    "levels": {"type": "integer", "description": "max stack frames (op=stack_trace, default 30)"},
    "frame_id": {"type": "integer", "description": "frame id from op=stack_trace (op=scopes; optional for op=evaluate)"},
    "variable_ref": {"type": "integer", "description": "variables reference from op=scopes or a variable's [ref N] (op=variables)"},
    "expression": {"type": "string", "description": "expression to evaluate (op=evaluate)"},
    "context": {"type": "string", "enum": ["watch", "repl", "hover", "variables", "clipboard"], "description": "evaluate context (op=evaluate, default repl)"},
    "command": {"type": "string", "description": "raw DAP request command (op=custom), e.g. disassemble or readMemory"},
    "arguments": {"type": "object", "description": "raw DAP request arguments (op=custom)"},
    "timeout": {"type": "integer", "description": "per-request timeout seconds (5..600, default 30); also bounds a post-continue stop wait"}
  },
  "required": ["op"]
}`)
}

// Close ends any session this tool owns and stops the adapter process. The
// tool is usable again afterwards (a new launch starts a fresh adapter).
func (t *Tool) Close() {
	t.mu.Lock()
	c := t.client
	t.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

type debugArgs struct {
	Op           string         `json:"op"`
	Adapter      string         `json:"adapter"`
	Program      string         `json:"program"`
	Args         []string       `json:"args"`
	Mode         string         `json:"mode"`
	StopOnEntry  bool           `json:"stop_on_entry"`
	PID          int            `json:"pid"`
	File         string         `json:"file"`
	Line         int            `json:"line"`
	Function     string         `json:"function"`
	Condition    string         `json:"condition"`
	HitCondition string         `json:"hit_condition"`
	Clear        bool           `json:"clear"`
	ThreadID     int            `json:"thread_id"`
	Levels       int            `json:"levels"`
	FrameID      int            `json:"frame_id"`
	VariableRef  int            `json:"variable_ref"`
	Expression   string         `json:"expression"`
	Context      string         `json:"context"`
	Command      string         `json:"command"`
	Arguments    map[string]any `json:"arguments"`
	Timeout      int            `json:"timeout"`
}

func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a debugArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errResult("debug: malformed arguments: " + err.Error()), nil
	}
	timeout := t.cfg.Timeout
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
		if timeout < minTimeout {
			timeout = minTimeout
		}
		if timeout > maxTimeout {
			timeout = maxTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch op := strings.ToLower(strings.TrimSpace(a.Op)); op {
	case "launch":
		return t.launch(ctx, a)
	case "attach":
		return t.attach(ctx, a)
	case "status":
		return t.status(), nil
	case "breakpoints":
		return t.breakpoints(ctx, a)
	case "continue", "pause":
		return t.resume(ctx, a, op)
	case "step_over":
		return t.resume(ctx, a, "next")
	case "step_in":
		return t.resume(ctx, a, "stepIn")
	case "step_out":
		return t.resume(ctx, a, "stepOut")
	case "threads":
		return t.threads(ctx)
	case "stack_trace":
		return t.stackTrace(ctx, a)
	case "scopes":
		return t.scopes(ctx, a)
	case "variables":
		return t.variables(ctx, a)
	case "evaluate":
		return t.evaluate(ctx, a)
	case "terminate":
		return t.terminate(ctx)
	case "custom":
		return t.custom(ctx, a)
	case "":
		return errResult("debug: op is required (" + opNames + ")"), nil
	default:
		return errResult("debug: unknown op " + a.Op + " (want " + opNames + ")"), nil
	}
}

func errResult(msg string) tool.Result {
	return tool.Result{Text: msg, IsError: true}
}

// session returns the live session. A session started by another registry in
// this process is still the process's one session (the guard is process-wide).
func (t *Tool) session() (*Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil && t.client.alive() {
		return t.client, nil
	}
	t.client = nil
	if c, _ := activeSession(); c != nil && c.alive() {
		t.client = c
		return c, nil
	}
	return nil, fmt.Errorf("debug: no active debug session — run op=launch (or op=attach) first (configured adapters: %s)", t.cfg.list())
}

func (t *Tool) setSession(c *Client, adapter, program string) {
	t.mu.Lock()
	t.client, t.adapter, t.program = c, adapter, program
	t.threadID = 0
	t.mu.Unlock()
}

func (t *Tool) lastThread() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.threadID
}

func (t *Tool) setThread(id int) {
	if id <= 0 {
		return
	}
	t.mu.Lock()
	t.threadID = id
	t.mu.Unlock()
}

func (t *Tool) launch(ctx context.Context, a debugArgs) (tool.Result, error) {
	if strings.TrimSpace(a.Program) == "" {
		return errResult("debug: op=launch needs program (the binary or package directory to debug)"), nil
	}
	program := t.absPath(a.Program)
	name, spec, err := t.cfg.AdapterFor(a.Adapter, program)
	if err != nil {
		return errResult(err.Error()), nil
	}
	args := map[string]any{"program": program, "cwd": t.CWD}
	if len(a.Args) > 0 {
		args["args"] = a.Args
	}
	if a.StopOnEntry {
		args["stopOnEntry"] = true
	}
	// Adapter launch quirks: dlv's dap launch requires a mode, and debugpy
	// needs an internal console because xdev implements no runInTerminal.
	if a.Mode != "" {
		args["mode"] = a.Mode
	} else if name == "dlv" {
		args["mode"] = "debug"
	}
	if name == "debugpy" {
		args["console"] = "internalConsole"
	}
	desc := name + " → " + program
	c, err := startSession(ctx, t.spawn, name, spec, t.CWD, desc, "launch", args)
	if err != nil {
		return errResult(err.Error()), nil
	}
	t.setSession(c, name, program)
	return t.started(ctx, c, a.StopOnEntry, "launched "+displayPath(program, t.CWD)+" with "+name)
}

// attach attaches to a running process. The adapter still speaks DAP over
// stdio here and receives the pid.
//
// ponytail: no socket attach (omp connects to dlv/debugpy in listen mode).
// Ceiling: an adapter that only attaches over TCP is unsupported; the upgrade
// path is a dial-based transport when a real need shows up.
func (t *Tool) attach(ctx context.Context, a debugArgs) (tool.Result, error) {
	if a.PID <= 0 {
		return errResult("debug: op=attach needs pid (adapters speak DAP over stdio here; socket attach is not supported)"), nil
	}
	name, spec, err := t.cfg.AdapterFor(a.Adapter, a.Program)
	if err != nil {
		return errResult(err.Error()), nil
	}
	// lldb-dap/gdb read `pid`, debugpy reads `processId`; an adapter ignores
	// the key it does not know.
	args := map[string]any{"cwd": t.CWD, "pid": a.PID, "processId": a.PID}
	desc := fmt.Sprintf("%s → pid %d", name, a.PID)
	c, err := startSession(ctx, t.spawn, name, spec, t.CWD, desc, "attach", args)
	if err != nil {
		return errResult(err.Error()), nil
	}
	t.setSession(c, name, fmt.Sprintf("pid %d", a.PID))
	return t.started(ctx, c, false, fmt.Sprintf("attached to pid %d with %s", a.PID, name))
}

// started finishes a launch/attach: a free-running debuggee is reported and
// returned from immediately; with stop_on_entry the adapter's first stop is
// waited for and reported instead.
func (t *Tool) started(ctx context.Context, c *Client, stopOnEntry bool, headline string) (tool.Result, error) {
	if !stopOnEntry {
		return tool.Result{Text: "debug: " + headline + "; debuggee running — set breakpoints then op=continue, or pass stop_on_entry"}, nil
	}
	ev, out, err := t.waitStop(ctx, c)
	if err != nil {
		return errResult("debug: " + headline + "; waiting for the entry stop: " + err.Error()), nil
	}
	return tool.Result{Text: "debug: " + headline + "\n" + renderStop(ev, out)}, nil
}

func (t *Tool) breakpoints(ctx context.Context, a debugArgs) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	if fn := strings.TrimSpace(a.Function); fn != "" {
		var reqs []map[string]any
		if !a.Clear {
			bp := map[string]any{"name": fn}
			if a.Condition != "" {
				bp["condition"] = a.Condition
			}
			if a.HitCondition != "" {
				bp["hitCondition"] = a.HitCondition
			}
			reqs = append(reqs, bp)
		}
		body, err := c.Call(ctx, "setFunctionBreakpoints", map[string]any{"breakpoints": reqs})
		if err != nil {
			return errResult(err.Error()), nil
		}
		bs, err := decodeList[Breakpoint](body, "breakpoints")
		if err != nil {
			return errResult("debug: setFunctionBreakpoints: " + err.Error()), nil
		}
		return tool.Result{Text: renderBreakpoints(bs, "function "+fn, a.Clear)}, nil
	}
	if strings.TrimSpace(a.File) == "" {
		return errResult("debug: op=breakpoints needs file (or function)"), nil
	}
	if !a.Clear && a.Line <= 0 {
		return errResult("debug: op=breakpoints needs line (or function)"), nil
	}
	path := t.absPath(a.File)
	var reqs []map[string]any
	if !a.Clear {
		bp := map[string]any{"line": a.Line}
		if a.Condition != "" {
			bp["condition"] = a.Condition
		}
		if a.HitCondition != "" {
			bp["hitCondition"] = a.HitCondition
		}
		reqs = append(reqs, bp)
	}
	body, err := c.Call(ctx, "setBreakpoints", map[string]any{
		"source":      map[string]any{"path": path},
		"breakpoints": reqs,
	})
	if err != nil {
		return errResult(err.Error()), nil
	}
	bs, err := decodeList[Breakpoint](body, "breakpoints")
	if err != nil {
		return errResult("debug: setBreakpoints: " + err.Error()), nil
	}
	return tool.Result{Text: renderBreakpoints(bs, displayPath(path, t.CWD), a.Clear)}, nil
}

// resume sends one execution request (continue/next/stepIn/stepOut/pause) and
// reports the stop that follows. A debuggee that runs free past the timeout is
// reported as still running rather than as an error.
func (t *Tool) resume(ctx context.Context, a debugArgs, command string) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	thread, err := t.threadFor(ctx, c, a)
	if err != nil {
		return errResult("debug: " + err.Error()), nil
	}
	if _, err := c.Call(ctx, command, map[string]any{"threadId": thread}); err != nil {
		return errResult(err.Error()), nil
	}
	ev, out, err := t.waitStop(ctx, c)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return tool.Result{Text: fmt.Sprintf("debug: %s sent to thread %d; no stop event within the timeout — the debuggee is still running (op=threads or op=stack_trace to look)", command, thread)}, nil
		}
		return errResult("debug: " + err.Error()), nil
	}
	return tool.Result{Text: "debug: " + command + "\n" + renderStop(ev, out)}, nil
}

// waitStop consumes events until the adapter reports a stop or the debuggee
// exits, collecting the program output emitted on the way. The wait is bounded
// by the call's context: a debuggee that never stops must not hang the tool.
func (t *Tool) waitStop(ctx context.Context, c *Client) (Event, string, error) {
	var out strings.Builder
	for {
		ev, err := c.NextEvent(ctx)
		if err != nil {
			return Event{}, capText(out.String(), MaxOutputBytes), err
		}
		switch ev.Name {
		case "output":
			if out.Len() < 2*MaxOutputBytes {
				var o OutputEvent
				if json.Unmarshal(ev.Body, &o) == nil {
					out.WriteString(o.Output)
				}
			}
		case "stopped":
			var s StoppedEvent
			if json.Unmarshal(ev.Body, &s) == nil {
				t.setThread(s.ThreadID)
			}
			return ev, capText(out.String(), MaxOutputBytes), nil
		case "terminated", "exited":
			return ev, capText(out.String(), MaxOutputBytes), nil
		}
	}
}

func (t *Tool) threads(ctx context.Context) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	ts, err := t.threadList(ctx, c)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return tool.Result{Text: renderThreads(ts)}, nil
}

func (t *Tool) threadList(ctx context.Context, c *Client) ([]Thread, error) {
	body, err := c.Call(ctx, "threads", nil)
	if err != nil {
		return nil, err
	}
	return decodeList[Thread](body, "threads")
}

// threadFor resolves the thread a request applies to: the explicit id, else
// the thread of the last stop, else the debuggee's first thread.
func (t *Tool) threadFor(ctx context.Context, c *Client, a debugArgs) (int, error) {
	if a.ThreadID > 0 {
		return a.ThreadID, nil
	}
	if id := t.lastThread(); id > 0 {
		return id, nil
	}
	ts, err := t.threadList(ctx, c)
	if err != nil {
		return 0, err
	}
	if len(ts) == 0 {
		return 0, errors.New("the debuggee reports no threads (is it still starting?)")
	}
	return ts[0].ID, nil
}

func (t *Tool) stackTrace(ctx context.Context, a debugArgs) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	thread, err := t.threadFor(ctx, c, a)
	if err != nil {
		return errResult("debug: " + err.Error()), nil
	}
	levels := a.Levels
	if levels <= 0 || levels > MaxFrames {
		levels = MaxFrames
	}
	body, err := c.Call(ctx, "stackTrace", map[string]any{"threadId": thread, "startFrame": 0, "levels": levels})
	if err != nil {
		return errResult(err.Error()), nil
	}
	frames, err := decodeList[StackFrame](body, "stackFrames")
	if err != nil {
		return errResult("debug: stackTrace: " + err.Error()), nil
	}
	return tool.Result{Text: renderFrames(frames, t.CWD, totalFrames(body))}, nil
}

// totalFrames reads stackTrace's totalFrames count (0 when the adapter omits
// it, which is legal).
func totalFrames(body json.RawMessage) int {
	if len(body) == 0 {
		return 0
	}
	var m struct {
		Total int `json:"totalFrames"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return 0
	}
	return m.Total
}

func (t *Tool) scopes(ctx context.Context, a debugArgs) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	if a.FrameID <= 0 {
		return errResult("debug: op=scopes needs frame_id (from op=stack_trace)"), nil
	}
	body, err := c.Call(ctx, "scopes", map[string]any{"frameId": a.FrameID})
	if err != nil {
		return errResult(err.Error()), nil
	}
	scopes, err := decodeList[Scope](body, "scopes")
	if err != nil {
		return errResult("debug: scopes: " + err.Error()), nil
	}
	return tool.Result{Text: renderScopes(scopes)}, nil
}

func (t *Tool) variables(ctx context.Context, a debugArgs) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	if a.VariableRef <= 0 {
		return errResult("debug: op=variables needs variable_ref (from op=scopes or a variable's [ref N])"), nil
	}
	// No start/count: the initialize handshake declared no variable paging,
	// so the cap is applied when rendering instead.
	body, err := c.Call(ctx, "variables", map[string]any{"variablesReference": a.VariableRef})
	if err != nil {
		return errResult(err.Error()), nil
	}
	vars, err := decodeList[Variable](body, "variables")
	if err != nil {
		return errResult("debug: variables: " + err.Error()), nil
	}
	return tool.Result{Text: renderVariables(vars, a.VariableRef)}, nil
}

func (t *Tool) evaluate(ctx context.Context, a debugArgs) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	expr := strings.TrimSpace(a.Expression)
	if expr == "" {
		return errResult("debug: op=evaluate needs expression"), nil
	}
	ctxName := a.Context
	if ctxName == "" {
		ctxName = "repl"
	}
	args := map[string]any{"expression": expr, "context": ctxName}
	if a.FrameID > 0 {
		args["frameId"] = a.FrameID
	}
	body, err := c.Call(ctx, "evaluate", args)
	if err != nil {
		return errResult("debug: evaluate " + expr + ": " + err.Error()), nil
	}
	var res EvaluateResult
	if len(body) > 0 {
		if err := json.Unmarshal(body, &res); err != nil {
			return errResult("debug: evaluate: " + err.Error()), nil
		}
	}
	text := fmt.Sprintf("debug: %s = %s", expr, capText(oneLine(res.Result), MaxValueBytes))
	if res.Type != "" {
		text += " (" + res.Type + ")"
	}
	if res.VariablesReference > 0 {
		text += fmt.Sprintf(" [ref %d]", res.VariablesReference)
	}
	return tool.Result{Text: text}, nil
}

// custom is the escape hatch for DAP requests the tool does not model
// (disassemble, readMemory, writeMemory, loadedSources, …).
func (t *Tool) custom(ctx context.Context, a debugArgs) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	command := strings.TrimSpace(a.Command)
	if command == "" {
		return errResult("debug: op=custom needs command (a raw DAP request, e.g. disassemble)"), nil
	}
	body, err := c.Call(ctx, command, a.Arguments)
	if err != nil {
		return errResult(err.Error()), nil
	}
	out := "debug: " + command + " ok"
	if len(body) > 0 {
		var v any
		if json.Unmarshal(body, &v) == nil {
			if b, err := json.MarshalIndent(v, "", "  "); err == nil {
				out += "\n" + string(b)
			}
		}
	}
	return tool.Result{Text: capText(out, MaxTextBytes)}, nil
}

func (t *Tool) terminate(ctx context.Context) (tool.Result, error) {
	c, err := t.session()
	if err != nil {
		return errResult(err.Error()), nil
	}
	// Terminate disconnects and stops the adapter process either way; a
	// worth reporting but is not a failure of the teardown.
	verr := c.Terminate(ctx)
	t.setSession(nil, "", "")
	if verr != nil {
		return tool.Result{Text: "debug: session closed and adapter process stopped; the adapter refused terminate (" + verr.Error() + ")"}, nil
	}
	return tool.Result{Text: "debug: debuggee terminated; adapter process stopped"}, nil
}

func (t *Tool) status() tool.Result {
	var b strings.Builder
	if c, desc := activeSession(); c != nil {
		state := "running"
		if !c.alive() {
			state = "adapter exited"
		}
		fmt.Fprintf(&b, "debug: session active: %s (%s)\n", desc, state)
	} else {
		b.WriteString("debug: no active debug session\n")
	}
	for _, n := range t.cfg.Names() {
		spec := t.cfg.Adapters[n]
		transport := ""
		if spec.Socket {
			transport = " (socket)"
		}
		fmt.Fprintf(&b, "  %s: %s %s%s\n", n, spec.Command, strings.Join(spec.Args, " "), transport)
	}
	return tool.Result{Text: strings.TrimRight(b.String(), "\n")}
}

// absPath resolves a tool-supplied path against the session cwd.
func (t *Tool) absPath(file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(t.CWD, file)
}

func renderThreads(ts []Thread) string {
	if len(ts) == 0 {
		return "debug: no threads"
	}
	shown := ts
	if len(shown) > MaxThreads {
		shown = shown[:MaxThreads]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "debug: %d thread(s)%s\n", len(ts), countNote(len(shown), len(ts)))
	for _, th := range shown {
		fmt.Fprintf(&b, "  #%d %s\n", th.ID, th.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderFrames(frames []StackFrame, cwd string, total int) string {
	if len(frames) == 0 {
		return "debug: no stack frames (is the thread running?)"
	}
	shown := frames
	if len(shown) > MaxFrames {
		shown = shown[:MaxFrames]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "debug: %d frame(s)%s\n", len(frames), countNote(len(shown), total))
	for i, f := range shown {
		loc := f.Source.Path
		if loc == "" {
			loc = f.Source.Name
		}
		loc = displayPath(loc, cwd)
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", loc, f.Line)
		}
		fmt.Fprintf(&b, "  #%d %s at %s (frame %d)\n", i, f.Name, loc, f.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderScopes(scopes []Scope) string {
	if len(scopes) == 0 {
		return "debug: no scopes for that frame"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "debug: %d scope(s)\n", len(scopes))
	for _, s := range scopes {
		fmt.Fprintf(&b, "  %s (ref %d%s)", s.Name, s.VariablesReference, expensiveNote(s.Expensive))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func expensiveNote(expensive bool) string {
	if expensive {
		return ", expensive"
	}
	return ""
}

func renderVariables(vars []Variable, ref int) string {
	if len(vars) == 0 {
		return fmt.Sprintf("debug: variables reference %d is empty", ref)
	}
	shown := vars
	if len(shown) > MaxVariables {
		shown = shown[:MaxVariables]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "debug: %d variable(s) [ref %d]%s\n", len(vars), ref, countNote(len(shown), len(vars)))
	for _, v := range shown {
		fmt.Fprintf(&b, "  %s = %s", v.Name, capText(oneLine(v.Value), MaxValueBytes))
		if v.Type != "" {
			fmt.Fprintf(&b, " (%s)", v.Type)
		}
		if v.VariablesReference > 0 {
			fmt.Fprintf(&b, " [ref %d]", v.VariablesReference)
		}
		b.WriteString("\n")
	}
	// The row and value caps alone can still exceed the payload budget with
	// wide types, so the whole report is capped too.
	return capText(strings.TrimRight(b.String(), "\n"), MaxTextBytes)
}

func renderBreakpoints(bs []Breakpoint, target string, cleared bool) string {
	if len(bs) == 0 {
		if cleared {
			return "debug: breakpoints in " + target + " cleared"
		}
		return "debug: no breakpoints set for " + target
	}
	var b strings.Builder
	fmt.Fprintf(&b, "debug: %d breakpoint(s) for %s\n", len(bs), target)
	for _, bp := range bs {
		state := "unverified"
		if bp.Verified {
			state = "verified"
		}
		fmt.Fprintf(&b, "  #%d %s at line %d", bp.ID, state, bp.Line)
		if bp.Message != "" {
			fmt.Fprintf(&b, ": %s", bp.Message)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderStop renders the event that ended a step/continue: the stop reason
// plus whatever program output arrived while the debuggee ran.
func renderStop(ev Event, output string) string {
	var b strings.Builder
	switch ev.Name {
	case "stopped":
		var s StoppedEvent
		_ = json.Unmarshal(ev.Body, &s)
		reason := s.Reason
		if reason == "" {
			reason = "stopped"
		}
		fmt.Fprintf(&b, "stopped: %s", reason)
		if s.ThreadID > 0 {
			fmt.Fprintf(&b, ", thread %d", s.ThreadID)
		}
		if len(s.HitBreakpointIDs) > 0 {
			fmt.Fprintf(&b, ", breakpoint(s) %v", s.HitBreakpointIDs)
		}
		if s.Description != "" {
			fmt.Fprintf(&b, " — %s", s.Description)
		}
		if s.Text != "" {
			fmt.Fprintf(&b, " %s", s.Text)
		}
	case "terminated", "exited":
		var t TerminatedEvent
		_ = json.Unmarshal(ev.Body, &t)
		if t.ExitCode != nil {
			fmt.Fprintf(&b, "%s: debuggee exited with code %d", ev.Name, *t.ExitCode)
		} else {
			fmt.Fprintf(&b, "%s: debuggee exited", ev.Name)
		}
	default:
		fmt.Fprintf(&b, "%s event", ev.Name)
	}
	if output != "" {
		fmt.Fprintf(&b, "\noutput:\n%s", output)
	}
	return b.String()
}

// countNote says the report is partial when a cap cut it short.
func countNote(shown, total int) string {
	if total > shown {
		return fmt.Sprintf(" (showing %d of %d)", shown, total)
	}
	return ""
}

// capText bounds one text payload; the marker says the report is incomplete
// rather than silently short.
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n… [truncated: %d bytes total]", len(s))
}

// oneLine collapses a multi-line value so one variable cannot blow up a row.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\\n")
}

// displayPath renders path relative to cwd when it is below it.
func displayPath(path, cwd string) string {
	if cwd == "" || path == "" {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
