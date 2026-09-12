// Package hooks implements config-driven shell-command hooks (M11 #12,
// research §4 CORE): an event → command table resolved from settings, from
// discovered hook files (project .xdev/hooks, user <dataDir>/hooks,
// trusted extension dirs) and from --hook flags. Each command runs with the
// event payload as JSON on stdin; a non-zero exit is fail-closed (the action
// is denied). For tool_call pre-hooks the command's stdout may carry
// {"block":true,"reason":"…"} or an {"input":…} replacement; tool_result
// hooks may return {"text":…}. A `context` hook may return {"text":…} to
// replace the system prompt.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/tool"
)

// KnownEvents lists the event names the bus emits, with their emission
// points (research §4 audit). Subscribing to any other name is allowed — it
// simply never fires — but this is the audited surface:
//
//	session_start, before_agent_start, agent_start, agent_end,
//	turn_start, turn_end                internal/agent loop
//	session_compact                      internal/agent compaction (seam:
//	                                     agent.WithCompactionEvent)
//	session_before_switch, session_switch cmd/xdev session swap
//	tool_call, tool_result               the interceptor chain
//	context                              system-prompt assembly (the hook may
//	                                     return {"text": …} to replace it)
//	ttsr_triggered                       the TTSR rules engine (issue #35)
var KnownEvents = []string{
	"session_start", "session_before_switch", "session_switch",
	"before_agent_start", "agent_start", "agent_end", "turn_start", "turn_end",
	"session_compact", "context", "tool_call", "tool_result", "ttsr_triggered",
}

// Hook is one resolved hook command.
type Hook struct {
	// Name keys the merge across sources (settings win over a hook file
	// named after the same event). Empty names never collide.
	Name    string
	Event   string
	Command string
	// Matcher is the optional stage-2 regex, tested against the payload's
	// "tool" field when present (tool events) and against the compact JSON
	// payload otherwise.
	Matcher string
	// If is the optional permission-syntax prefilter, e.g. "Bash(git *)",
	// matched by the approval policy's own matchers (tool.MatchPermissionSpec).
	If string
	// Source records where the hook came from (cli | settings | project |
	// user | extension:<name>) for diagnostics.
	Source string

	re *regexp.Regexp // compiled Matcher; nil = no stage-2 filter
}

// Bus is the resolved hook configuration. nil Bus = no hooks configured;
// every method is nil-safe so agents never special-case hooks.
type Bus struct {
	// Hooks is ordered: within one source, declaration order; across
	// sources, CLI → settings → discovered. Last-wins for input/result
	// overrides (omp semantics); the first block short-circuits.
	Hooks []Hook
}

// DefaultTimeout bounds one hook command (omp hooks are expected to be
// fast; a hung hook must not hang the agent).
const DefaultTimeout = 10 * time.Second

// Options are the hook sources Build resolves, in precedence order: CLI
// (--hook), the settings `hooks` record, then discovered hook files.
type Options struct {
	Settings          any      // settings `hooks` record
	CLI               []string // --hook specs: "event=command" or a hook name
	CWD               string   // project root for .xdev/hooks discovery
	TrustedExtensions []string // extension names whose hooks/ dir may load
}

// Build assembles the bus and returns it with the non-fatal warnings
// (a bad hook file is skipped, never fatal). Settings win on a name
// conflict, then project, user, trusted extension.
func Build(o Options) (*Bus, []string) {
	disc, warns := Discover(o.CWD, o.TrustedExtensions)
	cli, cliWarns := cliHooks(o.CLI, disc)
	warns = append(warns, cliWarns...)

	all := make([]Hook, 0, len(cli)+len(disc)+4)
	all = append(all, cli...)
	all = append(all, settingsHooks(o.Settings)...)
	all = append(all, disc...)

	seen := map[string]bool{}
	out := make([]Hook, 0, len(all))
	for _, h := range all {
		if h.Name != "" && seen[h.Name] {
			continue // earlier source wins
		}
		if err := h.compile(); err != nil {
			warns = append(warns, fmt.Sprintf("hook %q: %v", h.Name, err))
			continue
		}
		if h.Name != "" {
			seen[h.Name] = true
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, warns
	}
	return &Bus{Hooks: out}, warns
}

// FromSettings builds the bus from a settings `hooks` record
// (map[string][]string or map[string]string — both accepted, YAML-wise).
func FromSettings(raw any) *Bus {
	b, _ := Build(Options{Settings: raw})
	return b
}

// settingsHooks parses the settings record. Each settings hook is named
// after its event, which is what makes it win over a discovered hook file
// named after the same event.
func settingsHooks(raw any) []Hook {
	m, ok := raw.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	events := make([]string, 0, len(m))
	for event := range m {
		events = append(events, event)
	}
	slices.Sort(events) // map order is random; declaration order is not recoverable
	var out []Hook
	for _, event := range events {
		var cmds []string
		switch t := m[event].(type) {
		case string:
			cmds = append(cmds, t)
		case []any:
			for _, item := range t {
				if s, ok := item.(string); ok {
					cmds = append(cmds, s)
				}
			}
		}
		for i, cmd := range cmds {
			name := event
			if i > 0 {
				name = fmt.Sprintf("%s#%d", event, i)
			}
			out = append(out, Hook{Name: name, Event: event, Command: cmd, Source: "settings"})
		}
	}
	return out
}

// cliHooks parses --hook specs: "event=command" declares an inline hook;
// a bare word names a discovered hook and pulls it in (the trust switch for
// an extension hook is --trusted-extension, not this flag).
func cliHooks(specs []string, disc []Hook) ([]Hook, []string) {
	var out []Hook
	var warns []string
	for _, raw := range specs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		event, cmd, inline := strings.Cut(s, "=")
		if inline {
			event, cmd = strings.TrimSpace(event), strings.TrimSpace(cmd)
			if event == "" || cmd == "" {
				warns = append(warns, fmt.Sprintf("--hook %q: want event=command", s))
				continue
			}
			if !slices.Contains(KnownEvents, event) {
				warns = append(warns, fmt.Sprintf("--hook %q: unknown event", event))
			}
			out = append(out, Hook{Name: "cli:" + event, Event: event, Command: cmd, Source: "cli"})
			continue
		}
		found := false
		for _, h := range disc {
			if h.Name == s {
				out = append(out, h)
				found = true
			}
		}
		if !found {
			warns = append(warns, fmt.Sprintf("--hook %q: no discovered hook with that name", s))
		}
	}
	return out, warns
}

// compile precompiles the stage-2 matcher so Run costs no regex parse.
func (h *Hook) compile() error {
	if h.Matcher == "" {
		return nil
	}
	re, err := regexp.Compile(h.Matcher)
	if err != nil {
		return fmt.Errorf("matcher: %w", err)
	}
	h.re = re
	return nil
}

// Events lists the configured event names, sorted.
func (b *Bus) Events() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.Hooks))
	for _, h := range b.Hooks {
		if !slices.Contains(out, h.Event) {
			out = append(out, h.Event)
		}
	}
	slices.Sort(out)
	return out
}

// matches is the two-stage filter (research §4): stage 1 is the event name;
// stage 2 is the optional matcher regex, and `if` is a permission-syntax
// prefilter resolved by the approval policy's matchers so hooks and the
// approval UI can never disagree about what "Bash(git *)" means.
func (h Hook) matches(event string, payload map[string]any) bool {
	if h.Event != event {
		return false
	}
	if h.re != nil && !h.re.MatchString(matchSubject(payload)) {
		return false
	}
	if h.If != "" && !ifMatch(h.If, payload) {
		return false
	}
	return true
}

// matchSubject is the string a stage-2 matcher tests: the payload's "tool"
// field when present (tool events), else the payload's compact JSON.
func matchSubject(payload map[string]any) string {
	if tool, ok := payload["tool"].(string); ok && tool != "" {
		return tool
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(b)
}

// ifMatch evaluates an `if` prefilter against a tool payload; a payload
// without a tool name never matches.
func ifMatch(spec string, payload map[string]any) bool {
	name, _ := payload["tool"].(string)
	if name == "" {
		return false
	}
	var args json.RawMessage
	switch v := payload["input"].(type) {
	case json.RawMessage:
		args = v
	case string:
		args = json.RawMessage(v)
	default:
		args, _ = json.Marshal(payload["input"])
	}
	return tool.MatchPermissionSpec(spec, name, args)
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

// Run executes every matching command for the event in order. Chain
// semantics: the payload starts as the given map; each command's stdout
// {"input":…} replaces the payload's "input" (last-wins); a {"block":true}
// stops the chain and returns the reason as an error. Used for pre-hooks.
func (b *Bus) Run(ctx context.Context, event string, payload map[string]any) (map[string]any, error) {
	if b == nil {
		return payload, nil
	}
	current := payload
	for _, h := range b.Hooks {
		if !h.matches(event, current) {
			continue
		}
		res, err := runOne(ctx, h.Command, current)
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

// Notify runs the matching commands for the event ignoring failures
// (post-hooks: tool_result, lifecycle). Failures log, never block.
func (b *Bus) Notify(ctx context.Context, event string, payload any) {
	if b == nil {
		return
	}
	m, _ := payload.(map[string]any)
	for _, h := range b.Hooks {
		if !h.matches(event, m) {
			continue
		}
		if _, err := runOne(ctx, h.Command, payload); err != nil {
			logx.Errorf("%s hook: %v", event, err)
		}
	}
}

// Context applies the `context` event (research §4: a chained prompt
// replacement): a hook returning {"text": …} replaces the system prompt.
// A failing or blocking hook keeps the original prompt — the run never
// proceeds promptless.
func (b *Bus) Context(ctx context.Context, system string) string {
	if b == nil {
		return system
	}
	res, err := b.Run(ctx, "context", map[string]any{"text": system, "system": system})
	if err != nil {
		logx.Errorf("context hook: %v", err)
		return system
	}
	if s, ok := res["text"].(string); ok && s != "" {
		return s
	}
	return system
}
