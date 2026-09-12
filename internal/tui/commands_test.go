package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// fakeAPI records CommandAPI calls for dispatch tests.
type fakeAPI struct {
	newed, freshed, cleared, dropped, quit int
	blocks                                 []string
	sent                                   []string
	dir                                    string
	fail                                   string // method name that returns an error

	extCalls []string
	extErr   error

	setModel string
	plan     string
	advisor  string
	mem      string
	theme    string
	prewalk  string
}

func (f *fakeAPI) NewSession() error {
	f.newed++
	if f.fail == "new" {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeAPI) ClearSession() error {
	f.cleared++
	if f.fail == "clear" {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeAPI) DropSession() error {
	f.dropped++
	if f.fail == "drop" {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeAPI) AddSystemBlock(text string) { f.blocks = append(f.blocks, text) }
func (f *fakeAPI) SendPrompt(text string)     { f.sent = append(f.sent, text) }
func (f *fakeAPI) CommandDir() string         { return f.dir }
func (f *fakeAPI) Quit()                      { f.quit++ }

func TestDispatchTasks(t *testing.T) {
	orig := BashJobs
	t.Cleanup(func() { BashJobs = orig })

	BashJobs = func() string { return "Background bash jobs (1):\n#1 exit 0 — echo hi" }
	f := &fakeAPI{}
	if !dispatch(f, "/tasks") {
		t.Fatal("/tasks must be consumed as a command")
	}
	if len(f.blocks) != 1 || !strings.Contains(f.blocks[0], "Background bash jobs (1)") {
		t.Fatalf("blocks = %q", f.blocks)
	}
	if len(f.sent) != 0 {
		t.Fatalf("/tasks must never reach the model: %q", f.sent)
	}

	// Nil renderer (bare TUI/test harness) degrades to a notice.
	BashJobs = nil
	f = &fakeAPI{}
	if !dispatch(f, "/tasks") {
		t.Fatal("/tasks must stay consumed when unwired")
	}
	if len(f.blocks) != 1 || !strings.Contains(f.blocks[0], "not wired") {
		t.Fatalf("unwired /tasks blocks = %q", f.blocks)
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input string
		name  string
		args  string
		ok    bool
	}{
		{input: "/new", name: "new", ok: true},
		{input: "/new extra words", name: "new", args: "extra words", ok: true},
		{input: "/q", name: "q", ok: true},           // alias still parses
		{input: "  /help  ", name: "help", ok: true}, // leading/trailing spaces tolerated
		{input: "/long-cmd-name", name: "long-cmd-name", ok: true},
		{input: "hello world", ok: false},   // plain text
		{input: "", ok: false},              // empty
		{input: "   ", ok: false},           // whitespace only
		{input: "/", ok: false},             // bare slash, no token
		{input: "/1abc", ok: false},         // first char must be [a-z]
		{input: "/NEW", ok: false},          // case-sensitive: uppercase is model text
		{input: "/new-session!", ok: false}, // punctuation not in the charset
		{input: "try /help", ok: false},     // slash not leading
		{input: "/-dash", ok: false},        // must start with a letter
		{input: "/a b c", name: "a", args: "b c", ok: true},
	}
	for _, tt := range tests {
		name, args, ok := ParseCommand(tt.input)
		if ok != tt.ok || name != tt.name || args != tt.args {
			t.Errorf("ParseCommand(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.input, name, args, ok, tt.name, tt.args, tt.ok)
		}
	}
}

func TestDispatchBuiltins(t *testing.T) {
	tests := []struct {
		input    string
		newed    int
		cleared  int
		dropped  int
		quit     int
		wantSub  string // substring required in the first system block
		wantFail string // fake method forced to error
	}{
		{input: "/new", newed: 1, wantFail: "new", wantSub: "error: boom"},
		{input: "/new", newed: 1},
		{input: "/clear", cleared: 1},
		{input: "/drop", dropped: 1},
		{input: "/quit", quit: 1},
		{input: "/q", quit: 1},
		{input: "/help", wantSub: "commands:"},
		// /model with no args shows the active model + available list.
		{input: "/model", wantSub: "active model:"},
	}
	for _, tt := range tests {
		f := &fakeAPI{fail: tt.wantFail}
		consumed := dispatch(f, tt.input)
		if !consumed {
			t.Errorf("dispatch(%q) = false, want true", tt.input)
			continue
		}
		if f.newed != tt.newed || f.cleared != tt.cleared || f.dropped != tt.dropped || f.quit != tt.quit {
			t.Errorf("dispatch(%q): fake = %+v, want new=%d clear=%d drop=%d quit=%d",
				tt.input, f, tt.newed, tt.cleared, tt.dropped, tt.quit)
		}
		if tt.wantSub != "" {
			if len(f.blocks) != 1 || !strings.Contains(f.blocks[0], tt.wantSub) {
				t.Errorf("dispatch(%q): blocks = %q, want one containing %q", tt.input, f.blocks, tt.wantSub)
			}
		} else if len(f.blocks) != 0 {
			t.Errorf("dispatch(%q): unexpected blocks %q", tt.input, f.blocks)
		}
	}
}

func TestDispatchPlainText(t *testing.T) {
	for _, in := range []string{"hello world", "/1abc", "/NEW", "", "  ", "/", "/unknowncmd"} {
		f := &fakeAPI{}
		if dispatch(f, in) {
			t.Errorf("dispatch(%q) consumed input, want plain-text pass-through", in)
		}
		if len(f.blocks) != 0 || f.quit != 0 {
			t.Errorf("dispatch(%q) had side effects: %+v", in, f)
		}
	}
}

// Unknown slash names are NOT consumed: they fall through as literal
// prompt text (issue #11).
func TestDispatchUnknownFallsThrough(t *testing.T) {
	f := &fakeAPI{}
	if dispatch(f, "/unknowncmd") {
		t.Fatal("dispatch consumed an unknown command; must fall through")
	}
	if len(f.blocks) != 0 {
		t.Fatalf("unexpected blocks: %q", f.blocks)
	}
}

// /model with a ref switches the live model (setModel captured) and
// confirms with a system block; an error propagates as a block, not a
// silent no-op.
func TestDispatchModelSwitch(t *testing.T) {
	f := &fakeAPI{}
	if !dispatch(f, "/model onegw/fast-model") {
		t.Fatal("/model not consumed")
	}
	if f.setModel != "onegw/fast-model" {
		t.Fatalf("setModel = %q", f.setModel)
	}
	// The confirmation block is the App's job; the fake only records the
	// request (so the dispatcher consumed it and routed the ref through).
	if len(f.blocks) != 0 {
		t.Fatalf("fake received blocks = %q", f.blocks)
	}
}

func TestDispatchSettingsTogglesThinking(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var toggles []bool
	app.SetSettingsOps(&SettingsOps{
		Path: "/tmp/xdev-config.yml",
		List: func() []string { return []string{"showThinking true", "theme auto"} },
		SetThinking: func(on bool) error {
			toggles = append(toggles, on)
			return nil
		},
	})
	// Bare /settings lists the resolved config.
	if !dispatch(app, "/settings") {
		t.Fatal("/settings not consumed")
	}
	if len(app.blocks) != 1 || app.blocks[0].Kind != KindSystem {
		t.Fatalf("settings list block = %+v", app.blocks)
	}
	// Nil ops: the display flip still works (App-local state); nothing
	// persists.
	app.SetSettingsOps(nil)
	if !dispatch(app, "/settings showThinking") {
		t.Fatal("/settings showThinking not consumed")
	}
	if app.Thinking() {
		t.Fatal("thinking display still on")
	}
	if len(toggles) != 0 {
		t.Fatalf("nil ops must not persist: %v", toggles)
	}
	// Wired again: explicit off persists through the ops and re-applies.
	app.SetSettingsOps(&SettingsOps{SetThinking: func(on bool) error {
		toggles = append(toggles, on)
		return nil
	}})
	if !dispatch(app, "/settings showThinking off") {
		t.Fatal("/settings showThinking off not consumed")
	}
	if app.Thinking() {
		t.Fatal("thinking display still on after off")
	}
	if len(toggles) != 1 || toggles[0] {
		t.Fatalf("toggles = %v, want [false]", toggles)
	}
	if !dispatch(app, "/settings showThinking on") {
		t.Fatal("/settings showThinking on not consumed")
	}
	if !app.Thinking() {
		t.Fatal("thinking display still off after on")
	}
	if len(toggles) != 2 || !toggles[1] {
		t.Fatalf("toggles = %v, want [false true]", toggles)
	}
}

func TestHelpTextAligned(t *testing.T) {
	got := helpText(builtinCommands())
	lines := strings.Split(got, "\n")
	if !strings.HasPrefix(lines[0], "commands:") {
		t.Fatalf("first line = %q", lines[0])
	}
	// Every entry line: two-space indent, command column padded to 12
	// cells before the description (aligned list).
	want := []string{
		"  /new      start a new session",
		"  /fresh    rotate provider state; keep this session",
		"  /quit, /q quit xdev",
	}
	for _, w := range want {
		if !strings.Contains(got, "\n"+w) {
			t.Errorf("helpText missing aligned row %q:\n%s", w, got)
		}
	}
	if len(lines) != 1+len(builtinCommands()) {
		t.Errorf("helpText has %d lines, want %d", len(lines), 1+len(builtinCommands()))
	}
}

// TestAppCommandHook drives the real submit path: a command is consumed by
// the router (no user block, no onSend) and plain text flows through.
func TestAppCommandHook(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})

	for _, r := range "/help" {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	app.mu.Lock()
	n, kind, text := len(app.blocks), app.blocks[0].Kind, app.blocks[0].Text
	app.mu.Unlock()
	if len(sent) != 0 {
		t.Fatalf("command reached onSend: %v", sent)
	}
	if n != 1 || kind != KindSystem || !strings.HasPrefix(text, "commands:") {
		t.Fatalf("blocks=%d kind=%v text=%q", n, kind, text)
	}

	for _, r := range "make a file" {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	app.mu.Lock()
	n, kind = len(app.blocks), app.blocks[1].Kind
	app.mu.Unlock()
	if len(sent) != 1 || sent[0] != "make a file" {
		t.Fatalf("sent = %v", sent)
	}
	if n != 2 || kind != KindUser {
		t.Fatalf("plain text after command: blocks=%d kind=%v", n, kind)
	}
}

// Stub implementations for the extended CommandAPI surface.
func (f *fakeAPI) OpenTreeSelector()               {}
func (f *fakeAPI) BranchSession(args string) error { return nil }

func (f *fakeAPI) KeyMap() *KeyMap { return DefaultKeyMap() }

func (f *fakeAPI) RunExtensionCommand(name, args string) (string, error) {
	f.extCalls = append(f.extCalls, name+" "+args)
	return "ext output: " + name, f.extErr
}

func (f *fakeAPI) ForkSession() error               { return nil }
func (f *fakeAPI) DumpSession() error               { return nil }
func (f *fakeAPI) ResumeSession(query string) error { return nil }

func (f *fakeAPI) SettingsView(args string) error { return nil }

// TestExtensionCommandDispatch routes "/server:cmd args" to the extension
// runner and prints its output as a system block — the consumer that makes
// announced commands more than handshake metadata.
func TestExtensionCommandDispatch(t *testing.T) {
	app := &fakeAPI{}
	if !dispatch(app, "/policy:ping --now") {
		t.Fatal("extension command was not consumed")
	}
	if len(app.extCalls) != 1 || app.extCalls[0] != "policy:ping --now" {
		t.Fatalf("ext calls = %v", app.extCalls)
	}
	if len(app.blocks) != 1 || !strings.Contains(app.blocks[0], "ext output: policy:ping") {
		t.Fatalf("output not surfaced: %v", app.blocks)
	}
}

// TestExtensionCommandErrorSurfaces: a failing extension command reports
// through the system block rather than vanishing.
func TestExtensionCommandErrorSurfaces(t *testing.T) {
	app := &fakeAPI{extErr: errors.New("extension offline")}
	dispatch(app, "/policy:ping")
	if len(app.blocks) != 1 || !strings.Contains(app.blocks[0], "extension offline") {
		t.Fatalf("error not surfaced: %v", app.blocks)
	}
}

func (f *fakeAPI) Prewalk(args string) error {
	if f.fail == "prewalk" {
		return fmt.Errorf("boom")
	}
	f.prewalk = args
	return nil
}

func (f *fakeAPI) Theme(args string) error {
	if f.fail == "theme" {
		return fmt.Errorf("boom")
	}
	f.theme = args
	return nil
}

func (f *fakeAPI) Memory(args string) error {
	if f.fail == "memory" {
		return fmt.Errorf("boom")
	}
	f.mem = args
	return nil
}

func (f *fakeAPI) Advisor(args string) error {
	if f.fail == "advisor" {
		return fmt.Errorf("boom")
	}
	f.advisor = args
	return nil
}

func (f *fakeAPI) PlanMode(args string) error {
	if f.fail == "plan" {
		return fmt.Errorf("boom")
	}
	f.plan = args
	return nil
}

func (f *fakeAPI) SwitchModel(args string) error {
	if f.fail == "model" {
		return fmt.Errorf("boom")
	}
	if args == "" {
		f.AddSystemBlock("active model: fake/fake\navailable:\n  fake/fake")
	} else {
		f.setModel = args
	}
	return nil
}

// /fresh rotates PROVIDER state only (issue #11 §5): it dispatches the
// app's FreshSession when the host exposes one, and degrades to /new
// semantics otherwise. /new always mints a new session.
func TestDispatchFreshVsNew(t *testing.T) {
	f := &fakeAPI{}
	if !dispatch(f, "/fresh") {
		t.Fatal("/fresh not consumed")
	}
	if f.newed != 1 {
		t.Fatalf("bare CommandAPI /fresh should fall back to new, newed=%d", f.newed)
	}
	fresh := &fakeAPI{}
	if !dispatch(withFresh{fresh}, "/fresh") {
		t.Fatal("/fresh not consumed")
	}
	if fresh.freshed != 1 || fresh.newed != 0 {
		t.Fatalf("/fresh must dispatch FreshSession only, freshed=%d newed=%d", fresh.freshed, fresh.newed)
	}
	if !dispatch(withFresh{fresh}, "/new") {
		t.Fatal("/new not consumed")
	}
	if fresh.newed != 1 || fresh.freshed != 1 {
		t.Fatalf("/new must dispatch NewSession, freshed=%d newed=%d", fresh.freshed, fresh.newed)
	}
}

// withFresh adds the optional FreshSession op to a fakeAPI.
type withFresh struct{ *fakeAPI }

func (w withFresh) FreshSession() error {
	w.freshed++
	return nil
}
