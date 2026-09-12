package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// stubAgent records what the protocol asked of it. Prompt behaviour is
// injected per test.
type stubAgent struct {
	prompt func(ctx context.Context, sessionID string, blocks []ContentBlock, emit Emitter) (string, error)

	mu       sync.Mutex
	sessions []string
}

func (s *stubAgent) NewSession(_ context.Context, cwd string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, cwd)
	return "sess-1", nil
}

func (s *stubAgent) Prompt(ctx context.Context, sessionID string, blocks []ContentBlock, emit Emitter) (string, error) {
	if s.prompt == nil {
		return StopEndTurn, nil
	}
	return s.prompt(ctx, sessionID, blocks, emit)
}

// client is the editor side of the wire: framed requests out, decoded frames
// in on a channel.
type client struct {
	t      *testing.T
	w      io.Writer
	frames chan map[string]any
}

// startServer runs a server over an in-memory transport and returns its client.
func startServer(t *testing.T, h Handler) *client {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	srv := New(c2sR, s2cW, h)
	go func() { _ = srv.Serve(context.Background()) }()
	c := &client{t: t, w: c2sW, frames: make(chan map[string]any, 64)}
	go func() {
		defer close(c.frames)
		r := bufio.NewReader(s2cR)
		for {
			body, err := readFrame(r)
			if err != nil {
				return
			}
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				return
			}
			c.frames <- m
		}
	}()
	return c
}

func (c *client) send(id any, method string, params any) {
	c.t.Helper()
	f := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		f["id"] = id
	}
	if params != nil {
		f["params"] = params
	}
	body, err := json.Marshal(f)
	if err != nil {
		c.t.Fatalf("marshal %s: %v", method, err)
	}
	if err := writeFrame(c.w, body); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
}

// reply answers a server-initiated request.
func (c *client) reply(id any, result any) {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		c.t.Fatalf("marshal reply: %v", err)
	}
	if err := writeFrame(c.w, body); err != nil {
		c.t.Fatalf("write reply: %v", err)
	}
}

// await returns the next frame matching pred, in arrival order.
func (c *client) await(what string, pred func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.frames:
			if !ok {
				c.t.Fatalf("stream closed while waiting for %s", what)
			}
			if pred(m) {
				return m
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func response(id float64) func(map[string]any) bool {
	return func(m map[string]any) bool {
		got, ok := m["id"].(float64)
		return ok && got == id && m["method"] == nil
	}
}

func notification(method string) func(map[string]any) bool {
	return func(m map[string]any) bool { s, _ := m["method"].(string); return s == method && m["id"] == nil }
}

// requestFrame matches a message from the server that expects an answer.
func requestFrame(method string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		s, _ := m["method"].(string)
		_, hasID := m["id"]
		return s == method && hasID
	}
}

func object(t *testing.T, v any) map[string]any {
	t.Helper()
	o, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value %#v is not an object", v)
	}
	return o
}

func TestInitializeHandshake(t *testing.T) {
	c := startServer(t, &stubAgent{})
	c.send(1.0, MethodInitialize, map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"terminal": false},
		"clientInfo":         map[string]any{"name": "zed", "version": "0.1"},
	})

	res := c.await("initialize response", response(1))
	if res["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", res["jsonrpc"])
	}
	result := object(t, res["result"])
	if got := result["protocolVersion"]; got != float64(ProtocolVersion) {
		t.Errorf("protocolVersion = %v, want %d", got, ProtocolVersion)
	}
	caps := object(t, result["agentCapabilities"])
	if caps["loadSession"] != false {
		t.Errorf("loadSession = %v, want false", caps["loadSession"])
	}
	prompt := object(t, caps["promptCapabilities"])
	if prompt["image"] != false || prompt["audio"] != false || prompt["embeddedContext"] != false {
		t.Errorf("promptCapabilities = %v, want all false (text prompts only)", prompt)
	}
	info := object(t, result["agentInfo"])
	if info["name"] != "xdev" || info["version"] == "" {
		t.Errorf("agentInfo = %v, want the xdev name and a version", info)
	}
}

func TestNewSessionAndPromptStreamUpdates(t *testing.T) {
	agent := &stubAgent{prompt: func(_ context.Context, sessionID string, blocks []ContentBlock, emit Emitter) (string, error) {
		if sessionID != "sess-1" {
			t.Errorf("prompt session = %q, want sess-1", sessionID)
		}
		if got := PromptText(blocks); got != "hello\nworld" {
			t.Errorf("prompt text = %q, want %q", got, "hello\nworld")
		}
		emit.Update(Chunk(UpdateAgentThoughtChunk, "thinking"))
		emit.Update(Chunk(UpdateAgentMessageChunk, "hi "))
		emit.Update(Chunk(UpdateAgentMessageChunk, "there"))
		emit.Update(ToolCallStarted("call-1", "read", ToolKind("read"), json.RawMessage(`{"path":"a.go"}`)))
		emit.Update(ToolCallUpdated("call-1", StatusCompleted, "package main"))
		return StopEndTurn, nil
	}}
	c := startServer(t, agent)

	c.send(1.0, MethodNewSession, map[string]any{"cwd": "/work", "mcpServers": []any{}})
	res := c.await("session/new response", response(1))
	if sid := object(t, res["result"])["sessionId"]; sid != "sess-1" {
		t.Fatalf("sessionId = %v, want sess-1", sid)
	}
	agent.mu.Lock()
	if len(agent.sessions) != 1 || agent.sessions[0] != "/work" {
		t.Errorf("sessions = %v, want the client cwd", agent.sessions)
	}
	agent.mu.Unlock()

	c.send(2.0, MethodPrompt, map[string]any{
		"sessionId": "sess-1",
		"prompt":    []any{map[string]any{"type": "text", "text": "hello"}, map[string]any{"type": "text", "text": "world"}},
	})

	// Updates arrive in emission order, before the prompt response resolves.
	want := []string{UpdateAgentThoughtChunk, UpdateAgentMessageChunk, UpdateAgentMessageChunk, UpdateToolCall, UpdateToolCallUpdate}
	for _, kind := range want {
		f := c.await("session/update "+kind, notification(MethodUpdate))
		params := object(t, f["params"])
		if params["sessionId"] != "sess-1" {
			t.Fatalf("%s sessionId = %v", kind, params["sessionId"])
		}
		if got := object(t, params["update"])["sessionUpdate"]; got != kind {
			t.Fatalf("sessionUpdate = %v, want %s", got, kind)
		}
		switch kind {
		case UpdateAgentMessageChunk:
			content := object(t, object(t, params["update"])["content"])
			if content["type"] != "text" || content["text"] == "" {
				t.Fatalf("message chunk content = %v", content)
			}
		case UpdateToolCall:
			tc := object(t, params["update"])
			if tc["toolCallId"] != "call-1" || tc["kind"] != KindRead || tc["status"] != StatusInProgress {
				t.Fatalf("tool_call = %v", tc)
			}
			if got := object(t, tc["rawInput"])["path"]; got != "a.go" {
				t.Fatalf("tool_call rawInput = %v", tc["rawInput"])
			}
		case UpdateToolCallUpdate:
			tc := object(t, params["update"])
			if tc["status"] != StatusCompleted || tc["toolCallId"] != "call-1" {
				t.Fatalf("tool_call_update = %v", tc)
			}
			outer, ok := tc["content"].([]any)
			if !ok || len(outer) != 1 {
				t.Fatalf("tool_call_update content = %v", tc["content"])
			}
			block := object(t, outer[0])
			if block["type"] != "content" || object(t, block["content"])["text"] != "package main" {
				t.Fatalf("tool_call_update content block = %v", block)
			}
		}
	}

	done := c.await("prompt response", response(2))
	if got := object(t, done["result"])["stopReason"]; got != StopEndTurn {
		t.Fatalf("stopReason = %v, want %s", got, StopEndTurn)
	}
	if done["error"] != nil {
		t.Fatalf("prompt answered with an error: %v", done["error"])
	}
}

func TestCancelStopsRun(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	agent := &stubAgent{prompt: func(ctx context.Context, _ string, _ []ContentBlock, _ Emitter) (string, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}}
	c := startServer(t, agent)

	c.send(1.0, MethodPrompt, map[string]any{"sessionId": "sess-1", "prompt": []any{map[string]any{"type": "text", "text": "long"}}})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt never started")
	}
	c.send(nil, MethodCancel, map[string]any{"sessionId": "sess-1"})

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel never reached the running turn")
	}
	done := c.await("cancelled prompt response", response(1))
	if got := object(t, done["result"])["stopReason"]; got != StopCancelled {
		t.Fatalf("stopReason = %v, want %s", got, StopCancelled)
	}
}

func TestCancelAsRequestStillCancels(t *testing.T) {
	started := make(chan struct{})
	agent := &stubAgent{prompt: func(ctx context.Context, _ string, _ []ContentBlock, _ Emitter) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	c := startServer(t, agent)
	c.send(1.0, MethodPrompt, map[string]any{"sessionId": "sess-1", "prompt": []any{map[string]any{"type": "text", "text": "x"}}})
	<-started
	// Some clients send cancel as a request; it must be answered, not failed.
	c.send(2.0, MethodCancel, map[string]any{"sessionId": "sess-1"})
	if res := c.await("cancel response", response(2)); res["error"] != nil {
		t.Fatalf("cancel-as-request failed: %v", res["error"])
	}
	if got := object(t, c.await("cancelled prompt response", response(1))["result"])["stopReason"]; got != StopCancelled {
		t.Fatalf("stopReason = %v, want %s", got, StopCancelled)
	}
}

func TestPermissionRequestRoundTrip(t *testing.T) {
	allowed := make(chan bool, 1)
	agent := &stubAgent{prompt: func(ctx context.Context, _ string, _ []ContentBlock, emit Emitter) (string, error) {
		out, err := emit.Permission(ctx, PermissionRequest{
			ToolCall: PermissionToolCall{ToolCallID: "call-9", Title: "bash", Kind: KindExecute, RawInput: json.RawMessage(`{"command":"rm -rf /"}`)},
			Options:  ApprovalOptions(),
		})
		if err != nil {
			allowed <- false
			return "", err
		}
		allowed <- out.Allowed()
		return StopEndTurn, nil
	}}
	c := startServer(t, agent)

	c.send(1.0, MethodPrompt, map[string]any{"sessionId": "sess-1", "prompt": []any{map[string]any{"type": "text", "text": "clean up"}}})
	req := c.await("permission request", requestFrame(MethodRequestPermission))
	id := req["id"]
	if id == nil {
		t.Fatalf("permission request carried no id: %v", req)
	}
	params := object(t, req["params"])
	if params["sessionId"] != "sess-1" {
		t.Fatalf("permission sessionId = %v", params["sessionId"])
	}
	if tc := object(t, params["toolCall"]); tc["toolCallId"] != "call-9" || tc["title"] != "bash" {
		t.Fatalf("permission toolCall = %v", tc)
	}
	opts, ok := params["options"].([]any)
	if !ok || len(opts) != len(ApprovalOptions()) {
		t.Fatalf("permission options = %v", params["options"])
	}
	if first := object(t, opts[0]); first["optionId"] != OptionAllowOnce || first["kind"] != OptionAllowOnce {
		t.Fatalf("first option = %v", first)
	}

	c.reply(req["id"], map[string]any{"outcome": map[string]any{"outcome": OutcomeSelected, "optionId": OptionAllowOnce}})
	select {
	case ok := <-allowed:
		if !ok {
			t.Fatal("allow_once was not accepted as an approval")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("permission outcome never reached the agent")
	}
	if got := object(t, c.await("prompt response", response(1))["result"])["stopReason"]; got != StopEndTurn {
		t.Fatalf("stopReason = %v", got)
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	c := startServer(t, &stubAgent{})
	c.send(7.0, "session/frobnicate", map[string]any{})
	res := c.await("method-not-found response", response(7))
	errObj := object(t, res["error"])
	if got := errObj["code"]; got != float64(CodeMethodNotFound) {
		t.Fatalf("error code = %v, want %d", got, CodeMethodNotFound)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Fatalf("error message is empty: %v", res)
	}
	if res["result"] != nil {
		t.Fatalf("response carried a result: %v", res)
	}
}
