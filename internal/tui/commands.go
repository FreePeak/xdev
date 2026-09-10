package tui

import (
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
	Fn          func(app CommandAPI) error
}

// SessionOps holds the session lifecycle operations. The session store
// lives in cmd, so it wires these; nil entries degrade the commands to
// notices instead of new store machinery in the TUI.
type SessionOps struct {
	New, Clear, Drop func() error
}

// CommandAPI is the app surface commands need. All methods are safe to call
// from the key thread (same mutators the UI already uses).
type CommandAPI interface {
	NewSession() error
	ClearSession() error
	DropSession() error
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
		{Name: "new", Description: "start a fresh session",
			Fn: func(app CommandAPI) error { return app.NewSession() }},
		{Name: "clear", Description: "reset context in place (history kept on disk)",
			Fn: func(app CommandAPI) error { return app.ClearSession() }},
		{Name: "drop", Description: "delete the session file and start fresh",
			Fn: func(app CommandAPI) error { return app.DropSession() }},
		{Name: "help", Description: "show available commands",
			Fn: func(app CommandAPI) error { app.AddSystemBlock(helpText(builtinCommands())); return nil }},
		{Name: "quit", Aliases: []string{"q"}, Description: "quit xdev",
			Fn: func(app CommandAPI) error { app.Quit(); return nil }},
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
	for _, r := range name[1:] {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
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
		if err := c.Fn(app); err != nil {
			app.AddSystemBlock("error: " + err.Error())
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
	app.AddSystemBlock(fmt.Sprintf("unknown command: /%s — type /help", name))
	return true
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

// SetCommandDir points markdown command discovery at cwd.
func (a *App) SetCommandDir(cwd string) { a.commandDir = cwd }

// helpText renders the aligned command list for /help.
func helpText(cmds []Command) string {
	var b strings.Builder
	b.WriteString("commands:")
	for _, c := range cmds {
		names := "/" + c.Name
		for _, al := range c.Aliases {
			names += ", /" + al
		}
		fmt.Fprintf(&b, "\n  %-12s %s", names, c.Description)
	}
	return b.String()
}
