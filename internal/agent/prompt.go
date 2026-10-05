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

// SystemPromptBase is the base system prompt. It stays inside PRD §1 Goal 4's
// <1,000-token budget (guarded by maxPromptTokens in cmd/xdev/prompt_test.go),
// but the earlier 6-line version was smaller than that budget on purpose —
// "pi philosophy: minimal, frontier models are RL-trained to understand coding
// agents" — and the measurement in
// docs/research/2026-09-15-xdev-slow-session-rca.md disproved the assumption it
// rests on. Two costs, both measured against the omp baseline:
//
//   - the single "use read/grep, not cat/sed" line was ignored: bash:read ran
//     15:1 against omp's 3.9:1, so whole files arrived through the shell and
//     the context window paid for them;
//   - nothing at all said when to delegate, so a harness with a working
//     fan-out engine (maxBatchParallel=8, five agent definitions, depth-2
//     nesting) was driven one serial call at a time.
//
// The rules below are the measured minimum, not a philosophy: each one names a
// behaviour the session store shows going wrong. They are deliberately short —
// a prompt nobody reads is a prompt that does not steer. Two budgets guard this
// constant and both must stay true: TestBasePromptStaysCompact (2000 runes
// here) and TestBundledPromptStaysUnderBudget (the whole assembled prompt at
// 1000 tokens, which shipped at 993) — so a new rule REPLACES prose here, it
// never appends.
//
// The last two rules under "Before you act" are one decision, not two.
// "Confirm the requirement" alone (#447) only stopped the agent acting on a
// request it had misread; it never said what a misread looks like. A question —
// "why is this slow?", "deep dive to find the root cause" — was still answered
// by reading the codebase and then starting to implement, because nothing said
// the report IS the deliverable. Over 355 keep-going nudges in this repo's own
// session store, 300 fired after the run had already mutated the workspace and
// 55 after one that had only read, so half the fix belongs in the loop
// (PromptContinuation, loop.go), not here.
const SystemPromptBase = `You are xdev, a coding agent working in the user's repository.

Rules:
- Work only inside the current working directory unless given an absolute path elsewhere.
- Prefer minimal, surgical edits; keep the codebase boring and consistent with its conventions.
- Verify changes: run the relevant build/test command before claiming success.
- Never invent file contents; read before editing. Never leave placeholders or stubs.
- If blocked by missing information you cannot obtain with tools, say so plainly.

Before you act:
- Confirm the requirement, don't assume it. Restate what you will do in a sentence or two, then
  start. "Find the root cause and fix" asks for a change; "why is this slow?" asks a question.
- A question gets an answer: if the ask is why/what/how, or research/explore and report, do
  the reading, give the report and stop. Don't start implementing.
- Ask first when the request is ambiguous, non-trivial, spans repos, or would touch files outside
  the working directory. Use ask with the readings you actually have. A clear, explicit
  instruction to do X is its own confirmation — asking anyway is noise.
- Confirm once, before the first change. That covers the whole run: after go, keep going.

Getting code into context:
- Search first, then read: grep and glob to find candidates, read only the ranges you need.
- Use read, not the shell, for file content. read pages the range you ask for; cat, head, sed
  and pipes dump whole files and burn the context window.
- Keep bash for work that actually runs: build, test, git, package managers.

Delegating:
- task runs a subagent in its own session and returns one result; its transcript never reaches
  you. Worth it when the answer means reading a lot of code, not for one read or command.
- Independent work is parallel: send slices in one batch.
- Say what the result must contain. You see that, never the work behind it.`

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
//
// The cap was written as a single number, but the budget it actually has to
// protect is the *system prompt*, and the two are not the same thing. Measured
// on a real repository (PRD entry 2026-09-30): the injected chain, the
// discovered rulebook and the skills block together run ~6.8k tokens against
// PRD §1 Goal 4's 1,000, so a 32 KiB chain alone is most of the blowout.
//
// The per-file split exists because a single shared budget silently starves
// the file that matters most. With one 98 KB global file the chain rendered
// 32,871 bytes and the repository's own AGENTS.md never appeared at all — no
// error, no marker, just missing rules. Every file now gets a bounded share
// instead of competing for one pool, so no single file can consume the chain
// budget that the other files were counting on.
const (
	MaxContextBytes = 32 << 10 // whole chain, unchanged as the outer bound
	// MaxContextFileKB bounds one ANCESTOR file. The chain is loaded
	// root→cwd, so an uncapped ancestor can consume the whole pool and the
	// closest file is never even read — the load pass below stops early, and
	// the file that carries the rules saying which ancestor rules do not
	// apply is simply absent. Bounding every ancestor is what guarantees the
	// closest file is always loaded; the closest file itself is bounded by
	// the pool (#477's floor), not by a share, so a large project ruleset
	// still reaches the model.
	MaxContextFileKB = 8 << 10
)

// contextBytesForFile returns the slice of the chain budget one file may
// render, given how much earlier files already took and whether this is the
// cwd's own file.
func contextBytesForFile(used, fileSize int, closest bool) int {
	share := MaxContextFileKB
	if closest {
		share = MaxContextBytes
	}
	// Never render more of a file than it actually contains.
	if fileSize < share {
		share = fileSize
	}
	if left := MaxContextBytes - used; share > left {
		share = left
	}
	return share
}

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

// ProjectContextHeader precedes injected AGENTS.md content in the system
// prompt. The bare "# Project context" heading made the block read as
// optional background, so the model discounted it: in a 42-run head-to-head
// xdev satisfied rules that exist only in AGENTS.md in 2 of 3 runs, and the
// one run that missed a rule was the one run that never opened the file.
//
// The wording states what the block is (this repository's binding rules) and
// bounds it correctly (a direct user instruction still wins). It does not
// claim more than the file has: a repository rule and a system rule are
// different things, and saying so keeps the hierarchy honest instead of
// merely louder.
const ProjectContextHeader = `# Project rules and conventions

These are the binding rules for this repository, loaded automatically. They
apply to the work in this session whether or not the task restates them: a
task prompt that omits a rule below has not cancelled it. Follow them without
re-reading this file, and read the file itself when you need detail a summary
would lose. Direct user instructions for this task still take precedence.`

// ProjectContextBlock frames injected context files with ProjectContextHeader.
// Both injection sites (the parent prompt and a subagent's) call this, so the
// framing cannot drift apart between them.
func ProjectContextBlock(contextFiles string) string {
	if contextFiles == "" {
		return ""
	}
	return ProjectContextHeader + "\n\n" + contextFiles
}

// BuildSystemPrompt assembles the system prompt: base + project context
// files + tool descriptions. Tool descriptions come last (they are part of
// the <1000-token budget).
func BuildSystemPrompt(base string, contextFiles string, defs []NamedToolDef) string {
	var b strings.Builder
	b.WriteString(base)
	if contextFiles != "" {
		b.WriteString("\n\n")
		b.WriteString(ProjectContextBlock(contextFiles))
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

	// Two passes, because the chain runs root→cwd and precedence runs the
	// other way: the CWD's own rules override its ancestors, so when the byte
	// budget cannot hold everything it must cost the broadest file, not the
	// closest one. The pre-fix loop walked in order and `break`ed when the
	// budget ran out, which dropped the tail — the most specific rules — and
	// kept every generic rule above them, so an overflowing chain injected
	// the least relevant content it had.
	loaded := make([]contextFile, 0, len(files))
	used := 0
	seen := map[string]bool{}
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// Every file in the chain is read, however full the pool already is.
		// The pre-merge loop stopped as soon as MaxContextBytes was spent, so
		// an ancestor that filled the budget meant the CWD's own file was
		// never read at all — and the fit pass below, whose whole job is to
		// prefer the most specific rules, had nothing to prefer. Measured on
		// #477 alone: a 31 KB global file rendered 32,823 bytes with the
		// repository's rule absent and the budget marker NOT rendered, so the
		// loss was silent. The budget is spent by the fit pass, which knows
		// the cost of each file, not by the order files happen to be read in.
		//
		// The per-file cap here is only a load guard — it stops one huge file
		// from being held whole in memory. It is deliberately the whole pool,
		// because ancestors are never *rendered* truncated: the fit pass keeps
		// an ancestor whole or drops it entirely, and a half-present broad
		// rulebook would quietly contradict the specific file that overrides
		// it.
		remaining := MaxContextBytes
		content := strings.TrimSpace(expandImports(strings.TrimSpace(string(raw)), filepath.Dir(p), &remaining, seen))
		if content == "" {
			continue
		}
		loaded = append(loaded, contextFile{path: p, content: content})
		used += len(content)
	}

	// Fit pass, most specific first: keep a file only if it still fits what
	// the more specific files already claimed. A skipped ancestor does not
	// end the walk — a small root file that fits is worth more than nothing.
	keep := make([]bool, len(loaded))
	kept := 0
	for i := len(loaded) - 1; i >= 0; i-- {
		if kept+len(loaded[i].content) > MaxContextBytes {
			continue
		}
		keep[i] = true
		kept += len(loaded[i].content)
	}
	// The closest file is the floor: if it alone overruns the budget it is
	// still injected, truncated, because dropping it would leave the session
	// holding only the ancestors' rules — the same inversion one level down.
	// How much it keeps is settled after rendering, because the budget covers
	// the whole block (marker, headings and separators included) and those
	// are not known until the marker text is.
	truncated := false
	if n := len(loaded); n > 0 && !keep[n-1] {
		keep[n-1] = true
		truncated = true
	}

	render := func() string {
		var b strings.Builder
		// The marker is the contract. The block is headed as the
		// repository's binding rules, and a silently-shortened list would
		// make that a lie the model cannot detect; naming the dropped paths
		// turns an invisible gap into a `read` it can close.
		var omitted []string
		for i, f := range loaded {
			if !keep[i] {
				omitted = append(omitted, f.path)
			}
		}
		if len(omitted) > 0 || truncated {
			b.WriteString("## Rules budget reached\n\n")
			if truncated {
				fmt.Fprintf(&b, "The closest rules file is over the %d-byte budget and is truncated below. ", MaxContextBytes)
			}
			if len(omitted) > 0 {
				fmt.Fprintf(&b, "%d of %d rules files were dropped to stay within it: %s. ",
					len(omitted), len(loaded), strings.Join(omitted, ", "))
			}
			b.WriteString("The most specific rules present are kept in full and broader ancestors were dropped first, so this list is incomplete — read an omitted path before relying on rules it may carry.\n\n")
		}
		// A file cut by the per-file cap is a partial list, and the block is
		// headed as binding rules: a silent shortening under that heading is
		// the lie the marker exists to prevent. Name the file and the size of
		// the cut so the agent can go read what it was not shown.
		for i, f := range loaded {
			if keep[i] && f.dropped > 0 {
				fmt.Fprintf(&b, "[%s: %d characters were left out by the %d KiB per-file cap — read the file if this part matters]\n\n", f.path, f.dropped, MaxContextFileKB>>10)
			}
		}
		rendered := false
		for i, f := range loaded {
			if !keep[i] {
				continue
			}
			if rendered {
				b.WriteString("\n\n---\n\n")
			}
			if !strings.HasPrefix(f.path, cwd) {
				fmt.Fprintf(&b, "## Global conventions (%s)\n\n", f.path)
			} else {
				fmt.Fprintf(&b, "## %s\n\n", f.path)
			}
			b.WriteString(f.content)
			rendered = true
		}
		return b.String()
	}
	out := render()
	// One correction pass, whenever the block carries a per-file cut: the
	// per-file cap bounds one file, not the block, so a chain of large files
	// can still overrun the whole budget. Trim the closest file until it fits
	// — a rules block that ignores its own budget is worse than no budget.
	// The cut is recorded on the file, so the marker re-renders with the
	// final withheld count instead of going stale.
	if over := len(out) - MaxContextBytes; over > 0 && len(loaded) > 0 {
		last := &loaded[len(loaded)-1]
		if !keep[len(loaded)-1] {
			// #477's floor: the closest file is injected truncated, never
			// dropped, so a chain that overflows the pool still delivers it.
			keep[len(loaded)-1] = true
			truncated = true
		}
		last.content = trimToBytes(last.content, len(last.content)-over)
		last.dropped = over
		out = render()
	}
	return out
}

// MinContextFileBytes is the budget below which expanding another rules file
// is pointless: a sliver of a file carries no instruction the model can act
// on, and a half-sentence rule is worse than a missing one.
const MinContextFileBytes = 256

// contextFile is one rules file rendered into the prompt. dropped is the
// number of runes the per-file cap withheld; it is what makes the cut
// visible in the render pass instead of silent.
type contextFile struct {
	path    string
	content string
	dropped int
}

// trimToBytes cuts s to at most n bytes without splitting a rune.
func trimToBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " \t\n") + "\n… [truncated]"
}

// trimToLineBoundary cuts s to about n runes without splitting a rune, then
// backs up to the last newline so the rendered rule is never a half sentence
// or a half markdown construct. Unlike trimToBytes it appends nothing: the
// per-file marker in the render pass is what declares the cut.
func trimToLineBoundary(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if nl := strings.LastIndex(cut, "\n"); nl > 0 {
		cut = cut[:nl]
	}
	return strings.TrimRight(cut, " \t\n")
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
