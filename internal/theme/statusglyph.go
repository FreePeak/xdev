package theme

// The agent hub's status glyph table (omp's theme__symbols.ts:3-13): one
// cell, one lifecycle state. The table is the whole contract — the paint
// code asks for a mark by state and never draws one itself, so a theme (or a
// test) can swap the marks without touching the TUI.
//
// The four states are the ones omp shows in a roster row: running, idle,
// parked, aborted. A settled child is not a lifecycle state but an OUTCOME
// (done, failed), which the roster's own row carries beside the mark.
var StatusGlyphs = map[string]string{
	"running": "⟳",
	"idle":    "●",
	"parked":  "○",
	"aborted": "⏹",
}

// StatusGlyph returns the one-cell mark for a lifecycle key ("running",
// "idle", "parked", "aborted"). An unknown key has no mark: a row that
// wears nothing is honest about a state this build does not know, where a
// wrong mark would be a lie.
//
// One cell in EVERY terminal: the table holds no wide rune, so the mark
// never shifts the columns to its right.
func StatusGlyph(status string) string {
	return StatusGlyphs[status]
}
