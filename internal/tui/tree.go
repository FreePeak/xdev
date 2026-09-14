package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// Session tree selector (M10 #27, omp /tree parity): /tree, double-Escape
// on an empty editor, and the app.session.tree keybinding open an
// interactive entry navigator above the composer instead of a text dump.
// The data (one row per entry) lives in cmd via SetTreeData; the TUI owns
// selection, filters, search, labels and rendering only.

// TreeEntry is one row of the tree selector, precomputed by cmd from the
// session store: depth and the active-leaf flag are graph properties the
// TUI should not re-derive from envelopes.
type TreeEntry struct {
	ID      string // full entry id (what NavigateTree and the store expect)
	Type    string // wire type: message, compaction, model_change, custom, ...
	Role    string // message role (user/assistant/toolResult); "" otherwise
	Summary string // one-line preview
	Depth   int    // indentation level
	Active  bool   // current leaf (rendered with the → bullet)
}

// treeFilter modes cycle in omp order (Ctrl+O forward, Alt+D/T/U/L/A jump).
type treeFilter int

const (
	treeDefault treeFilter = iota
	treeNoTools
	treeUserOnly
	treeLabeledOnly
	treeAll
	numTreeFilters
)

var treeFilterNames = [...]string{"default", "no-tools", "user-only", "labeled-only", "all"}

// keep applies the omp filter semantics: default shows conversational
// nodes and hides bookkeeping (custom/model_change/unknown); no-tools
// additionally hides toolResult messages; user-only keeps user messages;
// labeled-only keeps labeled entries; all keeps everything.
func (f treeFilter) keep(e TreeEntry, labeled bool) bool {
	switch f {
	case treeAll:
		return true
	case treeUserOnly:
		return e.Type == "message" && e.Role == "user"
	case treeLabeledOnly:
		return labeled
	case treeNoTools:
		return e.Role != "toolResult" && e.Type != "custom" && e.Type != "model_change" && e.Type != "unknown"
	default: // treeDefault
		return e.Type != "custom" && e.Type != "model_change" && e.Type != "unknown"
	}
}

// treeMatches is the search predicate: case-insensitive substring over
// id, summary (title) and label.
func treeMatches(e TreeEntry, label, q string) bool {
	return strings.Contains(strings.ToLower(e.ID), q) ||
		strings.Contains(strings.ToLower(e.Summary), q) ||
		strings.Contains(strings.ToLower(label), q)
}

type treeSelector struct {
	entries   []TreeEntry
	sel       int // index into entries (kept on a visible row)
	filter    treeFilter
	query     string
	labelEdit bool   // inline label prompt open
	labelBuf  string // prompt buffer (prefilled with the current label)
}

func (t *treeSelector) visibleIdx(labels map[string]string) []int {
	q := strings.ToLower(t.query)
	var out []int
	for i, e := range t.entries {
		label := labels[e.ID]
		if !t.filter.keep(e, label != "") {
			continue
		}
		if q != "" && !treeMatches(e, label, q) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// move steps the selection over visible rows with wraparound.
func (t *treeSelector) move(labels map[string]string, delta int) {
	vis := t.visibleIdx(labels)
	if len(vis) == 0 {
		return
	}
	pos := 0
	for i, idx := range vis {
		if idx == t.sel {
			pos = i
			break
		}
	}
	t.sel = vis[(pos+delta+len(vis))%len(vis)]
}

// retarget keeps the selection on its entry after a filter/search change,
// snapping to the nearest visible row at-or-after it when filtered out.
func (t *treeSelector) retarget(labels map[string]string) {
	vis := t.visibleIdx(labels)
	if len(vis) == 0 {
		return
	}
	for _, idx := range vis {
		if idx == t.sel {
			return
		}
	}
	next := vis[len(vis)-1]
	for _, idx := range vis {
		if idx >= t.sel {
			next = idx
			break
		}
	}
	t.sel = next
}

func (t *treeSelector) selected() (TreeEntry, bool) {
	if t == nil || t.sel < 0 || t.sel >= len(t.entries) {
		return TreeEntry{}, false
	}
	return t.entries[t.sel], true
}

// SetTreeData wires the entry snapshot for the tree selector (cmd owns the
// session store). Nil or empty → the selector never opens.
func (a *App) SetTreeData(fn func() []TreeEntry) {
	a.mu.Lock()
	a.treeData = fn
	a.mu.Unlock()
}

// SetTreeLabels wires label persistence (the sidecar JSON lives in cmd):
// load refreshes the id→label map when the selector opens; save applies
// one label change (empty string clears).
func (a *App) SetTreeLabels(load func() map[string]string, save func(id, label string) error) {
	a.mu.Lock()
	a.treeLabelLoad, a.treeLabelSave = load, save
	a.mu.Unlock()
}

// OpenTreeSelector shows the tree navigator, starting on the active leaf.
// No-op when the data seam is unwired or the session has no entries.
func (a *App) OpenTreeSelector() {
	a.mu.Lock()
	var entries []TreeEntry
	if a.treeData != nil {
		entries = a.treeData()
	}
	if len(entries) == 0 {
		a.mu.Unlock()
		return
	}
	t := &treeSelector{entries: entries, filter: treeDefault}
	for i, e := range entries {
		if e.Active {
			t.sel = i
			break
		}
	}
	if a.treeLabelLoad != nil {
		a.treeLabels = a.treeLabelLoad()
	}
	a.tpick = t
	a.mu.Unlock()
	a.poke()
}

// TreeSelectorOpen reports whether the tree selector is on screen.
func (a *App) TreeSelectorOpen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tpick != nil
}

// CloseTreeSelector dismisses the selector.
func (a *App) CloseTreeSelector() {
	a.mu.Lock()
	a.tpick = nil
	a.mu.Unlock()
	a.poke()
}

// treeSelect applies Enter/Shift+Enter with omp's navigateTree semantics:
// the leaf lands on the selected entry, except for user rows, which rewind
// to their PARENT and hand the prompt back as a composer draft (Claude-Code
// style resume-and-edit). Re-picking the active leaf is a no-op status, not
// a re-render. The draft is primed only over an empty composer — a parked
// or typed draft is never clobbered. The store move, the optional
// branch_summary, and the transcript restore all live in cmd's NavigateTree.
// The editor is UI-thread-owned: touch it unlocked, like clear-input.
func (a *App) treeSelect(e TreeEntry, summarize bool) {
	if a.ops == nil || a.ops.NavigateTree == nil {
		a.AddSystemBlock("tree: session branch not wired")
		return
	}
	// omp's guard: selecting the leaf you are already at navigates nowhere
	// (a user row still rewinds — its text belongs back in the composer).
	if e.Active && e.Role != "user" {
		a.AddSystemBlock("Already at this point")
		return
	}
	draft, err := a.ops.NavigateTree(e.ID, summarize)
	if err != nil {
		a.AddSystemBlock("tree: " + err.Error())
		return
	}
	a.AddSystemBlock("Navigated to selected point")
	if draft != "" && strings.TrimSpace(a.ed.Text()) == "" {
		a.ed.Reset()
		for _, r := range draft {
			a.ed.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		}
		a.ed.HandleKey(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone))
	}
	a.poke()
}

// handleTreeKey routes keys while the tree selector is open. Returns
// handled=true when the key belonged to the selector (it is modal: every
// key is swallowed, like the session picker).
func (a *App) handleTreeKey(key *tcell.EventKey) (handled bool) {
	if !a.TreeSelectorOpen() {
		return false
	}
	// Ctrl+C is never the modal's to swallow. Every other key is consumed
	// while the selector is open, and the selector can be open while showing
	// nothing (a filter or search that matches no row) — a user who cannot see
	// a panel and cannot quit has a frozen terminal, which is exactly how this
	// was reported. Handing the quit chord back lets handleKey resolve it as
	// cancel-or-quit like everywhere else in the app.
	if key.Key() == tcell.KeyCtrlC {
		return false
	}
	a.mu.Lock()
	t := a.tpick
	labels := a.treeLabels
	switch key.Key() {
	case tcell.KeyUp:
		if !t.labelEdit {
			t.move(labels, -1)
		}
	case tcell.KeyDown:
		if !t.labelEdit {
			t.move(labels, 1)
		}
	case tcell.KeyEnter:
		if t.labelEdit {
			// applyLabel mutates under the lock; the persistence error
			// must surface AFTER unlocking (AddSystemBlock re-locks).
			err := a.applyLabel(t)
			a.mu.Unlock()
			if err != nil {
				a.AddSystemBlock("tree: save label: " + err.Error())
			}
			a.poke()
			return true
		}
		e, ok := t.selected()
		summarize := key.Modifiers()&tcell.ModShift != 0
		a.tpick = nil
		a.mu.Unlock()
		if ok {
			a.treeSelect(e, summarize)
		}
		a.poke()
		return true
	case tcell.KeyEscape:
		switch {
		case t.labelEdit:
			t.labelEdit, t.labelBuf = false, ""
		case t.query != "":
			t.query = "" // first Esc clears search, second closes
		default:
			a.tpick = nil
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if t.labelEdit {
			if r := []rune(t.labelBuf); len(r) > 0 {
				t.labelBuf = string(r[:len(r)-1])
			}
		} else if t.query != "" {
			if r := []rune(t.query); len(r) > 0 {
				t.query = string(r[:len(r)-1])
				t.retarget(labels)
			}
		}
	case tcell.KeyCtrlO:
		if !t.labelEdit {
			t.filter = (t.filter + 1) % numTreeFilters
			t.retarget(labels)
		}
	default:
		m := key.Modifiers()
		switch {
		case m&tcell.ModAlt != 0 && !t.labelEdit && key.Rune() == 's':
			// Alt+S: portable summarize-and-switch. Shift+Enter is the
			// documented chord but plain terminals cannot send it (kitty
			// keyboard protocol only — see chordOf), so the alias keeps
			// the feature reachable everywhere.
			e, ok := t.selected()
			a.tpick = nil
			a.mu.Unlock()
			if ok {
				a.treeSelect(e, true)
			}
			a.poke()
			return true
		case m&tcell.ModAlt != 0 && !t.labelEdit:
			// Alt+D/T/U/L/A jump straight to a filter mode.
			switch key.Rune() {
			case 'd':
				t.filter = treeDefault
			case 't':
				t.filter = treeNoTools
			case 'u':
				t.filter = treeUserOnly
			case 'l':
				t.filter = treeLabeledOnly
			case 'a':
				t.filter = treeAll
			}
			t.retarget(labels)
		case key.Key() == tcell.KeyRune && t.labelEdit:
			t.labelBuf += string(key.Rune())
		case key.Key() == tcell.KeyRune && key.Rune() == 'L' && t.query == "":
			// Shift+L on a row edits its label (prefilled; empty clears).
			// Terminals deliver a shifted letter as the uppercase rune with
			// no ModShift (see chordOf), so the rune alone gates it.
			e, _ := t.selected()
			t.labelEdit = true
			t.labelBuf = labels[e.ID]
		case key.Key() == tcell.KeyRune:
			t.query += string(key.Rune())
			t.retarget(labels)
		}
	}
	a.mu.Unlock()
	a.poke()
	return true
}

// applyLabel commits the inline label prompt: non-empty sets/updates the
// label, empty clears it. Caller holds a.mu — the in-memory map keeps the
// edit even when the sidecar write fails (returned to the caller, which
// is unlocked and may notify). The selector stays consistent either way.
func (a *App) applyLabel(t *treeSelector) error {
	label := strings.TrimSpace(t.labelBuf)
	t.labelEdit, t.labelBuf = false, ""
	e, ok := t.selected()
	if !ok {
		return nil
	}
	if a.treeLabels == nil {
		a.treeLabels = map[string]string{}
	}
	if label == "" {
		delete(a.treeLabels, e.ID)
	} else {
		a.treeLabels[e.ID] = label
	}
	if a.treeLabelSave == nil {
		return nil
	}
	return a.treeLabelSave(e.ID, label)
}

// treeRowText renders one selector row: indentation, active-branch bullet
// (→, like store.Tree), [label], short id, type, summary.
func treeRowText(e TreeEntry, label string) string {
	mark := "  "
	if e.Active {
		mark = "→ "
	}
	lab := ""
	if label != "" {
		lab = "[" + label + "] "
	}
	return strings.Repeat("  ", e.Depth) + mark + lab + e.ID[:min(8, len(e.ID))] + " " + e.Type + " " + e.Summary
}

// drawTreeEmpty renders the selector's panel with no rows: the same chrome as
// a populated panel plus the two gestures that change the row set, so an open
// modal is always visible and never a dead end. Callers hold a.mu.
func (a *App) drawTreeEmpty(yComposerTop int) {
	w := a.width
	s := a.scr
	borderSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
	t := a.tpick
	title := " session tree · filter:" + treeFilterNames[t.filter]
	if t.query != "" {
		title += " · search:" + t.query
	}
	hint := "no rows match · Ctrl+O changes filter · Backspace edits search · Esc closes"
	boxW := min(w-4, 120)
	y := yComposerTop - 4 // title row + hint row + two borders, clearing the composer
	if y < 1 {
		a.tpick = nil // same invariant as above: never open-and-invisible
		return
	}
	box := a.th.Box()
	drawText(s, 2, y, box.TopLeft+strings.Repeat(box.Horizontal, boxW)+box.TopRight, borderSt)
	drawText(s, 4, y, title, dimSt)
	drawText(s, 2, y+1, box.Vertical+strings.Repeat(" ", boxW)+box.Vertical, borderSt)
	drawText(s, 4, y+1, hint, dimSt)
	drawText(s, 2, y+2, box.BottomLeft+strings.Repeat(box.Horizontal, boxW)+box.BottomRight, borderSt)
}

// drawTreeSelector renders the navigator above the composer (same chrome
// as the session picker). Callers hold a.mu (draw does) — no re-locking.
func (a *App) drawTreeSelector(yComposerTop int) {
	t := a.tpick
	if t == nil || len(t.entries) == 0 {
		return
	}
	vis := t.visibleIdx(a.treeLabels)
	if len(vis) == 0 {
		// An open selector that draws nothing is indistinguishable from a
		// frozen terminal: it still swallows the keyboard, so the user has no
		// visible reason why keys do nothing. Paint the panel anyway with the
		// way out.
		a.drawTreeEmpty(yComposerTop)
		return
	}
	selRow := 0
	for i, idx := range vis {
		if idx == t.sel {
			selRow = i
			break
		}
	}
	// Recentered viewport around the selection (omp: up to half the
	// terminal, min 5 rows).
	maxRows := max(5, a.height/2)
	start := max(0, min(selRow-maxRows/2, len(vis)-maxRows))
	idxs := vis[start:min(len(vis), start+maxRows)]
	selRow -= start

	w := a.width
	s := a.scr
	selSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgHighlight)))
	rowSt := tcell.StyleDefault.Background(a.cellColor(a.th.Get(theme.BgBase)))
	borderSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.PromptBorderActive)))
	dimSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))

	title := " session tree · filter:" + treeFilterNames[t.filter]
	if t.query != "" {
		title += " · search:" + t.query
	}
	if t.labelEdit {
		title = " label " + t.entries[t.sel].ID[:min(8, len(t.entries[t.sel].ID))] + ": " + t.labelBuf + "▍ (Enter applies, empty clears)"
	}
	footer := " C-O filter · A-D/T/U/L/A · type to search · S-L label · Enter switch · S-Enter/A-S summarize"

	boxW := min(w-4, 120)
	y := yComposerTop - (len(idxs) + 3) // bottom border must clear the composer's top border row
	if y < 1 {
		// No room above the composer. Closing is the only honest option: a
		// selector that stays open while it cannot paint owns the keyboard,
		// and an invisible panel plus a swallowed quit chord is what the user
		// experienced as a frozen terminal.
		a.tpick = nil
		return
	}
	box := a.th.Box()
	drawText(s, 2, y, box.TopLeft+strings.Repeat(box.Horizontal, boxW)+box.TopRight, borderSt)
	drawText(s, 4, y, title, dimSt)
	y++
	for i, idx := range idxs {
		st := rowSt
		if i == selRow {
			st = selSt
		}
		for x := 2; x < w-2; x++ {
			s.SetContent(x, y, ' ', nil, st)
		}
		mark := "  "
		if i == selRow {
			mark = "❯ "
		}
		drawText(s, 2, y, mark+treeRowText(t.entries[idx], a.treeLabels[t.entries[idx].ID]),
			st.Foreground(a.cellColor(a.th.Get(theme.TextPrimary))))
		y++
	}
	drawText(s, 2, y, box.BottomLeft+strings.Repeat(box.Horizontal, boxW)+box.BottomRight, borderSt)
	drawText(s, 4, y, footer, dimSt)
}
