package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// fakeAPI records CommandAPI calls for dispatch tests.
type fakeAPI struct {
	newed, cleared, dropped, quit int
	blocks                        []string
	sent                          []string
	dir                           string
	fail                          string // method name that returns an error
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

func TestHelpTextAligned(t *testing.T) {
	got := helpText(builtinCommands())
	lines := strings.Split(got, "\n")
	if !strings.HasPrefix(lines[0], "commands:") {
		t.Fatalf("first line = %q", lines[0])
	}
	// Every entry line: two-space indent, command column padded to 12
	// cells before the description (aligned list).
	want := []string{
		"  /new         start a fresh session",
		"  /quit, /q    quit xdev",
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
func (f *fakeAPI) ForkSession() error               { return nil }
func (f *fakeAPI) DumpSession() error               { return nil }
func (f *fakeAPI) ResumeSession(query string) error { return nil }
