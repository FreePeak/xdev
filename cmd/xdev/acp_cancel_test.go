package main

import (
	"context"
	"encoding/json"
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

// cancelLeakProvider streams a tool call and then blocks, so the cancel lands
// while the turn is mid-flight with a tool call already decoded.
type cancelLeakProvider struct {
	started chan struct{}
	release chan struct{} // held open until the test says the cancel is in
	once    sync.Once
}

func (p *cancelLeakProvider) Stream(ctx context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	p.once.Do(func() { close(p.started) })
	ch := make(chan ai.Event, 16)
	ch <- ai.Event{Type: ai.EventStart, Provider: "stub", Model: "m"}
	ch <- ai.Event{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "write", StreamIndex: 0}
	ch <- ai.Event{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"path":"leaked.txt","content":"SHOULD NOT EXIST"}`}
	// Hold the stream open on a TEST-owned channel — deliberately not
	// ctx.Done(), which would let an entirely broken cancel path pass by never
	// reaching execution at all. Releasing after the cancel has been sent makes
	// the ordering a fact rather than a race: the loop is guaranteed to reach
	// runTools with an already-cancelled context, so the guard must fire and a
	// missing guard cannot hide behind timing.
	if p.release != nil {
		<-p.release
	}
	close(ch)
	return ch, nil
}
func (p *cancelLeakProvider) Name() string { return "stub" }
func (p *cancelLeakProvider) API() string  { return "stub" }

func TestACPCancelStopsToolExecution(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	p := &cancelLeakProvider{started: make(chan struct{}), release: make(chan struct{})}
	reg := newToolRegistry(cwd, p, "stub", "m", lastSettings(), nil, nil)
	h := newACPHandler(cwd, &config.Config{}, p, "stub", "m", reg, func() string { return "sys" }, nil, 0, 0)
	t.Cleanup(h.close)
	c := startACPClient(t, h)
	c.send(1.0, acp.MethodInitialize, map[string]any{"protocolVersion": 1})
	c.await("init", func(m map[string]any) bool { return m["id"] != nil })
	c.send(2.0, acp.MethodNewSession, map[string]any{"cwd": cwd})
	sid := acpObject(t, c.await("new", func(m map[string]any) bool {
		id, ok := m["id"].(float64)
		return ok && id == 2
	})["result"])["sessionId"].(string)
	c.send(3.0, acp.MethodPrompt, map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "go"}}})
	// The turn must be in flight before it is cancelled. Without this wait the
	// cancel usually lands before any turn exists, there is nothing to cancel,
	// and the assertion below passes for the wrong reason.
	select {
	case <-p.started:
	case <-time.After(20 * time.Second):
		t.Fatal("the prompt never opened a stream")
	}
	c.send(nil, acp.MethodCancel, map[string]any{"sessionId": sid})
	// Let the stream finish now that cancellation is in: execution is reached
	// with a cancelled context, which is exactly the window the guard closes.
	close(p.release)
	// PromptResult resolves only when the turn ends or is cancelled, after every
	// session/update has gone out. Awaiting that edge bounds the check by
	// ordering instead of by a sleep: response arrived means the turn is
	// finished, so any leaked.txt is unambiguously a post-cancel execution.
	c.await("prompt response", func(m map[string]any) bool {
		id, ok := m["id"].(float64)
		return ok && id == 3
	})
	if _, err := os.Stat(filepath.Join(cwd, "leaked.txt")); err == nil {
		raw, _ := os.ReadFile(filepath.Join(cwd, "leaked.txt"))
		t.Fatalf("tool ran after session/cancel: leaked.txt = %q", strings.TrimSpace(string(raw)))
	}
	// The other leak T5 could mean: a cancelled turn still pushing tool frames
	// to the editor AFTER its response. Drain what arrives after the response
	// and require silence.
	late := 0
	deadline := time.After(800 * time.Millisecond)
	for late < 20 {
		select {
		case m, ok := <-c.frames:
			if !ok {
				return
			}
			if method, _ := m["method"].(string); strings.Contains(method, "update") {
				blob, _ := json.Marshal(m)
				if strings.Contains(string(blob), "leaked.txt") || strings.Contains(string(blob), "c1") {
					t.Fatalf("tool frame leaked after cancel: %s", blob)
				}
			}
		case <-deadline:
			return
		}
	}
}
