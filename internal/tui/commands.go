package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Command is one slash command: /Name, /Alias... — Fn runs at input-submit
// time, before a user message is created, so dispatched commands never
// reach the model.
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Fn          func(app CommandAPI, args string) error
}

// SessionOps holds the session lifecycle operations. The session store
// lives in cmd, so it wires these; nil entries degrade the commands to
// notices instead of new store machinery in the TUI.
type SessionOps struct {
	New, Clear, Drop func() error
	Fork             func() error
	Dump             func() (string, error)
	Resume           func(query string) error
	// SummarizeAndBranch appends a branch_summary entry for the
	// abandoned branch, then moves the leaf to entryID (tree selector
	// Shift+Enter). nil degrades to a notice.
	SummarizeAndBranch func(entryID string) error
}

// ModelOps wires the /model command to the live provider state (lives in
// cmd). Current reports the active ref; List returns the available refs;
// Set switches the active model for subsequent turns. nil ops degrade the
// command to a notice.
type ModelOps struct {
	Current func() string
	List    func() []string
	Set     func(ref string) error
}

// SettingsOps wires the /settings command to the config layer (lives in
// cmd). List renders the resolved settings; SetThinking persists the
// showThinking key to the global layer. nil ops degrade the command to a
// notice.
type SettingsOps struct {
	Path        string
	List        func() []string
	SetThinking func(on bool) error
}

// PlanOps wires the /plan command to the live plan-mode state (lives in
// cmd). Get reports the current state; Set toggles it. nil ops degrade
// the command to a notice.
type PlanOps struct {
	Get func() bool
	Set func(on bool) error
}

// PrewalkOps wires the /prewalk command (the live target lives in cmd).
type PrewalkOps struct {
	Enabled func() bool
	Status  func() string
	Set     func(on bool, into string) error
}

// ThemeOps wires the /theme command (theme resolution lives in cmd).
type ThemeOps struct {
	Current func() string
	List    func() []string
	Set     func(name string) error
}

// MemoryOps wires the /memory command (backend lives in cmd).
type MemoryOps struct {
	View  func() string
	Stats func() string
	Clear func() error
}

// AdvisorOps wires the /advisor command (state lives in cmd).
type AdvisorOps struct {
	Enabled func() bool
	Set     func(on bool) error
	Status  func() string
	Dump    func() string
}

// ExtensionCommand runs one "/ext:cmd" and returns its output. Wired by
// cmd from the extension manager; nil when no extensions are loaded.
type ExtensionCommand func(name, args string) (string, error)

// CommandAPI is the app surface commands need. All methods are safe to call
// from the key thread (same mutators the UI already uses).
type CommandAPI interface {
	NewSession() error
	ClearSession() error
	DropSession() error
	RunExtensionCommand(name, args string) (string, error)
	OpenTreeSelector()
	BranchSession(args string) error
	KeyMap() *KeyMap
	ForkSession() error
	DumpSession() error
	ResumeSession(query string) error
	SwitchModel(args string) error
	PlanMode(args string) error
	Advisor(args string) error
	Memory(args string) error
	Theme(args string) error
	Prewalk(args string) error
	SettingsView(args string) error
	AddSystemBlock(text string)
	SendPrompt(text string)
	CommandDir() string
	Quit()
}

// builtinCommands is the built-in registry. A function, not a package var:
// the /help closure references the registry itself, which would make a var
// an initialization cycle.
func builtinCommands() []Command {
	return []Command{
		{Name: "new", Aliases: []string{"fresh"}, Description: "start a fresh session",
			Fn: func(app CommandAPI, args string) error { return app.NewSession() }},
		{Name: "clear", Description: "reset context in place (history kept on disk)",
			Fn: func(app CommandAPI, args string) error { return app.ClearSession() }},
		{Name: "drop", Description: "delete the session file and start fresh",
			Fn: func(app CommandAPI, args string) error { return app.DropSession() }},
		{Name: "fork", Description: "branch the session into a new file",
			Fn: func(app CommandAPI, args string) error { return app.ForkSession() }},
		{Name: "tree", Description: "open the session tree navigator",
			Fn: func(app CommandAPI, args string) error { app.OpenTreeSelector(); return nil }},
		{Name: "branch", Description: "switch to an entry by id prefix",
			Fn: func(app CommandAPI, args string) error { return app.BranchSession(args) }},
		{Name: "dump", Description: "export the transcript to markdown",
			Fn: func(app CommandAPI, args string) error { return app.DumpSession() }},
		{Name: "resume", Description: "resume a session by id prefix",
			Fn: func(app CommandAPI, args string) error { return app.ResumeSession(args) }},
		{Name: "model", Description: "show or switch the active model",
			Fn: func(app CommandAPI, args string) error { return app.SwitchModel(args) }},
		{Name: "settings", Description: "show settings; toggle showThinking on|off",
			Fn: func(app CommandAPI, args string) error { return app.SettingsView(args) }},
		{Name: "prewalk", Description: "one-shot model handoff: /prewalk [on|off|into <ref>] (default @smol)",
			Fn: func(app CommandAPI, args string) error { return app.Prewalk(args) }},
		{Name: "theme", Description: "show or switch the theme: /theme <name>",
			Fn: func(app CommandAPI, args string) error { return app.Theme(args) }},
		{Name: "memory", Description: "long-term memory: /memory view|stats|clear",
			Fn: func(app CommandAPI, args string) error { return app.Memory(args) }},
		{Name: "advisor", Description: "background reviewer: /advisor on|off|status|dump",
			Fn: func(app CommandAPI, args string) error { return app.Advisor(args) }},
		{Name: "plan", Description: "toggle plan mode (read-only research, propose to exit)",
			Fn: func(app CommandAPI, args string) error { return app.PlanMode(args) }},
		{Name: "hotkeys", Description: "show keybinding map",
			Fn: func(app CommandAPI, args string) error { app.AddSystemBlock(app.KeyMap().Hotkeys()); return nil }},
		{Name: "help", Description: "show available commands",
			Fn: func(app CommandAPI, args string) error { app.AddSystemBlock(helpText(builtinCommands())); return nil }},
		{Name: "quit", Aliases: []string{"q"}, Description: "quit xdev",
			Fn: func(app CommandAPI, args string) error { app.Quit(); return nil }},
	}
}

// ParseCommand reports whether input names a slash command and splits it
// into the command name (without the leading '/') and the argument text.
// Whitespace-only input and bare "/" are not commands; leading whitespace is
// tolerated; the first token must be [a-z][a-z0-9-]* (so "/1abc" and "/NEW"
// go to the model as plain text). Unknown names still parse ok=true —
// dispatch decides the fall-through.
func ParseCommand(input string) (name, args string, ok bool) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "/") {
		return "", "", false
	}
	fields := strings.SplitN(trimmed[1:], " ", 2)
	name = fields[0]
	if !isCommandName(name) {
		return "", "", false
	}
	if len(fields) == 2 {
		args = strings.TrimSpace(fields[1])
	}
	return name, args, true
}

// isCommandName checks the [a-z][a-z0-9-]* token shape.
func isCommandName(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	colons := 0
	for _, r := range name[1:] {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		case r == ':':
			// Extension-qualified commands are "/server:cmd"; one colon,
			// never leading or trailing (each side must be a name).
			colons++
			if colons > 1 {
				return false
			}
		default:
			return false
		}
	}
	if strings.HasSuffix(name, ":") || strings.Contains(name, "::") {
		return false
	}
	return true
}

// dispatch routes input through the command table. It reports whether the
// input was consumed (a command, known or not) — the caller must then NOT
// submit it as a prompt. Plain text returns false. Markdown commands expand
// their template and send through the normal send path.
func dispatch(app CommandAPI, input string) bool {
	name, raw, ok := ParseCommand(input)
	if !ok {
		return false
	}
	cmds := builtinCommands()
	for i := range cmds {
		c := &cmds[i]
		if c.Name != name && !slices.Contains(c.Aliases, name) {
			continue
		}
		if err := c.Fn(app, raw); err != nil {
			app.AddSystemBlock("error: " + err.Error())
		}
		return true
	}
	if strings.Contains(name, ":") {
		// Extension command ("/server:cmd args").
		if out, err := app.RunExtensionCommand(name, raw); err != nil {
			app.AddSystemBlock("error: " + err.Error())
		} else {
			app.AddSystemBlock(out)
		}
		return true
	}
	for _, mc := range DiscoverCommands(app.CommandDir()) {
		if mc.Name != name {
			continue
		}
		app.SendPrompt(ExpandArgs(mc.Template, raw))
		return true
	}
	// Unknown name: not consumed — the caller submits it as literal prompt
	// text (issue #11: "/foo" is never rejected).
	return false
}

// Session lifecycle runs through SessionOps (the store lives in cmd);
// unset ops degrade to a notice instead of new TUI-side machinery.

func (a *App) NewSession() error {
	if a.ops == nil || a.ops.New == nil {
		return fmt.Errorf("session lifecycle not wired — restart xdev for a fresh session")
	}
	return a.ops.New()
}

func (a *App) ClearSession() error {
	if a.ops == nil || a.ops.Clear == nil {
		return fmt.Errorf("session lifecycle not wired")
	}
	return a.ops.Clear()
}

func (a *App) DropSession() error {
	if a.ops == nil || a.ops.Drop == nil {
		return fmt.Errorf("session lifecycle not wired")
	}
	return a.ops.Drop()
}

// CommandDir returns the cwd markdown commands are discovered from
// ("" = none).
func (a *App) CommandDir() string { return a.commandDir }

// SetExtensionCommands installs the "/server:cmd" roster (for the dropdown)
// and the runner invoked when one is submitted. Pass nil to disable.
func (a *App) SetExtensionCommands(desc map[string]string, run ExtensionCommand) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.extCommands = desc
	a.extRun = run
}

// RunExtensionCommand implements CommandAPI.
func (a *App) RunExtensionCommand(name, args string) (string, error) {
	a.mu.Lock()
	run := a.extRun
	a.mu.Unlock()
	if run == nil {
		return "", errors.New("no extensions loaded")
	}
	return run(name, args)
}

// SetCommandDir points markdown command discovery at cwd.
func (a *App) SetCommandDir(cwd string) { a.commandDir = cwd }

// helpText renders the aligned command list for /help.
func helpText(cmds []Command) string {
	// The name column covers aliases too ("/new, /fresh" is wider than
	// "/settings"), so every description starts on the same column.
	maxLen := 0
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = "/" + c.Name
		for _, al := range c.Aliases {
			names[i] += ", /" + al
		}
		if len(names[i]) > maxLen {
			maxLen = len(names[i])
		}
	}
	var b strings.Builder
	b.WriteString("commands:")
	for i, c := range cmds {
		fmt.Fprintf(&b, "\n  %-*s %s", maxLen, names[i], c.Description)
	}
	return b.String()
}
