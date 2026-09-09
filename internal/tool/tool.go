// Package tool implements xdev's four core tools (read, write, edit, bash),
// the OutputSink, env hardening, and the approval tier model (PRD §3.6).
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Result is what a tool returns to the model and session.
type Result struct {
	// Text is the primary text payload (already sink-windowed for bash).
	Text string
	// Details is tool-specific structured metadata persisted in the
	// toolResult message's "details" field (e.g. resolvedPath, exitCode).
	Details any
	// IsError marks a failed execution; Text should explain the failure.
	IsError bool
}

// Tool is one executable tool.
type Tool interface {
	// Name is the tool name the model calls ("read", "write", "edit", "bash").
	Name() string
	// Description feeds the system prompt (keep short; <1000-token budget).
	Description() string
	// Parameters is the JSON Schema object for arguments.
	Parameters() json.RawMessage
	// Execute runs the tool. ctx carries cancellation (agent abort → ctx
	// cancel → tools must return promptly). Implementations return a
	// Result with IsError=true on failure instead of a Go error unless the
	// failure is harness-level (e.g. malformed args JSON).
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Registry holds tools by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds t; panics on duplicate names (programming error).
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name()]; dup {
		panic(fmt.Sprintf("tool: duplicate tool %q", t.Name()))
	}
	r.tools[t.Name()] = t
}

// Get returns the tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Defs returns ai.ToolDef descriptors for every registered tool, ordered by
// name for deterministic prompts.
func (r *Registry) Defs() []NamedDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]NamedDef, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		out = append(out, NamedDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return out
}

// NamedDef is a registry entry projected for the prompt.
type NamedDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}
