// Package agent implements the xdev agent loop: one goroutine per turn,
// bounded tool worker pool, steering channels, persistence on message_end
// (PRD §3.4).
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SystemPromptBase is the <1000-token system prompt (pi philosophy: minimal;
// frontier models are RL-trained to understand coding agents).
const SystemPromptBase = `You are xdev, a coding agent working in the user's repository.

Rules:
- Work only inside the current working directory unless given an absolute path elsewhere.
- Prefer minimal, surgical edits; keep the codebase boring and consistent with its conventions.
- Verify changes: run the relevant build/test command before claiming success.
- Never invent file contents; read before editing. Never leave placeholders or stubs.
- If blocked by missing information you cannot obtain with tools, say so plainly.`

// MaxContextBytes caps the total AGENTS.md content injected into the prompt.
const MaxContextBytes = 32 << 10

// BuildSystemPrompt assembles the system prompt: base + project context
// files + tool descriptions. Tool descriptions come last (they are part of
// the <1000-token budget).
func BuildSystemPrompt(base string, contextFiles string, defs []NamedToolDef) string {
	var b strings.Builder
	b.WriteString(base)
	if contextFiles != "" {
		b.WriteString("\n\n# Project context\n")
		b.WriteString(contextFiles)
	}
	if len(defs) > 0 {
		b.WriteString("\n\n# Tools\n")
		for _, d := range defs {
			fmt.Fprintf(&b, "\n%s: %s\n", d.Name, d.Description)
		}
	}
	return b.String()
}

// NamedToolDef mirrors tool.NamedDef to avoid an import cycle; the CLI
// adapter converts.
type NamedToolDef struct {
	Name        string
	Description string
}

// LoadContextFiles collects the AGENTS.md hierarchy: global
// (~/.xdev/agent/AGENTS.md) first, then root→cwd. CLAUDE.md is loaded as an
// alias only where AGENTS.md is absent (M10 formalizes imports; MVP loads
// whole files, capped).
func LoadContextFiles(cwd string) string {
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".xdev", "agent", "AGENTS.md"))
	}
	// Walk from root to cwd, collecting AGENTS.md (or CLAUDE.md fallback).
	var chain []string
	dir := cwd
	for {
		p := filepath.Join(dir, "AGENTS.md")
		if _, err := os.Stat(p); err != nil {
			p = filepath.Join(dir, "CLAUDE.md")
			if _, err := os.Stat(p); err != nil {
				p = ""
			}
		}
		if p != "" {
			chain = append(chain, p)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// chain is cwd→root; reverse for root→cwd ordering.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	files = append(files, chain...)

	var b strings.Builder
	total := 0
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(raw))
		if content == "" {
			continue
		}
		remaining := MaxContextBytes - total
		if remaining < 256 {
			break // no useful budget left; skip further files entirely
		}
		if len(content) > remaining {
			content = string([]rune(content)[:len([]rune(strings.TrimSpace(content[:remaining])))]) + "\n… [truncated]"
		}
		if b.Len() > 0 {
			b.WriteString("\n\n---\n\n")
		}
		if !strings.HasPrefix(p, cwd) {
			fmt.Fprintf(&b, "## Global conventions (%s)\n\n", p)
		} else {
			fmt.Fprintf(&b, "## %s\n\n", p)
		}
		b.WriteString(content)
		total += len(content)
	}
	return b.String()
}
