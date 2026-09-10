// Package agent implements the xdev agent loop: one goroutine per turn,
// bounded tool worker pool, steering channels, persistence on message_end
// (PRD §3.4).
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
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

// SubagentSystemPromptBase is the child's system prompt: same working
// rules, plus the yield contract that ends the run.
const SubagentSystemPromptBase = `You are xdev's subagent: you run ONE focused task in the user's repository and report back to the agent that spawned you.

Rules:
- Work only inside the current working directory unless given an absolute path elsewhere.
- Never invent file contents; read before editing. Never leave placeholders or stubs.
- Finish by calling the yield tool exactly once, as your last action, with the result the caller asked for.
- Your transcript is not visible to the caller — put everything it needs into the yield result.`

// MaxContextBytes caps the total AGENTS.md content injected into the prompt.
const MaxContextBytes = 32 << 10

// maxImportDepth bounds recursive @path expansion (omp parity: <=5).
const maxImportDepth = 5

// expandImports resolves `@path` references in content: relative to the
// importing file (or cwd for absolute/~ paths), <=5 deep, cycles skipped,
// missing targets left literal. Only line-start or whitespace-preceded
// `@` tokens expand; `user@host` and emails never match.
func expandImports(content, baseDir string, budget *int, seen map[string]bool) string {
	re := regexp.MustCompile(`(^|[\s(])@([\w./~\-]+)`)
	var expand func(string, string, int) string
	expand = func(text, dir string, depth int) string {
		if depth > maxImportDepth {
			return text
		}
		return re.ReplaceAllStringFunc(text, func(m string) string {
			sep := ""
			target := m
			if m[0] != '@' {
				sep, target = string(m[0]), m[1:]
			}
			path := target[1:]
			var full string
			switch {
			case strings.HasPrefix(path, "~/"):
				home, err := os.UserHomeDir()
				if err != nil {
					return m
				}
				full = filepath.Join(home, path[2:])
			case filepath.IsAbs(path):
				full = path
			default:
				full = filepath.Join(dir, path)
			}
			full = filepath.Clean(full)
			if seen[full] || *budget <= 256 {
				return m // cycle or budget exhausted: leave literal
			}
			raw, err := os.ReadFile(full)
			if err != nil {
				return m // missing target: literal per spec
			}
			seen[full] = true
			imp := strings.TrimSpace(string(raw))
			if len(imp) > *budget {
				imp = imp[:*budget] + "\n… [truncated]"
			}
			*budget -= len(imp)
			return sep + "\n<file path=\"" + target + "\">\n" +
				expand(imp, filepath.Dir(full), depth+1) + "\n</file>"
		})
	}
	return expand(content, baseDir, 1)
}

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
			// One choke-point cap: bundled tools are terse by hand, but
			// MCP servers and extensions bring their own documentation,
			// and unbounded remote prose would bust PRD §1 Goal 4 silently.
			fmt.Fprintf(&b, "\n%s: %s\n", d.Name, capToolDescription(d.Description))
		}
	}
	return b.String()
}

// MaxToolDescriptionChars bounds any single tool's prompt prose.
//
// This trims only the redundant `# Tools` recap in the system prompt: the
// authoritative channel is the native tool schema in StreamRequest.Tools
// (Agent.toolDefs → registry Defs, uncapped), so capping prompt text loses
// no semantics. Remote prose is the risk being bounded here. pi ships 8
// built-ins in ~460-510 tokens, so 400 chars/tool keeps a dozen tools well
// inside the <1,000-token goal.
const MaxToolDescriptionChars = 400

// capToolDescription trims a description to the prompt budget on a rune
// boundary (never mid-codepoint) and marks the cut.
func capToolDescription(desc string) string {
	if len(desc) <= MaxToolDescriptionChars {
		return desc
	}
	cut := MaxToolDescriptionChars
	for cut > 0 && !utf8.RuneStart(desc[cut]) {
		cut--
	}
	return desc[:cut] + "…"
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
	seen := map[string]bool{}
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		remaining := MaxContextBytes - total
		content := expandImports(strings.TrimSpace(string(raw)), filepath.Dir(p), &remaining, seen)
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		remaining = MaxContextBytes - total
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
