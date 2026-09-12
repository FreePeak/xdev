package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/acp"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
)

// stubProvider streams scripted events, standing in for a real model.
type stubProvider struct {
	mu      sync.Mutex
	scripts [][]ai.Event
	i       int
}

func (p *stubProvider) Stream(_ context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.i >= len(p.scripts) {
		return nil, fmt.Errorf("stub provider: script exhausted after %d calls", p.i)
	}
	events := p.scripts[p.i]
	p.i++
	ch := make(chan ai.Event, len(events)+1)
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *stubProvider) Name() string { return "stub" }
func (p *stubProvider) API() string  { return "stub" }

// acpClient is the editor side of the wire for these tests.
type acpClient struct {
	t      *testing.T
	w      io.Writer
	frames chan map[string]any
}

func startACPClient(t *testing.T, h acp.Handler) *acpClient {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	srv := acp.New(c2sR, s2cW, h)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()

	c := &acpClient{t: t, w: c2sW, frames: make(chan map[string]any, 128)}
	go func() {
		defer close(c.frames)
		r := bufio.NewReader(s2cR)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			// Newline-delimited JSON: the ACP transport (T5). The client
			// half speaks what an editor speaks, not what xdev used to emit.
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				return
			}
			c.frames <- m
		}
	}()
	return c
}

func (c *acpClient) send(id any, method string, params any) {
	c.t.Helper()
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		frame["id"] = id
	}
	if params != nil {
		frame["params"] = params
	}
	body, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := fmt.Fprintf(c.w, "%s\n", body); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
}

func (c *acpClient) await(what string, pred func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
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

func acpObject(t *testing.T, v any) map[string]any {
	t.Helper()
	o, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value %#v is not an object", v)
	}
	return o
}

// TestACPModeDrivesTheAgentLoop proves the ACP mode reuses the agent loop end
// to end: a stub provider's stream turns into session/update notifications,
// the read tool really runs, and the prompt resolves with the stop reason.
func TestACPModeDrivesTheAgentLoop(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()
	// An editor spawns the agent in the workspace; tools resolve against
	// the process working directory.
	t.Chdir(cwd)
	file := filepath.Join(cwd, "hello.txt")
	if err := os.WriteFile(file, []byte("hello from the tool\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := &stubProvider{scripts: [][]ai.Event{
		{
			{Type: ai.EventStart, Provider: "stub", Model: "m"},
			{Type: ai.EventThinkingDelta, Delta: "look first"},
			{Type: ai.EventTextDelta, Delta: "reading "},
			{Type: ai.EventTextDelta, Delta: "hello.txt"},
			{Type: ai.EventToolcallStart, ToolCallID: "call-1", ToolName: "read", StreamIndex: 0},
			{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"path":"hello.txt"}`},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop}),
		},
		{
			{Type: ai.EventStart, Provider: "stub", Model: "m"},
			{Type: ai.EventTextDelta, Delta: "file read"},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
				Content: []ai.Block{ai.TextBlock{Text: "file read"}}}),
		},
	}}

	reg := newToolRegistry(cwd, prov, "stub", "m", lastSettings(), nil, nil)
	h := newACPHandler(cwd, &config.Config{}, prov, "stub", "m", reg, func() string { return "system" }, nil, 0, 0)
	t.Cleanup(h.close)
	c := startACPClient(t, h)

	c.send(1.0, acp.MethodInitialize, map[string]any{"protocolVersion": 1})
	init := acpObject(t, c.await("initialize response", func(m map[string]any) bool {
		id, ok := m["id"].(float64)
		return ok && id == 1
	})["result"])
	if got := acpObject(t, init["agentInfo"])["name"]; got != "xdev" {
		t.Fatalf("agentInfo.name = %v, want xdev", got)
	}

	c.send(2.0, acp.MethodNewSession, map[string]any{"cwd": cwd})
	sessionID, _ := acpObject(t, c.await("session/new response", func(m map[string]any) bool {
		id, ok := m["id"].(float64)
		return ok && id == 2
	})["result"])["sessionId"].(string)
	if sessionID == "" {
		t.Fatal("session/new returned no sessionId")
	}

	c.send(3.0, acp.MethodPrompt, map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "read hello.txt"}},
	})

	// Updates stream in order until the prompt resolves.
	var got []string
	var toolCall, toolDone map[string]any
	text := ""
	var stopReason string
	for stopReason == "" {
		m := c.await("session/update or prompt response", func(m map[string]any) bool { return true })
		if id, ok := m["id"].(float64); ok && id == 3 {
			if e := m["error"]; e != nil {
				t.Fatalf("prompt failed: %v", e)
			}
			stopReason, _ = acpObject(t, m["result"])["stopReason"].(string)
			break
		}
		params := acpObject(t, m["params"])
		if params["sessionId"] != sessionID {
			t.Fatalf("update sessionId = %v, want %s", params["sessionId"], sessionID)
		}
		update := acpObject(t, params["update"])
		kind, _ := update["sessionUpdate"].(string)
		got = append(got, kind)
		switch kind {
		case acp.UpdateAgentMessageChunk:
			text += acpObject(t, update["content"])["text"].(string)
		case acp.UpdateToolCall:
			toolCall = update
		case acp.UpdateToolCallUpdate:
			toolDone = update
		}
	}

	want := []string{
		acp.UpdateAgentThoughtChunk, acp.UpdateAgentMessageChunk, acp.UpdateAgentMessageChunk,
		acp.UpdateToolCall, acp.UpdateToolCallUpdate, acp.UpdateAgentMessageChunk,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("update sequence = %v, want %v", got, want)
	}
	if text != "reading hello.txtfile read" {
		t.Fatalf("streamed text = %q", text)
	}
	if stopReason != acp.StopEndTurn {
		t.Fatalf("stopReason = %q, want %s", stopReason, acp.StopEndTurn)
	}
	if toolCall["toolCallId"] != "call-1" || toolCall["kind"] != acp.KindRead || toolCall["title"] != "read" {
		t.Fatalf("tool_call = %v", toolCall)
	}
	if path := acpObject(t, toolCall["rawInput"])["path"]; path != "hello.txt" {
		t.Fatalf("tool_call rawInput = %v", toolCall["rawInput"])
	}
	if toolDone["status"] != acp.StatusCompleted {
		t.Fatalf("tool_call_update = %v", toolDone)
	}
	blocks, ok := toolDone["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("tool_call_update content = %v", toolDone["content"])
	}
	result := acpObject(t, acpObject(t, blocks[0])["content"])["text"].(string)
	if !strings.Contains(result, "hello from the tool") {
		t.Fatalf("tool result streamed to the client = %q", result)
	}
}

// TestACPCancelStopsTheRun proves session/cancel unwinds a live turn and the
// prompt still answers, with the spec's cancelled stop reason.
func TestACPCancelStopsTheRun(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()

	blocked := make(chan struct{})
	// A provider that never yields: the turn can only end by cancellation.
	slow := &blockingProvider{started: blocked}
	reg := newToolRegistry(cwd, slow, "stub", "m", lastSettings(), nil, nil)
	h := newACPHandler(cwd, &config.Config{}, slow, "stub", "m", reg, func() string { return "system" }, nil, 0, 0)
	t.Cleanup(h.close)
	c := startACPClient(t, h)

	c.send(1.0, acp.MethodNewSession, map[string]any{"cwd": cwd})
	sessionID, _ := acpObject(t, c.await("session/new response", func(m map[string]any) bool {
		id, ok := m["id"].(float64)
		return ok && id == 1
	})["result"])["sessionId"].(string)

	c.send(2.0, acp.MethodPrompt, map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "hang"}},
	})
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("provider stream never started")
	}
	c.send(nil, acp.MethodCancel, map[string]any{"sessionId": sessionID})

	res := c.await("cancelled prompt response", func(m map[string]any) bool {
		id, ok := m["id"].(float64)
		return ok && id == 2
	})
	if got := acpObject(t, res["result"])["stopReason"]; got != acp.StopCancelled {
		t.Fatalf("stopReason = %v, want %s", got, acp.StopCancelled)
	}
}

// blockingProvider opens its stream immediately and never emits, so only the
// turn's context can end it.
type blockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Stream(ctx context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	p.once.Do(func() { close(p.started) })
	ch := make(chan ai.Event)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) API() string  { return "blocking" }

// Mode parity for the deferred-tool catalog (#79): tool_call bridges into the
// agent's own call path in EVERY mode. Print wired this; TUI, RPC and ACP did
// not, so a catalogued tool refused with "no runner installed" in three of the
// four modes while the prompt index advertised it.
func TestACPModeWiresTheDeferredCatalog(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)

	prov := &stubProvider{scripts: [][]ai.Event{
		{
			{Type: ai.EventStart, Provider: "stub", Model: "m"},
			{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "tool_call", StreamIndex: 0},
			{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"name":"checkpoint","args":{"goal":"catalog parity probe"}}`},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop}),
		},
		{
			{Type: ai.EventStart, Provider: "stub", Model: "m"},
			ai.Donef(ai.StopReasonStop, nil, &ai.Message{Role: ai.RoleAssistant, StopReason: ai.StopReasonStop,
				Content: []ai.Block{ai.TextBlock{Text: "done"}}}),
		},
	}}

	reg := newToolRegistry(cwd, prov, "stub", "m", lastSettings(), nil, nil)
	h := newACPHandler(cwd, &config.Config{}, prov, "stub", "m", reg, func() string { return "system" }, nil, 0, 0)
	t.Cleanup(h.close)
	c := startACPClient(t, h)

	c.send(1.0, acp.MethodInitialize, map[string]any{"protocolVersion": 1})
	c.await("initialize", func(m map[string]any) bool { return m["id"] != nil })
	c.send(2.0, acp.MethodNewSession, map[string]any{"cwd": cwd})
	newRes := acpObject(t, c.await("session/new", func(m map[string]any) bool {
		id, ok := m["id"].(float64)
		return ok && id == 2
	})["result"])
	sessionID, _ := newRes["sessionId"].(string)
	if sessionID == "" {
		t.Fatal("no sessionId")
	}
	c.send(3.0, acp.MethodPrompt, map[string]any{"sessionId": sessionID, "prompt": []any{
		map[string]any{"type": "text", "text": "probe"},
	}})

	// Collect every tool_call_update / tool result frame for this prompt and
	// assert the deferred tool actually ran (no "no runner installed").
	var sawResult map[string]any
	var frames []string
	c.await("prompt response", func(m map[string]any) bool {
		if method, _ := m["method"].(string); strings.Contains(method, "update") {
			if raw, ok := m["params"].(map[string]any); ok {
				blob, _ := json.Marshal(raw)
				frames = append(frames, string(blob))
				if strings.Contains(string(blob), "checkpoint") || strings.Contains(string(blob), "no runner") {
					sawResult = raw
				}
			}
		}
		id, ok := m["id"].(float64)
		return ok && id == 3
	})
	t.Logf("update frames (%d):\n%s", len(frames), strings.Join(frames, "\n"))
	all := strings.Join(frames, "\n")
	// Silence would pass a pure negative check, so require BOTH: the call
	// produced a result, and that result is the deferred tool's own answer
	// (its arg validation here) rather than the unwired-catalog refusal.
	if frames == nil || !strings.Contains(all, "checkpoint:") {
		t.Fatalf("the deferred tool never ran; update frames:\n%s", all)
	}
	if strings.Contains(all, "no runner installed") {
		t.Fatalf("catalog bridge missing in ACP:\n%s", all)
	}
	_ = sawResult
}
