package tui

import (
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// The Alt+, row and /context write ONE key through ONE door. That is the whole
// point of the row existing: if the overlay had its own vocabulary, a user who
// set 500k in the panel and typed /context would be told two different things.
func TestSettingsOverlayCarriesTheContextWindowRow(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	written := ""
	app.SetSettingsOverlayOps(&SettingsOverlayOps{
		Path: t.TempDir() + "/config.yml",
		Read: func() []SettingsRow {
			return []SettingsRow{
				{Key: "thinking", Label: "Thinking level", Value: "auto",
					Editable: true, Kind: "select", Options: append([]string(nil), config.ThinkingLevels...)},
				{Key: "compaction.contextWindow", Label: "Context window", Value: "auto",
					Editable: true, Kind: "select", Options: append([]string(nil), config.ContextWindowChoices...)},
			}
		},
		Write: func(key, value string) error {
			written = key + "=" + value
			return nil
		},
	})

	rows := app.settingsOverlayOps.Read()
	var row *SettingsRow
	for i := range rows {
		if rows[i].Key == "compaction.contextWindow" {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatal("the overlay has no context-window row")
	}
	// A select, not a toggle and not free text: the vocabulary is a ladder of
	// sizes, and every rung must be reachable from the panel.
	if !row.Editable || row.Kind != "select" {
		t.Fatalf("row = %+v, want an editable select", row)
	}
	if got := row.Options; len(got) != len(config.ContextWindowChoices) {
		t.Fatalf("options = %v, want the whole vocabulary %v", got, config.ContextWindowChoices)
	}

	// Enter on the row cycles to the next option — auto → 200k — and writes
	// the key the rest of xdev reads, not a private one.
	app.OpenSettingsOverlay()
	st := settingsStateOf(app)
	for st.sel != 1 {
		st.sel++
	}
	app.settingsOverlayAction(st)
	if written != "compaction.contextWindow=200k" {
		t.Fatalf("overlay write = %q", written)
	}
}