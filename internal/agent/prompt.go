// Package agent implements the xdev agent loop: one goroutine per turn,
// bounded tool worker pool, steering channels, persistence on message_end
// (PRD §3.4).
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/rules"
	"github.com/FreePeak/xdev/internal/tool"
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

// SystemPromptOverrides discovers SYSTEM.md/APPEND_SYSTEM.md/
// PERSONALITY.md/TITLE_SYSTEM.md from the project directory first, then
// the user data dir. No ancestor walk — unlike AGENTS.md, system prompts
// are workspace-scoped by design. All fields return "" when no override
// exists.
type SystemPromptOverrides struct {
	System, Append string
	Personality    string
	// Title is TITLE_SYSTEM.md content: the system prompt for ai-title
	// generation (omp parity, M10 #32). xdev titles are purely mechanical
	// today — openSession in cmd/xdev/print.go stamps "print <timestamp>"
	// and /fork derives from the parent — so nothing consumes this yet.
	// When the ai-title call lands it reads its prompt override here
	// (fallback: the built-in title prompt) and normalizes the model's
	// answer: first line, strip quotes/<title>/punctuation, "none" or
	// "<title/>" = no title, >80 chars or >12 words rejected.
	Title string
}

// TitleSystemPrompt returns the TITLE_SYSTEM.md content for ai-title
// generation, "" when absent (the caller falls back to its default
// prompt). No ai-title call exists yet — see the Title field comment
// for where this plugs in.
func (o SystemPromptOverrides) TitleSystemPrompt() string { return o.Title }

// LoadSystemPromptOverrides checks project SYSTEM.md/APPEND_SYSTEM.md/
// PERSONALITY.md/TITLE_SYSTEM.md then their user-level counterparts.
func LoadSystemPromptOverrides(cwd string) SystemPromptOverrides {
	var o SystemPromptOverrides
	o.System = findSystemPromptFile(cwd, "SYSTEM.md")
	o.Append = findSystemPromptFile(cwd, "APPEND_SYSTEM.md")
	o.Personality = findSystemPromptFile(cwd, "PERSONALITY.md")
	o.Title = findSystemPromptFile(cwd, "TITLE_SYSTEM.md")
	return o
}

// findSystemPromptFile: project-first, then user data dir.
func findSystemPromptFile(cwd, name string) string {
	for _, dir := range []string{cwd, userAgentDir()} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if raw, err := os.ReadFile(p); err == nil && len(raw) > 0 {
			return strings.TrimSpace(string(raw))
		}
	}
	return ""
}

func userAgentDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".xdev", "agent")
	}
	return ""
}

// PersonalityPresets maps the `personality` settings key / --personality
// flag to the prompt-tail paragraph it injects (condensed from omp's
// PERSONALITY_SPECS). "none" injects nothing; "" is the unset flag value
// and behaves like "none" (the real path defaults it from
// settings.personality first). A discovered PERSONALITY.md always beats
// the preset.
var PersonalityPresets = map[string]string{
	"":          "",
	"default":   "Tone: evidence-first terse engineer. Every sentence carries a fact, decision, or risk; no filler, hedging, or narration of obvious steps. Conclusion first, evidence next; name the tradeoff and pick the boring, safe option. Push back on risk-hidden plans or wrong claims with evidence; if overruled, execute the user's call.",
	"friendly":  "Tone: warm, supportive collaborator. Adjust depth and pacing to the user, invite input, and keep momentum and confidence up. Basic questions must feel safe — never curt, dismissive, or patronizing. When something looks wrong, acknowledge the valid points first, then explain the concern. Assume a technical reader; warmth never means dumbing down.",
	"pragmatic": "Tone: pragmatic senior engineer — concise, respectful, task-focused; actionable guidance first (assumptions, prerequisites, next steps). Engineering quality is non-negotiable and reasoning must be defensible, but never cheerlead or pad explanations of your own work. Challenge weak assumptions with demonstrable reasoning, then work with the user's call.",
	"none":      "",
}

// ValidPersonality reports whether v is a personality preset value
// (default|friendly|pragmatic|none). Settings-layer and flag validation
// share this enum.
func ValidPersonality(v string) bool {
	_, ok := PersonalityPresets[v]
	return ok
}

// ApplyPersonalityPreset resolves the effective personality tail into
// the overrides: a discovered PERSONALITY.md (o.Personality already
// set) wins; otherwise the preset paragraph fills it; "none" and ""
// leave the tail empty. Unknown presets are an error — a typo'd flag
// must not silently run without its persona.
func (o *SystemPromptOverrides) ApplyPersonalityPreset(preset string) error {
	if !ValidPersonality(preset) {
		return fmt.Errorf("unknown personality %q (want default|friendly|pragmatic|none)", preset)
	}
	if o.Personality == "" {
		o.Personality = PersonalityPresets[preset]
	}
	return nil
}

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
		// Section-level budget on top of the per-tool cap: with a dozen
		//-plus bundled tools the recap itself would bust PRD §1 Goal 4,
		// so once the budget is spent later tools are listed name-only.
		// The authoritative channel is the native tool schema in
		// StreamRequest.Tools (Agent.toolDefs → registry Defs, uncapped).
		budget := MaxToolRecapChars
		for _, d := range defs {
			line := d.Name + ": " + capToolDescription(d.Description)
			if budget < len(line) {
				fmt.Fprintf(&b, "\n%s\n", d.Name)
				continue
			}
			budget -= len(line)
			fmt.Fprintf(&b, "\n%s\n", line)
		}
	}
	return b.String()
}

// BuildDeferredIndex renders the one-line index of the tools a catalog keeps
// out of the eager tool schema (M13 #54): one `name: summary` line each, plus
// the bridge entry point. It is appended to the prompt next to the `# Tools`
// recap, which is where a model looks for capability; empty when nothing is
// deferred, so a catalog-free prompt stays byte-identical.
func BuildDeferredIndex(entries []tool.Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Deferred tools\n")
	b.WriteString("Not in the tool list. Call tool_search to find one, tool_describe <name> for its schema, tool_call {\"name\":<name>,\"args\":{...}} to run it.\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "\n%s: %s\n", e.Name, e.Index)
	}
	return b.String()
}

// MaxToolRecapChars bounds the whole `# Tools` recap. Bundled base prose
// plus this section must stay inside the <1,000-token system-prompt goal
// (PRD §1 Goal 4); tools past the budget are listed name-only.
const MaxToolRecapChars = 1200

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

// Per-rule and total prompt caps for injected rules (issue #31): a
// single rule renders at most MaxRuleBytes; the whole block at most
// MaxRulesBytes, dropping lowest-priority rules first.
const (
	MaxRuleBytes  = 8 << 10
	MaxRulesBytes = 32 << 10
)

// BuildRulesBlock renders the discovered rulebook into the system-prompt
// tail next to the project-context block. Always-apply and glob-less
// rules render in full, priority-first; glob-scoped rules are listed
// with their edit/write shorthand only — their bodies stay reachable
// via rule://<name>, keeping the static prompt cheap while the path
// gating decision happens at edit/write time (rules.ForPath).
func BuildRulesBlock(rs []rules.Rule) string {
	if len(rs) == 0 {
		return ""
	}
	sorted := append([]rules.Rule(nil), rs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority > sorted[j].Priority
		}
		return sorted[i].Name < sorted[j].Name
	})
	var full []string // priority-desc rendered rules subject to the drop loop
	for _, r := range sorted {
		if !r.AlwaysApply && len(r.Globs) > 0 {
			continue // conditional: shorthand listing below, not full text
		}
		content := r.Content
		if len(content) > MaxRuleBytes {
			content = string([]rune(content[:MaxRuleBytes])) + "\n… [truncated]"
		}
		full = append(full, fmt.Sprintf("## %s\n\n%s", r.Name, strings.TrimRight(content, "\n")))
	}
	var gated []string
	for _, r := range sorted {
		if sh := r.Shorthand(); sh != "" {
			entry := "- rule://" + r.Name + " — " + sh
			if r.Description != "" {
				entry += " — " + r.Description
			}
			gated = append(gated, entry)
		}
	}
	render := func(blocks []string) string {
		var b strings.Builder
		b.WriteString("# Rules\n")
		for _, blk := range blocks {
			b.WriteString("\n\n")
			b.WriteString(blk)
		}
		if len(gated) > 0 {
			b.WriteString("\n\nConditional rules — they gate specific paths; read the body with rule://<name> before editing a matching file:\n")
			for _, g := range gated {
				b.WriteString("\n" + g)
			}
		}
		return b.String()
	}
	// Overflow drops the lowest-priority rendered rule first (sorted
	// priority-desc, so the tail goes).
	out := render(full)
	for len(full) > 0 && len(out) > MaxRulesBytes {
		full = full[:len(full)-1]
		out = render(full)
	}
	return out
}
