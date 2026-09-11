// Package hooks implements config-driven shell-command hooks (M11,
// research §4 CORE): a settings-declared map of event → commands. Each
// command runs with the event payload as JSON on stdin; a non-zero exit
// is fail-closed (the action is denied). For tool_call pre-hooks the
// command's stdout may carry {"block":true,"reason":"…"} or an
// {"input":…} replacement; tool_result hooks may return {"text":…}.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/logx"
)

// Bus is the parsed hook configuration. nil Bus = no hooks configured;
// every method is nil-safe so agents never special-case hooks.
type Bus struct {
	// Commands maps an event name to the shell commands that run on it,
	// in declaration order. Last-wins for input/result overrides (omp
	// semantics); the first block short-circuits.
	Commands map[string][]string
}

// DefaultTimeout bounds one hook command (omp hooks are expected to be
// fast; a hung hook must not hang the agent).
const DefaultTimeout = 10 * time.Second

// FromSettings builds the bus from a settings `hooks` record
// (map[string][]string or map[string]string — both accepted, YAML-wise).
func FromSettings(raw any) *Bus {
	m, ok := raw.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	b := &Bus{Commands: map[string][]string{}}
	for event, v := range m {
		switch t := v.(type) {
		case string:
			b.Commands[event] = append(b.Commands[event], t)
		case []any:
			for _, item := range t {
				if s, ok := item.(string); ok {
					b.Commands[event] = append(b.Commands[event], s)
				}
			}
		}
	}
	if len(b.Commands) == 0 {
		return nil
	}
	return b
}

// Events lists the configured event names (sorted-ish via map order is
// nondeterministic, so callers sort).
func (b *Bus) Events() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.Commands))
	for e := range b.Commands {
		out = append(out, e)
	}
	return out
}

// runOne executes one command with the payload on stdin and returns its
// parsed stdout (empty map when none).
func runOne(ctx context.Context, cmd string, payload any) (map[string]any, error) {
	cctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	c.Stdin = bytes.NewReader(body)
	c.Stderr = nil // hook stderr goes to the parent's stderr via inherit? No: drop noise.
	var out bytes.Buffer
	c.Stdout = &out
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("hook failed (fail-closed): %w", err)
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return map[string]any{}, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil, fmt.Errorf("hook stdout not JSON: %w", err)
	}
	return parsed, nil
}

// Run executes every command for the event in order. Chain semantics:
// the payload starts as the given map; each command's stdout {"input":…}
// replaces the payload's "input" (last-wins); a {"block":true} stops the
// chain and returns the reason as an error. Used for pre-hooks.
func (b *Bus) Run(ctx context.Context, event string, payload map[string]any) (map[string]any, error) {
	if b == nil {
		return payload, nil
	}
	cmds, ok := b.Commands[event]
	if !ok {
		return payload, nil
	}
	current := payload
	for _, cmd := range cmds {
		res, err := runOne(ctx, cmd, current)
		if err != nil {
			return current, err
		}
		if block, _ := res["block"].(bool); block {
			reason, _ := res["reason"].(string)
			return current, fmt.Errorf("blocked by %s hook: %s", event, reason)
		}
		if repl, ok := res["input"]; ok {
			current["input"] = repl
		}
		if repl, ok := res["text"]; ok {
			current["text"] = repl
		}
		if msg, ok := res["message"]; ok {
			current["message"] = msg
		}
	}
	return current, nil
}

// Notify runs the event ignoring failures (post-hooks: tool_result,
// lifecycle). Failures log, never block.
func (b *Bus) Notify(ctx context.Context, event string, payload any) {
	if b == nil {
		return
	}
	cmds, ok := b.Commands[event]
	if !ok {
		return
	}
	for _, cmd := range cmds {
		if _, err := runOne(ctx, cmd, payload); err != nil {
			logx.Errorf("%s hook: %v", event, err)
		}
	}
}
