package tool

import (
	"strings"
	"unicode/utf8"
)

// Session-effort deferral (feat/session-effort): one table decides which
// registered tools leave the eager schema. lean keeps the pi core + the
// discovery bridge; standard is today's hard-coded deferred set; full only
// defers the rare long tail. ApplySessionEffort is idempotent and safe to
// call mid-session after /effort flips the rung — it rebuilds the deferred
// set from scratch so a previous rung cannot leak tools in or out.

// sessionEffortLeanEager never defers on the lean rung. Bridge tools are
// always eager (Catalog.Defer panics on them); they are listed here so a
// Names() walk does not try to index them as "everything else".
var sessionEffortLeanEager = map[string]bool{
	"read": true, "write": true, "edit": true, "bash": true,
	"grep": true, "glob": true,
	"ask": true, "todo": true,
	"propose":      true, // plan-mode exit must stay callable without tool_search
	ToolSearchName: true, ToolDescribeName: true, ToolCallName: true,
}

// sessionEffortStandardDeferred is the set newToolRegistry deferred before
// session effort existed — keep byte-stable for the default rung.
var sessionEffortStandardDeferred = []struct {
	name  string
	index string
	tags  []string
}{
	{"ast_grep", "structural code search with ast-grep patterns", []string{"search", "code"}},
	{"ast_edit", "AST-aware codemod rewrites", []string{"edit", "codemod", "code"}},
	{"github", "GitHub operations: PRs, issues, files, search, Actions", []string{"git", "pr", "remote"}},
	{"hub", "message and inspect the subagents running in this session", []string{"subagent", "agent"}},
	{"send_message", "send a message to another xdev session (mailbox)", []string{"mailbox", "agent"}},
	{"inbox", "read messages other sessions sent this one (mailbox)", []string{"mailbox", "agent"}},
	{CheckpointToolName, "bookmark the session tree at this point", []string{"session", "rewind"}},
	{RewindToolName, "return the session to an earlier checkpoint", []string{"session", "rewind"}},
}

// sessionEffortFullDeferred is the rare long tail that stays behind the
// bridge even when the session is opened wide — desktop control, speech,
// image gen and the schedule surface are demand tools, not the coding path.
var sessionEffortFullDeferred = []struct {
	name  string
	index string
	tags  []string
}{
	{"computer", "Control the desktop: screenshot, click, type, key, scroll", []string{"desktop", "ui"}},
	{"tts", "Speak text aloud through the local synthesizer", []string{"audio", "speech"}},
	{"generate_image", "generate an image from a text prompt", []string{"image", "media"}},
	{"security_scan", "run installed security scanners over a path", []string{"security", "scan"}},
	{"schedule_create", "create a session-local reminder", []string{"schedule"}},
	{"schedule_list", "list active reminders in the current session", []string{"schedule"}},
	{"schedule_delete", "delete an active reminder by id", []string{"schedule"}},
}

// ApplySessionEffort rebuilds the deferred catalog for one effort rung.
// Unknown effort folds to standard. Tools not registered are skipped — a
// --no-lsp build must not panic on a missing name.
func ApplySessionEffort(r *Registry, effort string) {
	if r == nil {
		return
	}
	r.ResetDeferred()
	switch normalizeEffort(effort) {
	case "lean":
		for _, name := range r.Names() {
			if sessionEffortLeanEager[name] || isBridgeTool(name) {
				continue
			}
			r.Defer(name, indexFor(r, name), "effort", "lean")
		}
	case "full":
		for _, d := range sessionEffortFullDeferred {
			if _, ok := r.Get(d.name); !ok {
				continue
			}
			r.Defer(d.name, d.index, d.tags...)
		}
	default: // standard
		for _, d := range sessionEffortStandardDeferred {
			if _, ok := r.Get(d.name); !ok {
				continue
			}
			r.Defer(d.name, d.index, d.tags...)
		}
	}
}

func normalizeEffort(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "lean", "simple", "min", "minimal":
		return "lean"
	case "full", "max", "omp", "complex":
		return "full"
	default:
		return "standard"
	}
}

// indexFor prefers a known standard/full blurb, else the tool's first line.
func indexFor(r *Registry, name string) string {
	for _, d := range sessionEffortStandardDeferred {
		if d.name == name {
			return d.index
		}
	}
	for _, d := range sessionEffortFullDeferred {
		if d.name == name {
			return d.index
		}
	}
	t, ok := r.Get(name)
	if !ok {
		return name
	}
	desc := strings.TrimSpace(t.Description())
	if i := strings.IndexByte(desc, '\n'); i >= 0 {
		desc = strings.TrimSpace(desc[:i])
	}
	if desc == "" {
		return name
	}
	const max = 120
	if utf8.RuneCountInString(desc) > max {
		runes := []rune(desc)
		desc = string(runes[:max-1]) + "…"
	}
	return desc
}
