package tui

import (
	"strings"
	"testing"
)

// The status row must not carry a COUNT of open sessions. "6 tabs" was a
// number nobody could act on — the set is behind the chords and the /tabs
// list — and it read as a wrong count: a directory whose persisted set had
// grown showed six tabs for the one session in view (the user report: "why
// my xdev session TUI display 6 tab while I only open 1 session only?").
//
// What is worth a glance stays: a session that is running, and one that
// produced output the user has not seen.
func TestStatusRowDoesNotCountOpenSessions(t *testing.T) {
	app, _ := newTestApp(t, 110, 34)
	app.SetTabs([]TabInfo{
		{ID: "a", Current: true},
		{ID: "b"},
		{ID: "c"},
	})

	app.mu.Lock()
	text, _ := app.hudSegment("sessions")
	app.mu.Unlock()
	if text != "" {
		t.Fatalf("hudSegment(sessions) = %q, want empty for three idle sessions", text)
	}

	// A running session and one with unread output are the two facts the
	// row still carries — with no tab count anywhere in the text.
	app.SetTabs([]TabInfo{
		{ID: "a", Current: true},
		{ID: "b", Running: true},
		{ID: "c", Unread: true},
	})
	app.mu.Lock()
	text, _ = app.hudSegment("sessions")
	app.mu.Unlock()
	if strings.Contains(text, "tab") {
		t.Fatalf("hudSegment(sessions) = %q, want no tab count", text)
	}
	if !strings.Contains(text, "1 running") || !strings.Contains(text, "1 unread") {
		t.Fatalf("hudSegment(sessions) = %q, want the running and unread marks", text)
	}
}
