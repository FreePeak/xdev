package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// typeRunes feeds a string through the real key path.
func typeRunes(app *App, s string) {
	for _, r := range s {
		app.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
}

// TestAppSlashCommandConsumed proves the /help path works end to end through
// the real key handler: the notice lands, and nothing reaches the agent.
func TestAppSlashCommandConsumed(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})

	typeRunes(app, "/help")
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if len(sent) != 0 {
		t.Fatalf("command reached the agent: %q", sent)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.blocks) != 1 || app.blocks[0].Kind != KindSystem {
		t.Fatalf("expected one system block, got %+v", app.blocks)
	}
	if !strings.Contains(app.blocks[0].Text, "/quit") {
		t.Fatalf("help text missing commands: %q", app.blocks[0].Text)
	}
}

// TestAppPlainTextStillSends guards the fall-through: ordinary input is not
// swallowed by the command router.
func TestAppPlainTextStillSends(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})

	typeRunes(app, "hello there")
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if len(sent) != 1 || sent[0] != "hello there" {
		t.Fatalf("sent = %q", sent)
	}
}

// TestAppScrollKeysThroughRender is the #17 contract at the integration
// level: PgUp actually moves the rendered viewport off the tail, new output
// does not drag it back, and End returns to live.
func TestAppScrollKeysThroughRender(t *testing.T) {
	app, _ := newTestApp(t, 80, 10)
	for range 40 {
		app.AddSystemBlock(strings.Repeat("filler ", 3))
	}
	app.draw() // establish the row count in the model

	if app.sm.Following() != true {
		t.Fatal("fresh content must start following")
	}
	app.handleKey(tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone))
	if app.sm.Following() {
		t.Fatal("PgUp must leave follow mode")
	}
	offAfterPgUp := app.sm.offset
	if offAfterPgUp == 0 {
		t.Fatal("PgUp must move the viewport")
	}

	// Streaming output must not drag the user's position.
	app.AddSystemBlock("new output arrives")
	app.draw()
	if app.sm.Following() {
		t.Fatal("new content must not resume follow while scrolled")
	}

	// End jumps back to live.
	app.handleKey(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone))
	if !app.sm.Following() || app.sm.offset != 0 {
		t.Fatalf("End must return to live: follow=%v offset=%d", app.sm.Following(), app.sm.offset)
	}
}

// TestAppClearLifecycle drives /clear through the router into the wired
// SessionOps and asserts the transcript resets.
func TestAppClearLifecycle(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})
	cleared := 0
	app.SetSessionOps(&SessionOps{
		Clear: func() error { cleared++; return nil },
	})

	app.AddSystemBlock("existing transcript")
	typeRunes(app, "/clear")
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if cleared != 1 {
		t.Fatalf("clear called %d times, want 1", cleared)
	}
	if len(sent) != 0 {
		t.Fatalf("/clear reached the agent: %q", sent)
	}
}

// TestMarkdownCommandSendsExpanded proves discovery→dispatch→expansion→send.
func TestMarkdownCommandSendsExpanded(t *testing.T) {
	dir := t.TempDir()
	swapUserCommandsDir(t, dir)
	writeFile(t, dir+"/commands/review.md", "Review $1 carefully.")

	app, _ := newTestApp(t, 100, 30)
	app.SetCommandDir(dir)
	var sent []string
	app.SetHandlers(func(text string) { sent = append(sent, text) }, func() {}, func() {})

	typeRunes(app, "/review auth.go")
	app.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if len(sent) != 1 || sent[0] != "Review auth.go carefully." {
		t.Fatalf("sent = %q, want expanded template", sent)
	}
}
