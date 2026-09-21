package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/mcpclient"
	"github.com/FreePeak/xdev/internal/skills"
	"github.com/FreePeak/xdev/internal/tool"
)

// mcpsSeam is the test hook for MCP config loading.
// When set (in tests), mcpsCommand calls through it.
var mcpsSeam func() (cfg *mcpclient.Config, err error)

// probeSeam is the test hook for the reachability probe. When set (in
// tests), mcpsCommand calls through it instead of mcpclient.ServerHealthProbe,
// so a test can assert states without spinning up real servers.
var probeSeam func(cfg *mcpclient.Config) ([]mcpclient.ServerStatus, error)

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
// SessionOps wires the session lifecycle to the host (the store lives in
// cmd). Fresh rotates provider-facing state only — same session identity
// and transcript; nil ops degrade to a notice (Fresh falls back to New).
type SessionOps struct {
	New, Clear, Drop func() error
	Fresh            func() error
	Fork             func() error
	Dump             func() (string, error)
	// Export writes the transcript as one self-contained HTML file and
	// returns the path written ("" = the default export path).
	Export func(path string) (string, error)
	// Share seals the transcript, serves it on loopback, and returns the
	// view-only link (the key rides in the URL fragment).
	Share  func() (string, error)
	Resume func(query string) error
	// NavigateTree rewinds the session to a point in the message tree
	// (omp session.navigateTree, the tree selector's Enter / Shift+Enter):
	// a user row moves the leaf to its PARENT and returns that prompt as
	// the composer draft, so it can be edited and resent without
	// duplicating the entry; any other row moves the leaf onto itself.
	// With summarize the abandoned branch is first condensed into a
	// branch_summary entry hung off the target (visible to the model on
	// the new branch). The transcript is restored from the new leaf
	// before the draft is returned. nil degrades to a notice.
	NavigateTree func(entryID string, summarize bool) (draft string, err error)
	// Handoff replaces the live context with a handoff document (M5 #23):
	// the host generates the document through a side request, commits it as
	// a compaction entry on this session, and returns the document text.
	// nil degrades the command to a notice.
	Handoff func(instruction string) (string, error)
	// Rename stamps a custom title on the live session (/rename). The title
	// slot is rewritten in place, so the listing and the picker see it.
	Rename func(title string) error
	// Recent lists the sessions /resume offers when called with no
	// argument; the TUI renders them as a picker and hands the chosen id
	// back to Resume. nil degrades /resume to Resume("").
	Recent func() []ResumeOption
}

// ResumeOption is one session in the /resume picker.
type ResumeOption struct {
	ID      string // full id (Resume resolves by prefix; pass enough to be unambiguous)
	Title   string // display title
	Detail  string // e.g. "print · 17:53" or the entry preview
	Current bool   // the session this TUI is already on
}

// ModelOps wires the /model command to the live provider state (lives in
// cmd). Current reports the active ref; Views builds the selector's tabs
// (one per provider); Models returns the flat model catalog; Set switches
// the active model for subsequent turns. nil ops degrade the command to a
// notice.
type ModelOps struct {
	Current func() string
	Views   func() []PickerView
	Models  func() []PickerItem
	Set     func(ref string) error
	// Cycle advances through --models patterns (omp Ctrl+P).
	// ok=false when cycling is not configured (the chord stays
	// menu-prev in that case).
	Cycle func() (next string, ok bool)
}

// SettingsOps wires the /settings command to the config layer (lives in
// cmd). List renders the resolved settings; SetThinking persists the
// showThinking key to the global layer. nil ops degrade the command to a
// notice.
type SettingsOps struct {
	Path        string
	List        func() []string
	SetThinking func(on bool) error
	// SetSidebar persists the context dock's display policy (#291 §1), the
	// sidebarMode key in the same layer Alt+s writes.
	SetSidebar func(mode string) error
}

// PlanOps wires the /plan command to the live plan-mode state (lives in
// cmd). Get reports the current state; Set toggles it. nil ops degrade
// the command to a notice.
type PlanOps struct {
	Get func() bool
	Set func(on bool) error
	// Show renders the pending proposal and the task list it was written
	// against (/plan show, #291 §2): the "" case is a session with nothing
	// proposed yet. Approval is NOT here on purpose — /plan off and revision
	// feedback already resolve a proposal, and a second transition would drift.
	Show func() string
}

// PrewalkOps wires the /prewalk command (the live target lives in cmd).
type PrewalkOps struct {
	Enabled func() bool
	Status  func() string
	Set     func(on bool, into string) error
}

// GoalOps wires the /goal command to the live goal state (lives in cmd).
// View renders the current goal and budget. The verbs mirror the goal tool's
// ops so an interactive session can drive a goal without asking the model to
// do it; a nil verb is reported as unwired, never a silent no-op.
type GoalOps struct {
	View     func() string
	Create   func(objective string) (string, error)
	Resume   func(objective string) (string, error)
	Evidence func(note string) (string, error)
	Complete func(notes []string) (string, error)
	Drop     func() (string, error)
}

// Dispatch runs one /goal subcommand and returns the block to display. With
// no argument (or any read verb) it shows the current goal; an unknown verb is
// a usage error naming the grammar, because silently viewing made
// `/goal create …` look like a dead command.
func (o *GoalOps) Dispatch(args string) (string, error) {
	if o == nil || o.View == nil {
		return "", errors.New("goal not wired")
	}
	trimmed := strings.TrimSpace(args)
	verb, rest := trimmed, ""
	if i := strings.IndexFunc(trimmed, func(r rune) bool { return r == ' ' || r == '\t' }); i >= 0 {
		verb, rest = trimmed[:i], strings.TrimSpace(trimmed[i+1:])
	}
	// Read intent: every synonym for "show me the goal" routes to view. The
	// command used to reject `check` / `show` outright while its help named no
	// verb at all, so a user asking after the goal had only invented words to
	// try (same class as `/theme list`).
	switch verb {
	case "", "view", "get", "status", "show", "check", "list", "info":
		return o.View(), nil
	case "create":
		if o.Create == nil {
			return "", errors.New("goal create not wired")
		}
		if rest == "" {
			return "", errors.New("usage: /goal create <objective>")
		}
		return o.Create(rest)
	case "resume":
		if o.Resume == nil {
			return "", errors.New("goal resume not wired")
		}
		if rest == "" {
			return "", errors.New("usage: /goal resume <objective>")
		}
		return o.Resume(rest)
	case "evidence":
		if o.Evidence == nil {
			return "", errors.New("goal evidence not wired")
		}
		if rest == "" {
			return "", errors.New("usage: /goal evidence <what was verified>")
		}
		return o.Evidence(rest)
	case "complete":
		if o.Complete == nil {
			return "", errors.New("goal complete not wired")
		}
		return o.Complete(splitGoalNotes(rest))
	case "drop":
		if o.Drop == nil {
			return "", errors.New("goal drop not wired")
		}
		return o.Drop()
	default:
		return "", fmt.Errorf("unknown /goal verb %q (view | create <objective> | resume <objective> | evidence <note> | complete [notes] | drop)", verb)
	}
}

// splitGoalNotes turns "a; b" or "a, b" into separate completion notes; a
// plain sentence stays one note.
func splitGoalNotes(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ThemeOps wires the /theme command (theme resolution lives in cmd).
type ThemeOps struct {
	Current func() string
	List    func() []string
	Set     func(name string) error
}

// ConnectOps wires /connect to the provider catalog (the catalog and the
// writing live in cmd/config). Items lists the rows to offer — Label is the
// provider name, Detail says what it is and whether a credential is in hand.
// Connect writes one row into models.yml; DefaultRef names the model a session
// gets from it ("" = none pinned).
type ConnectOps struct {
	Items      func() []PickerItem
	Connect    func(name string) error
	DefaultRef func(name string) string
}

// MemoryOps wires the /memory command (backend lives in cmd). View, Stats and
// Clear are the backend-agnostic verbs; Diagnose is the remote backend's
// health dump and Queue/Sync/Enqueue are the queue-backed store's (mnemopi);
// a nil func is reported as unwired, never a silent no-op.
type MemoryOps struct {
	View  func() string
	Stats func() string
	Clear func() error
	// Diagnose is the backend config + server health dump.
	Diagnose func() string
	// Queue reports the pending retain work.
	Queue func() string
	// Sync forces one bounded consolidation and returns its report.
	Sync func() (string, error)
	// Enqueue queues one retain for the next boundary. The argument is the
	// subcommand tail, passed through verbatim (including empty); the
	// backend decides whether it is required.
	Enqueue func(text string) (string, error)
}

// Dispatch runs one /memory subcommand and returns the block to display. The
// grammar lives here rather than in the app method, so every backend answers
// the same verbs through one place.
func (o *MemoryOps) Dispatch(args string) (string, error) {
	if o == nil {
		return "", errors.New("memory not wired (set memory: local, mnemopi, hindsight or sharpshooter in settings)")
	}
	trimmed := strings.TrimSpace(args)
	verb, rest := trimmed, ""
	if i := strings.IndexFunc(trimmed, func(r rune) bool { return r == ' ' || r == '\t' }); i >= 0 {
		verb, rest = trimmed[:i], strings.TrimSpace(trimmed[i+1:])
	}
	switch verb {
	case "", "view":
		if o.View == nil {
			return "", errors.New("memory view not wired")
		}
		return o.View(), nil
	case "stats":
		if o.Stats == nil {
			return "", errors.New("memory stats not wired")
		}
		return o.Stats(), nil
	case "clear", "reset":
		if o.Clear == nil {
			return "", errors.New("memory clear not wired")
		}
		if err := o.Clear(); err != nil {
			return "", err
		}
		return "memory cleared", nil
	case "diagnose":
		if o.Diagnose == nil {
			return "", errors.New("memory diagnose is not available for this backend (hindsight only)")
		}
		return o.Diagnose(), nil
	case "queue":
		if o.Queue == nil {
			return "", errors.New("memory queue: this backend keeps no retain queue (memory: mnemopi has one)")
		}
		return o.Queue(), nil
	case "sync":
		if o.Sync == nil {
			return "", errors.New("memory sync: this backend keeps no retain queue (memory: mnemopi has one; hindsight syncs server-side)")
		}
		return o.Sync()
	case "enqueue", "rebuild":
		if o.Enqueue == nil {
			return "", errors.New("memory enqueue: this backend has no queue to enqueue into (memory: mnemopi or hindsight)")
		}
		return o.Enqueue(rest)
	default:
		return "", fmt.Errorf("memory: use view|stats|clear, or queue|sync|enqueue <text> (mnemopi) or diagnose|enqueue (hindsight)")
	}
}

// AdvisorOps wires the /advisor command (state lives in cmd).
type AdvisorOps struct {
	Enabled func() bool
	Set     func(on bool) error
	Status  func() string
	Dump    func() string
}

// CollabOps wires /collab and /join to the live session sharing (the relay
// lives in cmd). Start begins hosting and returns the join instructions
// (mode.View publishes a view-only link, mode.Remote binds beyond loopback);
// Status renders the active room; Stop ends hosting; Join joins a link and
// returns the notice line. nil ops degrade to a notice.
type CollabOps struct {
	Start  func(mode CollabMode) (string, error)
	Status func() string
	Stop   func() error
	Join   func(link string) (string, error)
	// Forward routes a prompt typed while joined as a guest to the host
	// session; it reports true when the guest path consumed the input (a
	// local turn must then not start).
	Forward func(text string) bool
}

// CollabMode selects how /collab hosts: a view-only link grants read access
// only, and Remote is the explicit opt-in for binding a non-loopback address.
type CollabMode struct {
	View   bool
	Remote bool
	Addr   string // explicit bind address ("" = loopback, random port)
}

// Collab is the process-wide /collab implementation. It is a package-level
// slot (like BashJobs) so the command seam lives entirely in this file: cmd
// installs it at startup, tests may override it, and nil degrades to a notice.
var Collab *CollabOps

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
	RenameSession(title string) error
	ExportSession(path string) error
	ShareSession() error
	ResumeSession(query string) error
	SwitchModel(args string) error
	PlanMode(args string) error
	Vibe(args string) error
	// Trajectory is /trajectory: the session's event ledger, opened as a
	// modal list where a row's inspector shows the record's full body.
	Trajectory() error
	Goal(args string) error
	Advisor(args string) error
	Memory(args string) error
	Theme(args string) error
	Prewalk(args string) error
	// Connect opens the provider catalog (xdev's own "connect to a
	// subscription"): with an argument it connects that provider, without one
	// it shows the picker.
	Connect(args string) error
	Handoff(args string) error
	HubRoster() error
	SettingsView(args string) error
	// ThinkingLevel is /thinking [level]: bare reports, a level applies and
	// persists the request-side reasoning level for the next turn.
	ThinkingLevel(args string) error
	// ExtensionCommands exposes the "/server:cmd" roster for /help; nil
	// when no extensions are loaded.
	ExtensionCommands() map[string]string
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
		{Name: "new", Description: "start a new session",
			Fn: func(app CommandAPI, args string) error { return app.NewSession() }},
		{Name: "fresh", Description: "rotate provider state; keep this session",
			Fn: func(app CommandAPI, args string) error { return freshSession(app) }},
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
		{Name: "rename", Description: "title this session: /rename <new title>",
			Fn: func(app CommandAPI, args string) error { return app.RenameSession(strings.TrimSpace(args)) }},
		{Name: "dump", Description: "export the transcript to markdown",
			Fn: func(app CommandAPI, args string) error { return app.DumpSession() }},
		{Name: "export", Description: "write the transcript as self-contained HTML: /export [path]",
			Fn: func(app CommandAPI, args string) error { return app.ExportSession(strings.TrimSpace(args)) }},
		{Name: "share", Description: "serve an E2E-encrypted view-only snapshot and print the link",
			Fn: func(app CommandAPI, args string) error { return app.ShareSession() }},
		{Name: "resume", Description: "resume a session by id prefix",
			Fn: func(app CommandAPI, args string) error { return app.ResumeSession(args) }},
		{Name: "model", Description: "show or switch the active model",
			Fn: func(app CommandAPI, args string) error { return app.SwitchModel(args) }},
		{Name: "settings", Description: "show settings; toggle showThinking on|off",
			Fn: func(app CommandAPI, args string) error { return app.SettingsView(args) }},
		{Name: "thinking", Description: "request-side reasoning: /thinking [off|auto|minimal|low|medium|high] (bare reports)",
			Fn: func(app CommandAPI, args string) error { return app.ThinkingLevel(args) }},
		{Name: "prewalk", Description: "one-shot model handoff: /prewalk [on|off|into <ref>] (default: the session model)",
			Fn: func(app CommandAPI, args string) error { return app.Prewalk(args) }},
		{Name: "handoff", Description: "replace the context with a handoff document (continues from it)",
			Fn: func(app CommandAPI, args string) error { return app.Handoff(args) }},
		{Name: "theme", Description: "show or switch the theme: /theme <name>",
			Fn: func(app CommandAPI, args string) error { return app.Theme(args) }},
		{Name: "memory", Description: "long-term memory: /memory view|stats|clear, plus queue|sync|enqueue (mnemopi) and diagnose|enqueue (hindsight)",
			Fn: func(app CommandAPI, args string) error { return app.Memory(args) }},
		{Name: "advisor", Description: "background reviewer: /advisor on|off|status|dump",
			Fn: func(app CommandAPI, args string) error { return app.Advisor(args) }},
		{Name: "plan", Description: "toggle plan mode (read-only research, propose to exit); /plan show reads the pending plan",
			Fn: func(app CommandAPI, args string) error { return app.PlanMode(args) }},
		{Name: "goal", Description: "session objective + token budget: /goal view|create <objective>|resume|evidence <note>|complete [notes]|drop",
			Fn: func(app CommandAPI, args string) error { return app.Goal(args) }},
		{Name: "vibe", Description: "director mode: read + todo + vibe_* worker tools (/vibe [prompt])",
			Fn: func(app CommandAPI, args string) error { return app.Vibe(args) }},
		{Name: "connect", Description: "connect a provider from the catalog: /connect [name]",
			Fn: func(app CommandAPI, args string) error { return app.Connect(args) }},
		{Name: "trajectory", Aliases: []string{"traj"}, Description: "session event ledger: one row per record, Enter for details",
			Fn: func(app CommandAPI, args string) error { return app.Trajectory() }},
		{Name: "hub", Description: "agent hub roster: live status, kill/revive, transcripts",
			Fn: func(app CommandAPI, args string) error { return app.HubRoster() }},
		{Name: "hotkeys", Description: "show keybinding map",
			Fn: func(app CommandAPI, args string) error { app.AddSystemBlock(app.KeyMap().Hotkeys()); return nil }},
		{Name: "tasks", Description: "list background bash jobs",
			Fn: func(app CommandAPI, args string) error {
				if BashJobs == nil {
					app.AddSystemBlock("background jobs are not wired in this build")
					return nil
				}
				app.AddSystemBlock(BashJobs())
				return nil
			}},
		{Name: "collab", Description: "share this session live: /collab [view|remote]|status|stop",
			Fn: func(app CommandAPI, args string) error { return collabCommand(app, args) }},
		{Name: "join", Description: "join a shared session as a guest: /join <link>",
			Fn: func(app CommandAPI, args string) error { return joinCommand(app, args) }},
		{Name: "help", Description: "show available commands",
			Fn: func(app CommandAPI, args string) error { app.AddSystemBlock(helpText(app)); return nil }},
		{Name: "quit", Aliases: []string{"q"}, Description: "quit xdev",
			Fn: func(app CommandAPI, args string) error { app.Quit(); return nil }},
		{Name: "mcps", Description: "list configured MCP servers and their status",
			Fn: func(app CommandAPI, args string) error { return mcpsCommand(app, args) }},
	}
}

// mcpsCommand implements /mcps: list configured MCP servers
// and their status (M6 #7). Absent config → "no MCP servers
// configured"; errors from the loader surface as an error block.
// Uses mcpsSeam when set (tests), otherwise mcpclient.LoadConfig.
func mcpsCommand(app CommandAPI, args string) error {
	var cfg *mcpclient.Config
	var err error
	if mcpsSeam != nil {
		cfg, err = mcpsSeam()
	} else {
		cfg, err = mcpclient.LoadConfig(mcpConfigPath())
	}
	if err != nil {
		return fmt.Errorf("mcp config: %v", err)
	}
	if len(cfg.Servers) == 0 {
		app.AddSystemBlock("no MCP servers configured")
		return nil
	}
	var statuses []mcpclient.ServerStatus
	if probeSeam != nil {
		statuses, err = probeSeam(cfg)
	} else {
		statuses, err = mcpclient.ServerHealthProbe(context.Background(), cfg)
	}
	if err != nil {
		app.AddSystemBlock("mcp health probe: " + err.Error())
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("MCP servers:")
	for _, n := range names {
		sc := cfg.Servers[n]
		state := "enabled"
		if sc.Disabled {
			state = "disabled"
		}
		if sc.Enabled != nil && !*sc.Enabled {
			state = "disabled"
		}
		transport := "stdio"
		if sc.URL != "" {
			transport = "http"
		}
		// Overlay the live probe: a server that is enabled but
		// unreachable shows "unreachable" so the user can tell a
		// broken server from a disabled one.
		for _, st := range statuses {
			if st.Name == n && st.State != "disabled" && st.State != "enabled" {
				state = st.State
				break
			}
		}
		fmt.Fprintf(&b, "\n  %s  %-11s  %s", n, state, transport)
	}
	app.AddSystemBlock(b.String())
	return nil
}

// mcpConfigPath is <dataDir>/mcp.yml (absent = MCP off, PRD §2).
func mcpConfigPath() string {
	return filepath.Join(config.DataDir(), "mcp.yml")
}

// serverURL renders the URL for a server config, falling
// back to the command when it is a stdio server.
func serverURL(sc *mcpclient.ServerConfig) string {
	if sc == nil {
		return "(unknown)"
	}
	if sc.URL != "" {
		return sc.URL
	}
	if sc.Command != "" {
		return sc.Command
	}
	return "(no url/command)"
}

// Handoff implements CommandAPI: /handoff [instruction] hands the live
// context off to a generated document (M5 #23). The document generation,
// the per-branch reset and the commit all live in cmd (the store,
// the session model, and the advisor are wired there); this surfaces
// the result — including the document itself, which is the point of the
// command.
func (a *App) Handoff(args string) error {
	if a.ops == nil || a.ops.Handoff == nil {
		return fmt.Errorf("handoff not wired")
	}
	doc, err := a.ops.Handoff(strings.TrimSpace(args))
	if err != nil {
		return err
	}
	a.AddSystemBlock(doc + "\n\n· handoff committed — the next turn continues from this document")
	return nil
}

// BashJobs renders the background bash job listing for /tasks. It defaults
// to the tool package's process-wide registry (the same one the bash tool
// records into); tests may override it. Nil degrades to a notice.
var BashJobs = func() string { return tool.SharedBashJobs().Render() }

// Bang runs one composer "! command" draft as a local shell command (PRD
// M10 #163). It is a package-level slot (like BashJobs) so cmd installs the
// executor at startup and tests may override it; nil degrades to a notice.
// The transcript is the only destination — a bang run never reaches the
// model, which is the whole point of the mode.
var Bang func(cmd string) error

// bangCommand consumes a draft that opens with '!'. It reports whether the
// draft belonged to shell mode, in which case the caller must NOT submit it
// as a prompt: a bare '!' and an unwired seam are notices, not turns.
func bangCommand(app CommandAPI, input string) bool {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "!") {
		return false
	}
	cmd := strings.TrimSpace(trimmed[1:])
	switch {
	case cmd == "":
		app.AddSystemBlock(`shell mode: nothing to run — type "!<command>"`)
	case Bang == nil:
		app.AddSystemBlock("shell mode is not wired in this build")
	default:
		if err := Bang(cmd); err != nil {
			app.AddSystemBlock("error: " + err.Error())
		}
	}
	return true
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
	// Shell mode is checked before the slash table: '!' is not a command
	// name, and a bang draft must be consumed on its own terms.
	if bangCommand(app, input) {
		return true
	}
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
	// "/skill:<name> [args]" — the skill protocol's command form. Checked
	// before the extension branch: the skill: namespace is reserved.
	if strings.HasPrefix(name, "skill:") {
		return skillCommand(app, strings.TrimPrefix(name, "skill:"), raw)
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

// skillCommand runs "/skill:<name> [args]": the skill's body (frontmatter
// stripped by discovery) goes out as a prompt with the user's args appended
// as a "User:" line. An unknown name is a notice — a typo'd skill must
// never become a model turn.
func skillCommand(app CommandAPI, name, raw string) bool {
	s, ok := skills.Find(skills.Discover(app.CommandDir()), name)
	if !ok {
		app.AddSystemBlock(fmt.Sprintf("no skill named %q — discovered: %s", name, skillNames(app.CommandDir())))
		return true
	}
	prompt := s.Body
	if raw != "" {
		prompt += "\n\nUser: " + raw
	}
	app.SendPrompt(prompt)
	return true
}

// skillSuggestions lists discovered skills as "/skill:<name>" dropdown
// rows. Hidden skills are listed too: hide only removes them from the
// model's prompt list, and the command stays the explicit way in.
func skillSuggestions(cwd string) []suggestion {
	list := skills.Discover(cwd)
	out := make([]suggestion, 0, len(list))
	for _, s := range list {
		out = append(out, suggestion{Name: "/skill:" + s.Name, Description: s.Description, Tag: "skill"})
	}
	return out
}

// skillNames renders the discovered skill names for the unknown-skill
// notice.
func skillNames(cwd string) string {
	list := skills.Discover(cwd)
	if len(list) == 0 {
		return "none"
	}
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// Session lifecycle runs through SessionOps (the store lives in cmd);
// unset ops degrade to a notice instead of new TUI-side machinery.

func (a *App) NewSession() error {
	if a.ops == nil || a.ops.New == nil {
		return fmt.Errorf("session lifecycle not wired — restart xdev for a fresh session")
	}
	return a.ops.New()
}

// FreshSession rotates provider-facing state only (issue #11 §5): the
// session identity and transcript are kept. Hosts without a Fresh op fall
// back to /new semantics rather than erroring.
func (a *App) FreshSession() error {
	if a.ops == nil {
		return fmt.Errorf("session lifecycle not wired")
	}
	if a.ops.Fresh != nil {
		return a.ops.Fresh()
	}
	return a.NewSession()
}

// freshSession routes `/fresh` to the app's provider-rotation op when the
// host exposes one (App does; bare CommandAPI fakes in tests may not), and
// otherwise degrades to /new semantics.
func freshSession(app CommandAPI) error {
	if f, ok := app.(interface{ FreshSession() error }); ok {
		return f.FreshSession()
	}
	return app.NewSession()
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

// ExportSession implements CommandAPI /export: write the transcript as one
// self-contained HTML file at path ("" = the default export path) and report
// where it landed.
func (a *App) ExportSession(path string) error {
	if a.ops == nil || a.ops.Export == nil {
		return fmt.Errorf("session export not wired")
	}
	written, err := a.ops.Export(path)
	if err != nil {
		return err
	}
	a.AddSystemBlock("exported to " + written)
	return nil
}

// ShareSession implements CommandAPI /share: seal the transcript, serve it
// over loopback, and put the view-only link in the transcript. The link
// works while this process lives.
func (a *App) ShareSession() error {
	if a.ops == nil || a.ops.Share == nil {
		return fmt.Errorf("session share not wired")
	}
	link, err := a.ops.Share()
	if err != nil {
		return err
	}
	a.AddSystemBlock("share link (view-only, key in the URL fragment):\n" + link)
	return nil
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

// ExtensionCommands exposes the "/server:cmd" roster (for /help); nil when
// no extensions are loaded.
func (a *App) ExtensionCommands() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.extCommands
}

// RunExtensionCommand implements CommandAPI.
func (a *App) RunExtensionCommand(name, args string) (string, error) {
	a.mu.Lock()
	run := a.extRun
	a.mu.Unlock()
	if run == nil {
		// The manager normally produces this message with its own drop-in
		// dir; with no extensions attached the TUI never wired a runner,
		// so name the default location here.
		return "", fmt.Errorf("no extensions loaded (drop an executable into %s and restart)",
			filepath.Join(config.DataDir(), "extensions"))
	}
	return run(name, args)
}

// SetCommandDir points markdown command discovery at cwd.
func (a *App) SetCommandDir(cwd string) { a.commandDir = cwd }

// helpText renders the aligned command list for /help: built-ins first,
// then markdown-discovered and extension commands — /help must show every
// command the dropdown offers, not only the static registry.
func helpText(app CommandAPI) string {
	cmds := builtinCommands()
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
	if mc := DiscoverCommands(app.CommandDir()); len(mc) > 0 {
		b.WriteString("\n\nproject commands:")
		for _, c := range mc {
			fmt.Fprintf(&b, "\n  %-*s %s", maxLen, "/"+c.Name, collapseLine(c.Description))
		}
	}
	if ext := app.ExtensionCommands(); len(ext) > 0 {
		b.WriteString("\n\nextension commands:")
		extNames := make([]string, 0, len(ext))
		for n := range ext {
			extNames = append(extNames, n)
		}
		sort.Strings(extNames)
		for _, n := range extNames {
			fmt.Fprintf(&b, "\n  %-*s %s", maxLen, "/"+n, collapseLine(ext[n]))
		}
	}
	return b.String()
}

// collabCommand parses "/collab [view|remote]|status|stop". Hosting is
// started by bare /collab (full control) or /collab view (read-only link);
// "remote" is the explicit opt-in for binding beyond loopback.
func collabCommand(app CommandAPI, args string) error {
	if Collab == nil {
		app.AddSystemBlock("collab is not wired in this build")
		return nil
	}
	mode := CollabMode{}
	for _, word := range strings.Fields(args) {
		switch strings.ToLower(word) {
		case "":
		case "view", "view-only":
			mode.View = true
		case "remote":
			mode.Remote = true
		case "status":
			if Collab.Status == nil {
				app.AddSystemBlock("collab status is not wired in this build")
				return nil
			}
			app.AddSystemBlock(Collab.Status())
			return nil
		case "stop":
			if Collab.Stop == nil {
				app.AddSystemBlock("collab stop is not wired in this build")
				return nil
			}
			if err := Collab.Stop(); err != nil {
				return err
			}
			app.AddSystemBlock("· collab stopped")
			return nil
		default:
			return fmt.Errorf("usage: /collab [view|remote] | /collab status | /collab stop (got %q)", word)
		}
	}
	if Collab.Start == nil {
		app.AddSystemBlock("collab hosting is not wired in this build")
		return nil
	}
	out, err := Collab.Start(mode)
	if err != nil {
		return err
	}
	app.AddSystemBlock(out)
	return nil
}

// Connect implements CommandAPI /connect: the provider catalog as a picker,
// and one row's write into models.yml. With an argument it connects that
// provider directly — the shape a user reaches for once they know the name.
func (a *App) Connect(args string) error {
	if a.connectOps == nil {
		return fmt.Errorf("provider catalog not wired")
	}
	if name := strings.TrimSpace(args); name != "" {
		return a.connectProvider(name)
	}
	if a.connectOps.Items == nil {
		return fmt.Errorf("provider catalog not wired")
	}
	items := a.connectOps.Items()
	if len(items) == 0 {
		return fmt.Errorf("provider catalog is empty (this build has no connect catalog)")
	}
	a.OpenPicker(PickerOptions{
		Title: "connect a provider",
		Views: []PickerView{{Name: "providers", Action: "connect", Items: items}},
		OnSelect: func(name string) {
			if err := a.connectProvider(name); err != nil {
				a.AddSystemBlock("error: " + err.Error())
			}
		},
	})
	return nil
}

// connectProvider writes one catalog row and reports the model that is now
// usable. The picker is already gone by the time a select lands here, so the
// answer goes to the transcript either way.
func (a *App) connectProvider(name string) error {
	if a.connectOps.Connect == nil {
		return fmt.Errorf("connecting providers is not wired")
	}
	if err := a.connectOps.Connect(name); err != nil {
		return err
	}
	msg := "connected " + name
	if a.connectOps.DefaultRef != nil {
		if ref := a.connectOps.DefaultRef(name); ref != "" {
			msg += "\n· this session can switch to it now: /model " + ref
		}
	}
	a.AddSystemBlock(msg)
	return nil
}

// joinCommand parses "/join <link>" and joins as a guest.
func joinCommand(app CommandAPI, args string) error {
	link := strings.TrimSpace(args)
	if link == "" {
		return fmt.Errorf(`usage: /join "<link>" — the link printed by /collab`)
	}
	if Collab == nil || Collab.Join == nil {
		app.AddSystemBlock("collab joining is not wired in this build")
		return nil
	}
	out, err := Collab.Join(link)
	if err != nil {
		return err
	}
	app.AddSystemBlock(out)
	return nil
}

// collapseLine truncates a description to its first line so a multi-line
// markdown command cannot break the table.
func collapseLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

// ThinkingOps wires the /thinking command to the live request-side level
// (lives in cmd, which owns the provider holder and the settings write).
// Current reports the level in force for the next turn; Set applies and
// persists one. nil ops degrade the command to a notice.
type ThinkingOps struct {
	Current func() string
	Set     func(level string) error
}
