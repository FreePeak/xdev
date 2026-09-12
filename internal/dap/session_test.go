package dap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// newDefaultAdapter wires the standard dlv-shaped responder onto an
// in-process adapter.
func newDefaultAdapter(t *testing.T) *fakeAdapter {
	t.Helper()
	f := newFakeAdapter(t, nil)
	f.setRespond(defaultRespond(f))
	return f
}

func TestHandshakeOrderAndLaunchArgs(t *testing.T) {
	f := newDefaultAdapter(t)
	if err := handshake(context.Background(), f.client, "fake", "launch", map[string]any{"program": "/prog/main.go"}); err != nil {
		t.Fatal(err)
	}
	// initialize → launch → configurationDone: an adapter that declares
	// supportsConfigurationDoneRequest holds the debuggee until
	// configurationDone, so the handshake must send it.
	if got, want := f.commands(), []string{"initialize", "launch", "configurationDone"}; !equalStrings(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	var init struct {
		AdapterID     string `json:"adapterID"`
		RunInTerminal bool   `json:"supportsRunInTerminalRequest"`
		LinesStartAt1 bool   `json:"linesStartAt1"`
		PathFormat    string `json:"pathFormat"`
	}
	if err := json.Unmarshal(f.lastArgs("initialize"), &init); err != nil {
		t.Fatal(err)
	}
	if init.AdapterID != "fake" || init.PathFormat != "path" || !init.LinesStartAt1 {
		t.Fatalf("initialize args = %+v", init)
	}
	// xdev implements no runInTerminal, so claiming it would make an adapter
	// wait for a reverse request that never comes.
	if init.RunInTerminal {
		t.Fatal("initialize claims supportsRunInTerminalRequest")
	}
	var launch struct {
		Program string `json:"program"`
	}
	if err := json.Unmarshal(f.lastArgs("launch"), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.Program != "/prog/main.go" {
		t.Fatalf("launch program = %q", launch.Program)
	}
}

// dlv never emits `initialized` (the spec's courtesy event) — the handshake
// must carry on and still launch: the configuration requests that follow a
// launch apply to the running debuggee.
func TestHandshakeToleratesMissingInitializedEvent(t *testing.T) {
	f := newFakeAdapter(t, func(command string, _ json.RawMessage) (json.RawMessage, error) {
		if command == "initialize" {
			return json.RawMessage(`{"supportsConfigurationDoneRequest":true}`), nil // no initialized event
		}
		return json.RawMessage(`{}`), nil
	})
	done := make(chan error, 1)
	go func() {
		done <- handshake(context.Background(), f.client, "dlv-like", "launch", map[string]any{"program": "/prog"})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dlv-style handshake failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake never finished without the initialized event")
	}
	if got, want := f.commands(), []string{"initialize", "launch", "configurationDone"}; !equalStrings(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestHandshakeSurfacesAdapterError(t *testing.T) {
	f := newFakeAdapter(t, nil)
	f.setRespond(func(command string, _ json.RawMessage) (json.RawMessage, error) {
		if command == "initialize" {
			f.emit("initialized", nil)
			return json.RawMessage(`{}`), nil
		}
		return nil, errors.New("could not launch program")
	})
	err := handshake(context.Background(), f.client, "fake", "launch", map[string]any{"program": "/prog"})
	if err == nil || !strings.Contains(err.Error(), "could not launch program") {
		t.Fatalf("err = %v", err)
	}
}

// helperAdapterConfig points an adapter at the test binary, which TestMain
// turns into a scripted DAP adapter — so the real spawn/teardown path runs.
func helperAdapterConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("XDEV_DAP_FAKE_ADAPTER", "1")
	return Config{
		Adapters: map[string]AdapterSpec{
			"fake": {Command: os.Args[0], Languages: []string{"go"}},
		},
		Timeout: 10 * time.Second,
	}
}

func TestSingleSessionRefusalAndTerminate(t *testing.T) {
	tool := NewToolWithConfig(helperAdapterConfig(t), t.TempDir())
	t.Cleanup(func() {
		if c, _ := activeSession(); c != nil {
			_ = c.Close()
		}
	})
	res := mustExec(t, tool, `{"op":"launch","adapter":"fake","program":"/prog/main.go"}`)
	if res.IsError || !strings.Contains(res.Text, "launched") {
		t.Fatalf("launch = %+v", res)
	}
	c, desc := activeSession()
	if c == nil || !strings.Contains(desc, "fake") {
		t.Fatalf("activeSession = %v, %q", c, desc)
	}

	// A second session — launch or attach — is refused with the active one named.
	res = mustExec(t, tool, `{"op":"launch","adapter":"fake","program":"/prog/other"}`)
	if !res.IsError || !strings.Contains(res.Text, "already active") || !strings.Contains(res.Text, "fake") {
		t.Fatalf("second launch = %+v, want a refusal naming the active session", res)
	}
	res = mustExec(t, tool, `{"op":"attach","adapter":"fake","pid":1}`)
	if !res.IsError || !strings.Contains(res.Text, "already active") {
		t.Fatalf("attach while active = %+v", res)
	}

	// terminate ends the debuggee, reaps the adapter process and frees the slot.
	res = mustExec(t, tool, `{"op":"terminate"}`)
	if res.IsError {
		t.Fatalf("terminate = %+v", res)
	}
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the adapter process was not reaped after terminate")
	}
	if c.alive() {
		t.Fatal("client still alive after terminate")
	}
	if other, _ := activeSession(); other != nil {
		t.Fatal("the session slot is still taken after terminate")
	}
	if err := claimLaunch("test"); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	abortLaunch()

	// …and a fresh session starts again.
	res = mustExec(t, tool, `{"op":"launch","adapter":"fake","program":"/prog/main.go"}`)
	if res.IsError {
		t.Fatalf("relaunch = %+v", res)
	}
}

func TestLaunchMissingAdapterBinaryIsActionable(t *testing.T) {
	tool := NewToolWithConfig(Config{
		Adapters: map[string]AdapterSpec{"ghost": {Command: "xdev-no-such-adapter-binary"}},
		Timeout:  5 * time.Second,
	}, t.TempDir())

	res := mustExec(t, tool, `{"op":"launch","adapter":"ghost","program":"/prog/main.go"}`)
	if !res.IsError {
		t.Fatalf("launch with a missing binary = %+v, want an error result", res)
	}
	for _, want := range []string{"not found on PATH", "debug.adapters.ghost.command"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("error %q does not mention %q", res.Text, want)
		}
	}
	// A failed launch must not wedge the process-wide slot.
	if c, _ := activeSession(); c != nil {
		t.Fatalf("a failed launch left a session behind: %v", c)
	}
	if err := claimLaunch("test"); err != nil {
		t.Fatalf("slot still claimed after a failed launch: %v", err)
	}
	abortLaunch()
}

// The adapter is started lazily: no launch means no process and an actionable
// error from every session-bound op.
func TestLazyStartAndSessionBoundOps(t *testing.T) {
	tool := NewToolWithConfig(Config{Adapters: DefaultAdapters()}, t.TempDir())
	res := mustExec(t, tool, `{"op":"threads"}`)
	if !res.IsError || !strings.Contains(res.Text, "no active debug session") {
		t.Fatalf("threads without a session = %+v", res)
	}
	res = mustExec(t, tool, `{"op":"status"}`)
	if res.IsError || !strings.Contains(res.Text, "no active debug session") {
		t.Fatalf("status = %+v", res)
	}
	if !strings.Contains(res.Text, "dlv: dlv dap") {
		t.Fatalf("status does not list the configured adapters: %q", res.Text)
	}
}

// Socket adapters (dlv's --client-addr mode) answer DAP on a TCP connection
// xdev listens for, instead of on stdio.
func TestSocketAdapterSession(t *testing.T) {
	t.Setenv("XDEV_DAP_FAKE_ADAPTER", "1")
	cfg := Config{
		Adapters: map[string]AdapterSpec{"fake": {Command: os.Args[0], Socket: true, Languages: []string{"go"}}},
		Timeout:  10 * time.Second,
	}
	tool := NewToolWithConfig(cfg, t.TempDir())
	t.Cleanup(func() {
		if c, _ := activeSession(); c != nil {
			_ = c.Close()
		}
	})

	if res := mustExec(t, tool, `{"op":"launch","adapter":"fake","program":"/prog/main.go"}`); res.IsError {
		t.Fatalf("socket launch = %+v", res)
	}
	c, _ := activeSession()
	if c == nil {
		t.Fatal("no session after a socket launch")
	}
	if res := mustExec(t, tool, `{"op":"threads"}`); res.IsError || !strings.Contains(res.Text, "#1 fake") {
		t.Fatalf("threads over the dial-in connection = %+v", res)
	}
	if res := mustExec(t, tool, `{"op":"terminate"}`); res.IsError {
		t.Fatalf("terminate = %+v", res)
	}
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the socket adapter process was not reaped")
	}
}

// A tool with no session must survive teardown: Close is the harness hook and
// has nothing to stop.
func TestCloseWithoutSession(t *testing.T) {
	tool := NewToolWithConfig(helperAdapterConfig(t), t.TempDir())
	if c, _ := activeSession(); c != nil {
		t.Fatalf("unexpected pre-existing session %v", c)
	}
	tool.Close()
	if err := claimLaunch("after close"); err != nil {
		t.Fatalf("slot wedged by Close: %v", err)
	}
	abortLaunch()
}
