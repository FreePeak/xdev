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
	once    sync.Once
}

func (p *cancelLeakProvider) Stream(ctx context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	p.once.Do(func() { close(p.started) })
	ch := make(chan ai.Event, 16)
	ch <- ai.Event{Type: ai.EventStart, Provider: "stub", Model: "m"}
	ch <- ai.Event{Type: ai.EventToolcallStart, ToolCallID: "c1", ToolName: "write", StreamIndex: 0}
	ch <- ai.Event{Type: ai.EventToolcallEnd, StreamIndex: 0, PartialJSON: `{"path":"leaked.txt","content":"SHOULD NOT EXIST"}`}
	close(ch)
	return ch, nil
}
func (p *cancelLeakProvider) Name() string { return "stub" }
func (p *cancelLeakProvider) API() string  { return "stub" }

func TestACPCancelStopsToolExecution(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	p := &cancelLeakProvider{started: make(chan struct{})}
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
	c.send(nil, acp.MethodCancel, map[string]any{"sessionId": sid})
	// Give the loop a moment to reach tool execution either way.
	time.Sleep(600 * time.Millisecond)
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
