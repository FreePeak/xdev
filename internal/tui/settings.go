package tui

import (
	"slices"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// SettingsOverlayOps wires the settings overlay to the config layer (lives in
// cmd). Read returns the rows to show — the settings the running session
// actually resolved — and Write persists one changed key. The panel is
// deliberately not a generic config editor: a setting appears here only when
// cmd can also apply it to the live session, so the two callbacks are the
// whole contract and there is no third seam to keep in step.
type SettingsOverlayOps struct {
	// Path is the file a write lands in (the same global layer `xdev config
	// set` edits), named in the panel's footer so the user knows where the
	// change is going.
	Path  string
	Read  func() []SettingsRow
	Write func(key, value string) error
}

// SettingsRow is one line in the overlay: a label, its current value, and how
// an Enter acts on it. The key is the dotted config key the write uses, and it
// is also the row's identity across a refresh — the selection is an index into
// a filtered view, so nothing may key off the index.
type SettingsRow struct {
	Key      string   // dotted config key (e.g. "showThinking")
	Label    string   // display label
	Value    string   // current value as text
	Editable bool     // true = Enter/click changes it
	Kind     string   // "toggle" | "select" | "text"
	Options  []string // select: the values Enter cycles through, in order
}

// settingsOverlayState is the live overlay state (mu-guarded).
type settingsOverlayState struct {
	open       bool
	rows       []SettingsRow
	sel        int
	top        int
	categories []string
	activeCat  int
	// bodyTop/bodyStart/bodyEnd describe the painted row window: block start
	// index in the filter, first screen row, and the one past the last. They
	// are published every frame so a click resolves against the frame the
	// user saw rather than a re-derivation that could disagree with it.
	bodyTop   int
	bodyStart int
	bodyEnd   int
	// bodyRows is how many rows the last frame actually painted. The selection
	// clamps against this rather than the cap, so on a short screen (where the
	// panel is squeezed) the highlight cannot scroll below the visible rows.
	bodyRows int
}

var (
	settingsRegMu sync.Mutex
	settingsReg   = map[*App]*settingsOverlayState{}
)

func settingsStateOf(a *App) *settingsOverlayState {
	settingsRegMu.Lock()
	defer settingsRegMu.Unlock()
	return settingsReg[a]
}

// OpenSettingsOverlay shows the settings overlay.
func (a *App) OpenSettingsOverlay() bool {
	ops := a.settingsOverlayOps
	if ops == nil || ops.Read == nil {
		return false
	}
	rows := ops.Read()
	if len(rows) == 0 {
		return false
	}
	settingsRegMu.Lock()
	settingsReg[a] = &settingsOverlayState{
		open:       true,
		rows:       rows,
		sel:        0,
		top:        0,
		categories: buildSettingsCategories(rows),
		activeCat:  0,
	}
	settingsRegMu.Unlock()
	a.poke()
	return true
}

// CloseSettingsOverlay dismisses the overlay.
func (a *App) CloseSettingsOverlay() {
	settingsRegMu.Lock()
	if st := settingsReg[a]; st != nil {
		st.open = false
	}
	settingsRegMu.Unlock()
	a.poke()
}

// SettingsOverlayOpen reports whether the overlay is on screen.
func (a *App) SettingsOverlayOpen() bool {
	st := settingsStateOf(a)
	return st != nil && st.open
}

// SetSettingsOverlayOps wires the overlay to its config seam.
func (a *App) SetSettingsOverlayOps(ops *SettingsOverlayOps) {
	a.settingsOverlayOps = ops
}

// SettingsOverlay implements CommandAPI /settings overlay: open the panel.
func (a *App) SettingsOverlay() error {
	if !a.OpenSettingsOverlay() {
		a.AddSystemBlock("settings overlay is not wired in this build")
	}
	return nil
}

// settingsLabel renders the label column: one leading space, the label
// clipped to its column, then the gap before the value. rosterSnippet leaves
// the cell count right for a label of CJK width, which is why the clip goes
// through it rather than through a byte slice.
func settingsLabel(label string) string {
	return " " + rosterSnippet(label, settingsLabelW) +
		strings.Repeat(" ", max(1, settingsLabelW-width(rosterSnippet(label, settingsLabelW))+1))
}

const (
	// settingsOverlayMaxRows caps the body so the panel cannot grow past the
	// transcript; the selection scrolls inside it (clampSettingsSel).
	settingsOverlayMaxRows = 12
	// settingsChrome is the rows inside the box the body cannot use: top
	// border, title, tab strip, separator, footer, bottom border.
	settingsChrome = 6
	// settingsLabelW is the label column: fixed, so the values line up in a
	// scannable column instead of trailing each label's own length.
	settingsLabelW    = 22
	settingsPanelMaxW = 80
	settingsPanelMinW = 30
)

// The toggle column's two marks. A value that is neither is drawn as it is
// (see drawSettingsOverlay): a settings file the user hand-edited to `yes`
// must not be painted as "off" while the config layer treats it as on.
const (
	onMark  = "● on"
	offMark = "○ off"
)

// settingsTruthy is the one reading of a boolean setting the overlay paints
// with. go-yaml decodes `yes`/`on`/`true` (and their capitals) into a bool, so
// the config layer has already normalised a hand-edited file to true/false by
// the time a row carries a value — anything else is a value this panel does
// not understand and must not claim to.
func settingsTruthy(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// handleSettingsOverlayKey routes keys while the settings overlay is open.
func (a *App) handleSettingsOverlayKey(key *tcell.EventKey) (handled bool) {
	if key.Key() == tcell.KeyCtrlC {
		return false
	}
	st := settingsStateOf(a)
	if st == nil || !st.open {
		return false
	}
	settingsRegMu.Lock()
	defer settingsRegMu.Unlock()

	// nav moves the selection within the visible rows, so a filtered list
	// cannot walk the cursor onto a row the user cannot see.
	nav := func(d int) {
		n := len(st.visibleRows())
		if n == 0 {
			return
		}
		st.sel = clamp(st.sel+d, 0, n-1)
		clampSettingsSel(st)
	}
	cycleCat := func(d int) {
		if len(st.categories) == 0 {
			return
		}
		st.activeCat = (st.activeCat + d + len(st.categories)) % len(st.categories)
		st.sel, st.top = 0, 0
	}

	switch key.Key() {
	case tcell.KeyUp:
		nav(-1)
	case tcell.KeyDown:
		nav(1)
	case tcell.KeyPgUp:
		nav(-settingsOverlayMaxRows)
	case tcell.KeyPgDn:
		nav(settingsOverlayMaxRows)
	case tcell.KeyHome:
		st.sel, st.top = 0, 0
	case tcell.KeyEnd:
		st.sel = max(0, len(st.visibleRows())-1)
		clampSettingsSel(st)
	case tcell.KeyEnter:
		a.settingsOverlayAction(st)
	case tcell.KeyEsc:
		st.open = false
	case tcell.KeyTab:
		cycleCat(1)
	case tcell.KeyBacktab, tcell.KeyLeft:
		cycleCat(-1)
	case tcell.KeyRight:
		cycleCat(1)
	default:
		switch key.Rune() {
		case 'q':
			st.open = false
		case 'j':
			nav(1)
		case 'k':
			nav(-1)
		case 'g':
			st.sel, st.top = 0, 0
		case 'G':
			st.sel = max(0, len(st.visibleRows())-1)
			clampSettingsSel(st)
		case 'r':
			// Re-read from disk: the overlay is not the only writer (`xdev
			// config set`, an editor, another pane), so a refresh must show
			// what is actually configured rather than a stale snapshot.
			if a.settingsOverlayOps != nil && a.settingsOverlayOps.Read != nil {
				st.rows = a.settingsOverlayOps.Read()
				st.categories = buildSettingsCategories(st.rows)
				st.sel = clamp(st.sel, 0, max(0, len(st.visibleRows())-1))
				clampSettingsSel(st)
			}
		default:
			return false
		}
	}
	a.poke()
	return true
}

// settingsOverlayAction handles Enter on the selected row.
func (a *App) settingsOverlayAction(st *settingsOverlayState) {
	visible := st.visibleRows()
	if st.sel < 0 || st.sel >= len(visible) {
		return
	}
	row := visible[st.sel]
	if !row.Editable {
		return
	}
	ops := a.settingsOverlayOps
	if ops == nil || ops.Write == nil {
		return
	}

	var newVal string
	switch row.Kind {
	case "toggle":
		newVal = toggleBoolValue(row.Value)
	case "select":
		if len(row.Options) == 0 {
			return // a select with nothing to select: nothing to write
		}
		// Cycle to the next option. An unreadable current value starts at the
		// first option rather than wrapping from -1 to the last one, which
		// would make the first press look like a jump backwards.
		next := 0
		if idx := slices.Index(row.Options, row.Value); idx >= 0 {
			next = (idx + 1) % len(row.Options)
		}
		newVal = row.Options[next]
	default:
		return // a row with no kind has no action; Editable alone is not one
	}

	// Persist first, then show it: a failed write must leave the panel showing
	// what is actually configured rather than the value the user hoped for.
	if err := ops.Write(row.Key, newVal); err != nil {
		a.AddSystemBlock("settings: " + err.Error())
		return
	}
	st.setRowValue(row.Key, newVal)
	a.applySettingImmediate(row.Key, newVal)
}

// setRowValue updates a row's displayed value by key: the selection is an
// index into the filtered view, so writing back through that index would
// land on the wrong row. Keying by the config key is the one identity both
// lists share.
func (st *settingsOverlayState) setRowValue(key, value string) {
	for i := range st.rows {
		if st.rows[i].Key == key {
			st.rows[i].Value = value
			return
		}
	}
}

// handleSettingsOverlayMouse routes mouse events in the overlay.
func (a *App) handleSettingsOverlayMouse(m *tcell.EventMouse, press bool) bool {
	st := settingsStateOf(a)
	if st == nil || !st.open {
		return false
	}
	wheel := 0
	switch m.Buttons() {
	case tcell.WheelUp:
		wheel = -1
	case tcell.WheelDown:
		wheel = 1
	}
	if wheel == 0 && !press {
		return false
	}
	settingsRegMu.Lock()
	defer settingsRegMu.Unlock()

	if wheel != 0 {
		// Same index space as the selection: the visible (category-filtered)
		// rows, so a wheel notch cannot move the highlight onto a row that is
		// not on screen.
		st.sel = clamp(st.sel+wheel, 0, max(0, len(st.visibleRows())-1))
		clampSettingsSel(st)
		a.poke()
		return true
	}
	_, y := m.Position()
	// A click on a body row selects it and runs the same action Enter does, so
	// the mouse is a real path to a setting rather than a highlighter.
	if y >= st.bodyStart && y < st.bodyStart+(st.bodyEnd-st.bodyTop) {
		st.sel = st.bodyTop + (y - st.bodyStart)
		if press {
			a.settingsOverlayAction(st)
		}
	}
	a.poke()
	return true
}

// visibleRows is the row list the overlay is currently showing: the category
// filter applied to the full set. The key path and the painter must both go
// through this — the selection is an index into the VISIBLE rows, so indexing
// the unfiltered set (as the first cut did) selects one row and toggles
// another the moment a category tab is active.
func (st *settingsOverlayState) visibleRows() []SettingsRow {
	if st.activeCat <= 0 || st.activeCat >= len(st.categories) {
		return st.rows
	}
	cat := st.categories[st.activeCat]
	out := make([]SettingsRow, 0, len(st.rows))
	for _, r := range st.rows {
		if settingsCategory(r.Key) == cat {
			out = append(out, r)
		}
	}
	return out
}

// clampSettingsSel keeps the selection inside the window the last frame
// painted. The window height is published by the painter (it depends on the
// screen and the category), so this uses it when it is known and falls back to
// the cap before the first frame: an unclamped `top` would leave the highlight
// off the bottom of a squeezed panel, where the user cannot see which row the
// Enter they are about to press belongs to.
func clampSettingsSel(st *settingsOverlayState) {
	rows := st.bodyRows
	if rows <= 0 {
		rows = settingsOverlayMaxRows
	}
	if st.sel < st.top {
		st.top = st.sel
	}
	if st.sel >= st.top+rows {
		st.top = st.sel - rows + 1
	}
	if st.top < 0 {
		st.top = 0
	}
}

// buildSettingsCategories groups rows by a logical prefix.
func buildSettingsCategories(rows []SettingsRow) []string {
	cats := []string{"all"}
	seen := map[string]bool{"all": true}
	for _, r := range rows {
		cat := settingsCategory(r.Key)
		if !seen[cat] {
			seen[cat] = true
			cats = append(cats, cat)
		}
	}
	return cats
}

// settingsCategory maps a dotted key to a category name.
func settingsCategory(key string) string {
	parts := strings.SplitN(key, ".", 2)
	switch parts[0] {
	case "showThinking", "thinking":
		return "reasoning"
	case "theme", "colorBlindMode":
		return "appearance"
	case "sidebarMode", "debugMouse":
		return "ui"
	case "approvalMode", "toolsApproval", "bashPatterns", "bash":
		return "tools"
	case "memory", "memoryPipeline", "memoryMnemopi", "hindsight":
		return "memory"
	case "advisor", "advisorModel", "advisorSyncBacklog":
		return "advisor"
	case "prewalk":
		return "agent"
	case "defaultModel", "models":
		return "model"
	case "retry", "compaction":
		return "advanced"
	default:
		return "other"
	}
}

// toggleBoolValue gives a boolean setting its other value. A value the
// overlay cannot read as a boolean is treated as not-on, so the toggle turns
// it ON: writing `false` over something the user wrote by hand would be the
// panel inventing a state it never showed them.
func toggleBoolValue(v string) string {
	if on, known := settingsTruthy(v); known && on {
		return "false"
	}
	return "true"
}

// applySettingImmediate applies the settings that can take effect without a
// restart. Everything else in the panel is still written — persisting the
// choice is the point — but a value only the next turn reads (thinking, the
// model, approval) cannot be pushed into the running session from here: the
// turn reads it once, at agent construction.
func (a *App) applySettingImmediate(key, value string) {
	truthy := func() bool {
		on, _ := settingsTruthy(value)
		return on
	}
	switch key {
	case "showThinking":
		a.SetShowThinking(truthy())
	case "sidebarMode":
		a.SetDockMode(value)
	case "debugMouse":
		a.SetDebugMouse(truthy())
	}
}

// drawSettingsOverlay renders the settings overlay panel.
func (a *App) drawSettingsOverlay(yComposerTop int) {
	st := settingsStateOf(a)
	if st == nil || !st.open {
		return
	}
	s := a.scr
	w := a.width

	settingsRegMu.Lock()
	rows := st.rows
	sel := st.sel
	top := st.top
	cats := st.categories
	activeCat := st.activeCat
	settingsRegMu.Unlock()

	// The visible set is computed by the same helper the key path uses, so the
	// highlight and the action can never disagree about which row is selected.
	filtered := (&settingsOverlayState{rows: rows, categories: cats, activeCat: activeCat}).visibleRows()
	if len(filtered) == 0 {
		return
	}
	sel = clamp(sel, 0, len(filtered)-1)

	// Bottom-anchored like the other overlays, but content-sized: the box is
	// the rows (capped) plus the chrome it cannot do without. The chrome is
	// named rather than counted with magic offsets, so the painter, the body
	// window and the click hit-test all fall out of the same arithmetic.
	maxBody := max(1, min(len(filtered), settingsOverlayMaxRows))
	h := maxBody + settingsChrome
	if yComposerTop-2 < h {
		// Not enough room above the composer: the panel takes the rows it can
		// and the body scrolls (clampSettingsSel keeps the selection visible)
		// rather than the panel painting off-screen or losing its border.
		h = max(settingsChrome+1, yComposerTop-2)
		maxBody = h - settingsChrome
	}
	// Never above the transcript's own top: a panel that paints over the top
	// bar hides the chrome it belongs to, and one anchored off-screen is
	// invisible while still owning the keyboard.
	y := clamp(yComposerTop-h-1, a.transcriptTop(), max(a.transcriptTop(), yComposerTop-2))
	if room := yComposerTop - y - 1; room > 0 && h > room {
		// The anchor took rows back; give them up so the box still fits.
		h = max(settingsChrome+1, room)
		maxBody = h - settingsChrome
	}
	panelW := min(w-4, settingsPanelMaxW)
	if panelW < settingsPanelMinW {
		return // too narrow to read: the key path still works, painting noise does not
	}
	x0 := max(0, (w-panelW)/2)

	rowBg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	selBg := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
	borderSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	textSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextPrimary)))
	onSt := textSt.Foreground(a.cellColor(a.th.Get(theme.AccentSuccess)))
	offSt := textSt.Foreground(a.cellColor(a.th.Get(theme.AccentError)))

	box := a.th.Box()
	titleW := panelW - 4

	// clear and fill background
	for row := y; row < y+h; row++ {
		fillPanelRows(s, row, row, x0, x0+panelW, rowBg)
	}

	// borders
	drawText(s, x0, y, box.TopLeft+strings.Repeat(box.Horizontal, panelW-2)+box.TopRight, borderSt)
	drawText(s, x0, y+h-1, box.BottomLeft+strings.Repeat(box.Horizontal, panelW-2)+box.BottomRight, borderSt)

	// title
	title := "⚙ SETTINGS"
	drawText(s, x0+1, y+1, rosterSnippet(title, titleW), textSt.Bold(true))

	// category tabs
	tabY := y + 2
	tabText := " "
	for i, cat := range cats {
		if i > 0 {
			tabText += " │ "
		}
		if i == activeCat {
			tabText += "▸" + strings.ToUpper(cat)
		} else {
			tabText += " " + cat
		}
	}
	drawText(s, x0+1, tabY, rosterSnippet(tabText, titleW), dimSt)

	// separator
	drawText(s, x0+1, tabY+1, strings.Repeat(box.Horizontal, panelW-2), borderSt)

	// settings rows
	bodyStart := tabY + 2
	// Rows are exactly one line tall and painted contiguously from bodyStart,
	// so the click hit-test is arithmetic on the frame the user saw — no
	// per-frame row map to fall out of step with what was painted.
	st.bodyTop, st.bodyStart = top, bodyStart
	st.bodyEnd = min(len(filtered), top+max(0, maxBody))
	end := st.bodyEnd
	rowIdx := 0
	for i := top; i < end; i++ {
		row := filtered[i]
		rowY := bodyStart + rowIdx
		if rowY >= y+h-2 {
			break
		}

		style := textSt
		if i == sel {
			style = selBg
			fillPanelRows(s, rowY, rowY, x0+1, x0+panelW-1, selBg)
		}

		// Value ink: toggles read as ● on / ○ off in the success/error accents,
		// so a scan down the column answers "what is on" without reading. An
		// editable select keeps the row's own ink — an accent there would
		// compete with the toggle column for the same meaning.
		val := row.Value
		valStyle := style
		if row.Kind == "toggle" {
			if on, known := settingsTruthy(val); known {
				if on {
					val, valStyle = onMark, onSt
				} else {
					val, valStyle = offMark, offSt
				}
			}
		}

		// One line per row: the label, then the value in its own ink. The
		// value is drawn separately because it carries the on/off colour and
		// the label column is fixed, so the values line up down the panel.
		label := settingsLabel(row.Label)
		drawText(s, x0+1, rowY, rosterSnippet(label, titleW), style)
		if col := x0 + 1 + width(label); col < x0+panelW-1 {
			drawText(s, col, rowY, rosterSnippet(val, x0+panelW-1-col), valStyle)
		}

		rowIdx++
	}
	// The height the key path clamps against is what was actually painted, not
	// what fit the budget: a `break` above (a squeezed panel) is the reason.
	st.bodyRows = rowIdx

	// footer
	footerY := y + h - 2
	footer := "↑↓/jk nav · Enter toggle · Tab category · r refresh · q/Esc close"
	drawText(s, x0+1, footerY, rosterSnippet(footer, titleW), dimSt)
}
