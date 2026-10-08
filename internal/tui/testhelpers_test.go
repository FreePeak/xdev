package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// Shared test helpers used by chrome/effort/thinkinglabel tests. Lived in
// mode_test.go while /mode owned the divider; kept here after that surface
// was removed so the helpers stay one place.

// statusRow is the bottom row of a drawn screen (the status line); the
// composer divider sits directly above it.
func statusRow(t *testing.T, scr tcell.SimulationScreen) string {
	t.Helper()
	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) == 0 {
		t.Fatal("nothing drawn")
	}
	return rows[len(rows)-1]
}

func rowOf(t *testing.T, rows []string, needle string) int {
	t.Helper()
	for i, r := range rows {
		if strings.Contains(r, needle) {
			return i
		}
	}
	t.Fatalf("no row carries %q", needle)
	return -1
}

func lastBlock(t *testing.T, app *App) string {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	for i := len(app.blocks) - 1; i >= 0; i-- {
		if app.blocks[i].Kind == KindSystem {
			return app.blocks[i].Text
		}
	}
	t.Fatal("no system block")
	return ""
}
