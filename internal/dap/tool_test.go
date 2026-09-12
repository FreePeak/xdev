package dap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// newFakeTool builds a tool whose adapter is an in-process fake, so launch,
// attach and the ops run end to end without a subprocess. Tests override
// single commands with f.setRespond(withOverride(defaultRespond(f), …)).
func newFakeTool(t *testing.T) (*Tool, *fakeAdapter) {
	return newFakeToolWith(t, map[string]AdapterSpec{"fake": {Command: "fake", Languages: []string{"go"}}})
}

func newFakeToolWith(t *testing.T, adapters map[string]AdapterSpec) (*Tool, *fakeAdapter) {
	t.Helper()
	f := newFakeAdapter(t, nil)
	f.setRespond(defaultRespond(f))
	tool := NewToolWithConfig(Config{Adapters: adapters, Timeout: 5 * time.Second}, "/proj")
	tool.spawn = f.spawn
	t.Cleanup(tool.Close)
	return tool, f
}

// startFake opens the in-process session the way a caller would: launch with
// stop_on_entry, so the fake's entry stop is consumed here and a later
// continue/step waits on its own stop event alone.
func startFake(t *testing.T, tool *Tool) {
	t.Helper()
	if res := mustExec(t, tool, `{"op":"launch","adapter":"fake","program":"/proj/main.go","stop_on_entry":true}`); res.IsError {
		t.Fatalf("fake launch failed: %+v", res)
	}
}

func mustExec(t *testing.T, tool *Tool, args string) tool.Result {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return res
}

func TestDebugFlowFromLaunchToInspection(t *testing.T) {
	tool, f := newFakeTool(t)

	res := mustExec(t, tool, `{"op":"launch","adapter":"fake","program":"/proj/main.go","stop_on_entry":true}`)
	if res.IsError || !strings.Contains(res.Text, "launched main.go with fake") {
		t.Fatalf("launch = %+v", res)
	}
	if !strings.Contains(res.Text, "stopped: entry, thread 1") {
		t.Fatalf("launch did not report the entry stop: %q", res.Text)
	}
	// The launch request must carry the resolved program and the stop flag.
	var launchReq struct {
		Program     string `json:"program"`
		StopOnEntry bool   `json:"stopOnEntry"`
	}
	if err := json.Unmarshal(f.lastArgs("launch"), &launchReq); err != nil {
		t.Fatal(err)
	}
	if launchReq.Program != "/proj/main.go" || !launchReq.StopOnEntry {
		t.Fatalf("launch args = %+v, want the program and stopOnEntry", launchReq)
	}
	if f.spawnCount() != 1 {
		t.Fatalf("adapter spawned %d times, want 1", f.spawnCount())
	}

	res = mustExec(t, tool, `{"op":"breakpoints","file":"main.go","line":12,"condition":"x > 1","hit_condition":">= 2"}`)
	if res.IsError || !strings.Contains(res.Text, "#1 verified at line 12") {
		t.Fatalf("breakpoints = %+v", res)
	}
	var bpReq struct {
		Source struct {
			Path string `json:"path"`
		} `json:"source"`
		Breakpoints []struct {
			Line         int    `json:"line"`
			Condition    string `json:"condition"`
			HitCondition string `json:"hitCondition"`
		} `json:"breakpoints"`
	}
	if err := json.Unmarshal(f.lastArgs("setBreakpoints"), &bpReq); err != nil {
		t.Fatal(err)
	}
	if bpReq.Source.Path != "/proj/main.go" || len(bpReq.Breakpoints) != 1 {
		t.Fatalf("setBreakpoints args = %+v", bpReq)
	}
	if bp := bpReq.Breakpoints[0]; bp.Line != 12 || bp.Condition != "x > 1" || bp.HitCondition != ">= 2" {
		t.Fatalf("breakpoint = %+v, want line/condition/hitCondition passed through", bp)
	}

	res = mustExec(t, tool, `{"op":"continue"}`)
	if res.IsError || !strings.Contains(res.Text, "stopped: breakpoint, thread 1, breakpoint(s) [1]") {
		t.Fatalf("continue = %+v", res)
	}

	res = mustExec(t, tool, `{"op":"stack_trace"}`)
	if res.IsError || !strings.Contains(res.Text, "#0 main.main at main.go:12 (frame 7)") {
		t.Fatalf("stack_trace = %+v", res)
	}

	res = mustExec(t, tool, `{"op":"scopes","frame_id":7}`)
	if res.IsError || !strings.Contains(res.Text, "Locals (ref 2)") || !strings.Contains(res.Text, "Globals (ref 3, expensive)") {
		t.Fatalf("scopes = %+v", res)
	}

	res = mustExec(t, tool, `{"op":"variables","variable_ref":2}`)
	if res.IsError || !strings.Contains(res.Text, "x = 42 (int)") || !strings.Contains(res.Text, "s = hello (string)") {
		t.Fatalf("variables = %+v", res)
	}

	res = mustExec(t, tool, `{"op":"evaluate","expression":"x","frame_id":7}`)
	if res.IsError || !strings.Contains(res.Text, "x = 42 (int)") {
		t.Fatalf("evaluate = %+v", res)
	}

	res = mustExec(t, tool, `{"op":"step_over"}`)
	if res.IsError || !strings.Contains(res.Text, "stopped: step, thread 1") {
		t.Fatalf("step_over = %+v", res)
	}
	res = mustExec(t, tool, `{"op":"threads"}`)
	if res.IsError || !strings.Contains(res.Text, "#1 main") {
		t.Fatalf("threads = %+v", res)
	}

	// The handshake order matters: configuration requests (breakpoints) come
	// after initialize + configurationDone, and the stepping ops must not
	// re-query threads once a stop has named the thread.
	want := []string{"initialize", "launch", "configurationDone", "setBreakpoints", "continue", "stackTrace", "scopes", "variables", "evaluate", "next", "threads"}
	if got := f.commands(); !equalStrings(got, want) {
		t.Fatalf("DAP commands = %v, want %v", got, want)
	}
	if got := len(f.commands()); got != len(want) {
		t.Fatalf("unexpected request count %d", got)
	}
}

func TestLaunchWithoutStopOnEntryReportsRunning(t *testing.T) {
	tool, f := newFakeTool(t)
	res := mustExec(t, tool, `{"op":"launch","adapter":"fake","program":"/proj/main.go"}`)
	if res.IsError || !strings.Contains(res.Text, "debuggee running") {
		t.Fatalf("launch = %+v", res)
	}
	var args map[string]any
	if err := json.Unmarshal(f.lastArgs("launch"), &args); err != nil {
		t.Fatal(err)
	}
	if _, ok := args["stopOnEntry"]; ok {
		t.Fatalf("stopOnEntry sent without being asked: %v", args)
	}
}

// Adapter launch quirks: dlv's dap launch requires a mode, debugpy needs an
// internal console (xdev implements no runInTerminal).
func TestLaunchAdapterSpecificArgs(t *testing.T) {
	tests := []struct {
		adapter string
		wantKey string
		wantVal string
	}{
		{"dlv", "mode", "debug"},
		{"debugpy", "console", "internalConsole"},
	}
	for _, tc := range tests {
		t.Run(tc.adapter, func(t *testing.T) {
			tool, f := newFakeToolWith(t, DefaultAdapters())
			res := mustExec(t, tool, `{"op":"launch","adapter":"`+tc.adapter+`","program":"/proj/main"}`)
			if res.IsError {
				t.Fatalf("launch = %+v", res)
			}
			var args map[string]any
			if err := json.Unmarshal(f.lastArgs("launch"), &args); err != nil {
				t.Fatal(err)
			}
			if args[tc.wantKey] != tc.wantVal {
				t.Fatalf("%s launch args = %v, want %s=%s", tc.adapter, args, tc.wantKey, tc.wantVal)
			}
			if args["cwd"] != "/proj" || args["program"] != "/proj/main" {
				t.Fatalf("launch args = %v", args)
			}
		})
	}
}

func TestAttachPassesPid(t *testing.T) {
	tool, f := newFakeTool(t)
	res := mustExec(t, tool, `{"op":"attach","adapter":"fake","pid":4242}`)
	if res.IsError || !strings.Contains(res.Text, "attached to pid 4242 with fake") {
		t.Fatalf("attach = %+v", res)
	}
	var args struct {
		PID       int `json:"pid"`
		ProcessID int `json:"processId"`
	}
	if err := json.Unmarshal(f.lastArgs("attach"), &args); err != nil {
		t.Fatal(err)
	}
	if args.PID != 4242 || args.ProcessID != 4242 {
		t.Fatalf("attach args = %+v, want pid and processId (lldb-dap/gdb vs debugpy)", args)
	}
}

func TestBreakpointClearAndFunctionBreakpoints(t *testing.T) {
	tool, f := newFakeTool(t)
	startFake(t, tool)

	res := mustExec(t, tool, `{"op":"breakpoints","function":"main.main"}`)
	if res.IsError || !strings.Contains(res.Text, "#9 verified") {
		t.Fatalf("function breakpoint = %+v", res)
	}
	var fnReq struct {
		Breakpoints []struct {
			Name string `json:"name"`
		} `json:"breakpoints"`
	}
	if err := json.Unmarshal(f.lastArgs("setFunctionBreakpoints"), &fnReq); err != nil {
		t.Fatal(err)
	}
	if len(fnReq.Breakpoints) != 1 || fnReq.Breakpoints[0].Name != "main.main" {
		t.Fatalf("setFunctionBreakpoints args = %+v", fnReq)
	}

	empty := func(string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"breakpoints":[]}`), nil
	}
	f.setRespond(withOverride(defaultRespond(f), "setBreakpoints", empty))
	res = mustExec(t, tool, `{"op":"breakpoints","file":"main.go","clear":true}`)
	if res.IsError || !strings.Contains(res.Text, "breakpoints in main.go cleared") {
		t.Fatalf("clear = %+v", res)
	}
	var clearReq struct {
		Source struct {
			Path string `json:"path"`
		} `json:"source"`
		Breakpoints []any `json:"breakpoints"`
	}
	if err := json.Unmarshal(f.lastArgs("setBreakpoints"), &clearReq); err != nil {
		t.Fatal(err)
	}
	if clearReq.Source.Path != "/proj/main.go" || len(clearReq.Breakpoints) != 0 {
		t.Fatalf("clear args = %+v, want the file with no breakpoints", clearReq)
	}
}

func TestCustomRequestEscapeHatch(t *testing.T) {
	tool, f := newFakeTool(t)
	startFake(t, tool)
	f.setRespond(withOverride(defaultRespond(f), "disassemble", func(_ string, args json.RawMessage) (json.RawMessage, error) {
		var got struct {
			MemoryReference string `json:"memoryReference"`
			Count           int    `json:"count"`
		}
		if err := json.Unmarshal(args, &got); err != nil {
			return nil, err
		}
		if got.MemoryReference != "0x1000" || got.Count != 4 {
			return nil, fmt.Errorf("unexpected args %s", args)
		}
		return json.RawMessage(`{"instructions":[{"address":"0x1000","instruction":"nop"}]}`), nil
	}))
	res := mustExec(t, tool, `{"op":"custom","command":"disassemble","arguments":{"memoryReference":"0x1000","count":4}}`)
	if res.IsError || !strings.Contains(res.Text, "disassemble ok") || !strings.Contains(res.Text, "nop") {
		t.Fatalf("custom = %+v", res)
	}
	res = mustExec(t, tool, `{"op":"custom","command":"nonexistent"}`)
	if !res.IsError || !strings.Contains(res.Text, "unsupported request") {
		t.Fatalf("custom unsupported = %+v", res)
	}
}

// Output caps: frames, variables, program output and evaluate results are all
// bounded, and a cap says it truncated rather than silently lying.
func TestOutputCaps(t *testing.T) {
	tool, f := newFakeTool(t)
	startFake(t, tool)

	f.setRespond(withOverride(defaultRespond(f), "stackTrace", func(string, json.RawMessage) (json.RawMessage, error) {
		frames := make([]StackFrame, 0, 100)
		for i := 0; i < 100; i++ {
			frames = append(frames, StackFrame{ID: i + 1, Name: fmt.Sprintf("pkg.f%d", i), Line: i + 1})
		}
		b, err := json.Marshal(map[string]any{"totalFrames": 100, "stackFrames": frames})
		return b, err
	}))
	res := mustExec(t, tool, `{"op":"stack_trace"}`)
	if res.IsError || !strings.Contains(res.Text, "showing 30 of 100") {
		t.Fatalf("stack_trace cap = %q", res.Text)
	}
	if got := strings.Count(res.Text, "at "); got != MaxFrames {
		t.Fatalf("rendered %d frames, want the %d cap", got, MaxFrames)
	}

	f.setRespond(withOverride(defaultRespond(f), "variables", func(string, json.RawMessage) (json.RawMessage, error) {
		vars := make([]Variable, 0, MaxVariables+50)
		for i := 0; i < MaxVariables+50; i++ {
			vars = append(vars, Variable{Name: fmt.Sprintf("v%d", i), Value: strings.Repeat("y", 4000), Type: "string"})
		}
		b, err := json.Marshal(map[string]any{"variables": vars})
		return b, err
	}))
	res = mustExec(t, tool, `{"op":"variables","variable_ref":2}`)
	if res.IsError || !strings.Contains(res.Text, fmt.Sprintf("showing %d of %d", MaxVariables, MaxVariables+50)) {
		t.Fatalf("variables cap = %q", res.Text)
	}
	if !strings.Contains(res.Text, "[truncated") {
		t.Fatal("a 4000-byte value was not capped")
	}
	if len(res.Text) > MaxTextBytes+512 {
		t.Fatalf("variables output is %d bytes, want the %d byte cap", len(res.Text), MaxTextBytes)
	}

	f.setRespond(withOverride(defaultRespond(f), "continue", func(string, json.RawMessage) (json.RawMessage, error) {
		f.emit("output", OutputEvent{Category: "stdout", Output: strings.Repeat("line\n", 2000)})
		f.emit("output", OutputEvent{Category: "stdout", Output: "after\n"})
		f.emit("stopped", map[string]any{"reason": "breakpoint", "threadId": 1})
		return json.RawMessage(`{}`), nil
	}))
	res = mustExec(t, tool, `{"op":"continue"}`)
	if res.IsError || !strings.Contains(res.Text, "stopped: breakpoint") {
		t.Fatalf("continue = %+v", res)
	}
	if !strings.Contains(res.Text, "[truncated") {
		t.Fatal("program output was not capped")
	}
	if len(res.Text) > MaxOutputBytes+512 {
		t.Fatalf("program output is %d bytes, want the %d byte cap", len(res.Text), MaxOutputBytes)
	}

	f.setRespond(withOverride(defaultRespond(f), "evaluate", func(string, json.RawMessage) (json.RawMessage, error) {
		b, err := json.Marshal(EvaluateResult{Result: strings.Repeat("z", 4000), Type: "string"})
		return b, err
	}))
	res = mustExec(t, tool, `{"op":"evaluate","expression":"buf"}`)
	if res.IsError || !strings.Contains(res.Text, "[truncated") {
		t.Fatalf("evaluate cap = %q", res.Text)
	}
	if len(res.Text) > MaxValueBytes+512 {
		t.Fatalf("evaluate output is %d bytes, want the %d byte value cap", len(res.Text), MaxValueBytes)
	}
}

// A debuggee that runs free past the timeout leaves the session usable and is
// reported as still running instead of as an error.
func TestContinueTimeoutReportsStillRunning(t *testing.T) {
	tool, f := newFakeTool(t)
	startFake(t, tool)
	f.setRespond(withOverride(defaultRespond(f), "continue", func(string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil // no stop event ever arrives
	}))
	res := mustExec(t, tool, `{"op":"continue","timeout":5}`)
	if res.IsError || !strings.Contains(res.Text, "still running") {
		t.Fatalf("continue timeout = %+v", res)
	}
	res = mustExec(t, tool, `{"op":"threads"}`)
	if res.IsError {
		t.Fatalf("session unusable after a stop timeout: %+v", res)
	}
}

func TestOpArgumentErrors(t *testing.T) {
	tool, _ := newFakeTool(t)
	startFake(t, tool)
	tests := []struct {
		args string
		want string
	}{
		{`{}`, "op is required"},
		{`{"op":"bogus"}`, "unknown op"},
		{`{"op":"launch"}`, "needs program"},
		{`{"op":"attach"}`, "needs pid"},
		{`{"op":"breakpoints"}`, "needs file"},
		{`{"op":"breakpoints","file":"main.go"}`, "needs line"},
		{`{"op":"scopes"}`, "needs frame_id"},
		{`{"op":"variables"}`, "needs variable_ref"},
		{`{"op":"evaluate"}`, "needs expression"},
		{`{"op":"custom"}`, "needs command"},
	}
	for _, tc := range tests {
		res := mustExec(t, tool, tc.args)
		if !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Fatalf("%s = %+v, want an error mentioning %q", tc.args, res, tc.want)
		}
	}
}

func TestUnknownAdapterNamedInError(t *testing.T) {
	tool := NewToolWithConfig(Config{Adapters: DefaultAdapters()}, "/proj")
	res := mustExec(t, tool, `{"op":"launch","adapter":"ghost","program":"/prog"}`)
	if !res.IsError || !strings.Contains(res.Text, "unknown adapter \"ghost\"") || !strings.Contains(res.Text, "dlv") {
		t.Fatalf("unknown adapter = %+v", res)
	}
}

// The schema the model sees must offer exactly the ops the switch dispatches.
func TestSchemaMatchesDispatchedOps(t *testing.T) {
	tool := NewToolWithConfig(Config{Adapters: DefaultAdapters()}, "/proj")
	if tool.Name() != "debug" {
		t.Fatalf("Name() = %q", tool.Name())
	}
	var schema struct {
		Type       string `json:"type"`
		Required   []string
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("Parameters() is not valid JSON: %v", err)
	}
	if schema.Type != "object" || !equalStrings(schema.Required, []string{"op"}) {
		t.Fatalf("schema = %+v", schema)
	}
	want := strings.Split(opNames, "|")
	if !equalStrings(schema.Properties.Op.Enum, want) {
		t.Fatalf("schema enum %v does not match the dispatched ops %v", schema.Properties.Op.Enum, want)
	}
}

func TestConfigFromSettingsLayersAdapters(t *testing.T) {
	s := &config.Settings{Debug: &config.DebugConfig{
		Timeout: "45s",
		Adapters: map[string]config.DebugAdapter{
			"dlv":  {Command: "/opt/dlv", Args: []string{"dap", "--log"}},
			"nous": {Command: "nous", Args: []string{"--stdio"}, Languages: []string{"go"}},
		},
	}}
	cfg := ConfigFromSettings(s)
	if cfg.Timeout != 45*time.Second {
		t.Fatalf("timeout = %v", cfg.Timeout)
	}
	dlv := cfg.Adapters["dlv"]
	if dlv.Command != "/opt/dlv" || !equalStrings(dlv.Args, []string{"dap", "--log"}) {
		t.Fatalf("dlv override = %+v", dlv)
	}
	if !equalStrings(dlv.Languages, []string{"go"}) {
		t.Fatalf("dlv languages = %v, want the built-in ones kept", dlv.Languages)
	}
	// dlv answers on a socket, and an override that only changes the command
	// must keep that (a bool layer cannot express "unset").
	if !dlv.Socket {
		t.Fatalf("dlv override dropped the socket transport: %+v", dlv)
	}
	if cfg.Adapters["nous"].Command != "nous" {
		t.Fatalf("added adapter = %+v", cfg.Adapters["nous"])
	}
	if len(cfg.Adapters) != len(DefaultAdapters())+1 {
		t.Fatalf("adapters = %v, want the defaults plus one", cfg.Names())
	}

	// A nil settings value yields the built-ins.
	base := ConfigFromSettings(nil)
	if base.Timeout != DefaultTimeout || len(base.Adapters) != 3 {
		t.Fatalf("nil settings config = %+v", base)
	}
}

func TestAdapterForPicksByLanguage(t *testing.T) {
	cfg := Config{Adapters: DefaultAdapters()}
	tests := []struct {
		name    string
		adapter string
		file    string
		want    string
	}{
		{"explicit", "debugpy", "", "debugpy"},
		{"go file", "", "/proj/main.go", "dlv"},
		{"python file", "", "/proj/app.py", "debugpy"},
		{"rust file", "", "/proj/lib.rs", "lldb-dap"},
		{"c file", "", "/proj/hello.c", "lldb-dap"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, spec, err := cfg.AdapterFor(tc.adapter, tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || spec.Command == "" {
				t.Fatalf("AdapterFor(%q, %q) = %q (%+v), want %q", tc.adapter, tc.file, got, spec, tc.want)
			}
		})
	}
	if _, _, err := cfg.AdapterFor("", "/proj/README.md"); err == nil {
		t.Fatal("an unclaimed extension with several adapters must not guess")
	}
	if _, _, err := cfg.AdapterFor("ghost", ""); err == nil {
		t.Fatal("an unknown adapter name must be an error")
	}
	// Exactly one adapter configured: no need to name it.
	single := Config{Adapters: map[string]AdapterSpec{"only": {Command: "only"}}}
	if name, _, err := single.AdapterFor("", "/proj/README.md"); err != nil || name != "only" {
		t.Fatalf("single-adapter fallback = %q, %v", name, err)
	}
	// Nothing configured at all.
	if _, _, err := (Config{}).AdapterFor("", "/proj/main.go"); err == nil {
		t.Fatal("an empty adapter set must be an error")
	}
}

func TestRenderStopTerminated(t *testing.T) {
	tool, f := newFakeTool(t)
	startFake(t, tool)
	f.setRespond(withOverride(defaultRespond(f), "continue", func(string, json.RawMessage) (json.RawMessage, error) {
		f.emit("output", OutputEvent{Output: "bye\n"})
		f.emit("exited", map[string]any{"exitCode": 3})
		return json.RawMessage(`{}`), nil
	}))
	res := mustExec(t, tool, `{"op":"continue"}`)
	if res.IsError || !strings.Contains(res.Text, "exited: debuggee exited with code 3") {
		t.Fatalf("exited event = %+v", res)
	}
	if !strings.Contains(res.Text, "bye") {
		t.Fatalf("output of a finished debuggee was dropped: %q", res.Text)
	}
}
