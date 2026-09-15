package tui

import (
	"strings"
	"testing"
)

// TestComposerPlaceholder pins grok's "Type a message…" hint: an empty prompt
// says what to do instead of painting a blank row, and it is gone the moment
// there is a draft — a placeholder that survives under typed text reads as
// part of the user's message.
func TestComposerPlaceholder(t *testing.T) {
	app, scr := drawnApp(t, 60, 14)
	app.draw()
	text := screenText(scr)
	if !strings.Contains(text, "❯ Type a message…") {
		t.Fatalf("empty composer shows no placeholder:\n%s", text)
	}

	setDraft(&app.ed, "hi", 2)
	app.draw()
	text = screenText(scr)
	if strings.Contains(text, "Type a message") {
		t.Fatalf("placeholder must not outlive an empty draft:\n%s", text)
	}
	if !strings.Contains(text, "❯ hi") {
		t.Fatalf("draft misplaced after the placeholder:\n%s", text)
	}
}
