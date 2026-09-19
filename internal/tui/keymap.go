package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/config"
)

// KeyMap is the remappable keybinding layer (M10 #11, parity-session-ux §3).
// keybindings.yml maps action-ids to chords; an empty list disables the
// action entirely. Unmapped actions fall back to DefaultKeyMap so a partial
// config never leaves the editor dead.

// KeyMap resolves chords to action ids and back.
type KeyMap struct {
	// bindings maps "C-x" style chord strings to action ids.
	bindings map[string]string
	// actions lists the known action ids for /hotkeys display.
	actions []string
}

// BuiltinActions are the ~20 core actions the keybinding layer manages.
// BuiltinActions are the actions the keybinding layer manages, aligned with
// the DefaultKeyMap chord table. Actions that share a chord with another
// (abort shares C-c with quit; complete shares Tab with menu-accept) are
// omitted from the defaults but still resolvable from a user keybindings.yml.
var BuiltinActions = []string{
	"submit", "newline", "cancel", "quit",
	"scroll-up", "scroll-down", "scroll-top", "scroll-bottom", "scroll-page-up", "scroll-page-down",
	"menu-prev", "menu-next", "menu-accept",
	"history-prev", "history-next",
	"redraw",
	"expand", // Ctrl+O: reveal the newest boxed block (result or reasoning) in full (omp's ctrl+o)
	"clear-input",
	"app.session.tree",
	"model-select",    // Alt+M: the /model selector (omp app.model.select)
	"app.agents.hub",  // Alt+A: the agent-hub roster (omp app.agents.hub)
	"model-cycle",     // Ctrl+P: cycle the active model through --models patterns
	"paste-image",     // Ctrl+V: attach the clipboard image (omp app.clipboard.pasteImage)
	"retry",           // F5: re-run the current session's last turn (omp's retry)
	"dock-cycle",      // Alt+S: the context dock's display policy (#291 §1)
	"dock-fold",       // Ctrl+T: walk the dock's section folds
	"thinking-toggle", // Shift-Tab: request-side reasoning off ⇄ auto (omp alt+t)

	// (contextual: the chord is menu-prev while the slash dropdown is open)
}

// DefaultKeyMap is the factory chord table.
func DefaultKeyMap() *KeyMap {
	m := &KeyMap{
		bindings: map[string]string{
			// Editor / composer. Ctrl+J is the portable newline (every
			// terminal can send 0x0A). The aliases are real chords on the
			// terminals that report them: Alt-Enter via ESC-prefixing,
			// Shift-Enter via kitty/CSI-u (chordOf marks KeyEnter
			// shift-eligible); elsewhere they degrade to plain Enter.
			"Enter":   "submit",
			"C-j":     "newline",
			"A-Enter": "newline",
			"S-Enter": "newline",
			"Escape":  "cancel",
			"C-u":     "clear-input",
			// Quit
			"C-c": "quit",
			"C-d": "quit",
			"Tab": "menu-accept",
			// omp's app.model.cycle: Ctrl+P advances the active model
			// through --models patterns. While the slash dropdown is open
			// the same chord moves its selection, so dispatch decides by
			// context (the same trick as C-c quit/abort).
			"C-p":  "model-cycle",
			"C-n":  "menu-next",
			"Up":   "menu-prev",
			"Down": "menu-next",
			// Scroll
			"PgUp":   "scroll-page-up",
			"PgDn":   "scroll-page-down",
			"C-b":    "scroll-page-up",
			"C-f":    "scroll-page-down",
			"Home":   "scroll-top",
			"End":    "scroll-bottom",
			"S-Up":   "scroll-up",
			"S-Down": "scroll-down",
			// TUI extras
			"C-l": "redraw",
			// omp's ctrl+o: expand the newest boxed block (a result or a
			// reasoning box). The tree selector owns the same chord while it
			// is open (filter cycle); that branch is taken before this action
			// is ever resolved.
			"C-o": "expand",
			// Model selector. omp parity chord (app.model.select); Alt+M
			// is deliverable in every terminal we target.
			"A-m": "model-select",
			// Agent hub roster. Alt+A is the same deliverable chord class
			// as Alt+M (omp's app.agents.hub); it opens the overlay even
			// when no agent is running.
			"A-a": "app.agents.hub",
			"C-r": "history-prev",
			// omp's app.clipboard.pasteImage. This is NOT the ordinary paste —
			// the terminal owns that and bracketed paste delivers it (see
			// paste.go). The chord reaches only for the clipboard's bitmap,
			// which no terminal forwards to an app.
			"C-v": "paste-image",
			"A-t": "app.session.tree",
			// F5: retry the current session — re-run the agent over the store
			// as it stands, so a turn a dropped stream cut short keeps going
			// without a new prompt. omp binds retry to Alt+R; function keys are
			// first-class chords here, so keybindings.yml can move it freely.
			"F5": "retry",
			// The context dock (#291 §1). Ctrl+B is what the issue asked for and
			// it is taken — scroll-page-up since the pager chords landed — so the
			// panel rides the Alt+letter class the model and hub selectors already
			// use, and keybindings.yml can move it like any other action.
			"A-s": "dock-cycle",
			"C-t": "dock-fold",

			// The request-side reasoning toggle (omp's alt+t, Claude Code's
			// Alt+T). Every terminal sends Shift-Tab as KeyBacktab, which
			// chordOf renders "Shift-Tab" — so that is the chord to bind, not
			// the "S-Tab" spelling chordOf can never emit. keybindings.yml can
			// move it like any other action.
			"Shift-Tab": "thinking-toggle",
			// history-next, abort and complete share chords with menu/history
			// actions or have no default: context disambiguates at dispatch.
			// They remain settable from keybindings.yml.
			// Actions with no default chord (listed so /hotkeys shows them).
			// "abort" → C-c (shared with quit); "complete" → Tab (shared with
			// menu-accept). These share chords because context disambiguates.
			// history-next has no default (Up/Down already recall when the
			// editor is in history mode); it stays settable from
			// keybindings.yml.
		},
		actions: append([]string(nil), BuiltinActions...),
	}
	return m
}

// keybindingsPath is <dataDir>/keybindings.yml.
func keybindingsPath() string {
	return filepath.Join(config.DataDir(), "keybindings.yml")
}

// LoadKeyMap reads keybindings.yml, layering user overrides on the
// defaults. A missing file is not an error; a malformed one is.
func LoadKeyMap() (*KeyMap, error) {
	m := DefaultKeyMap()
	raw, err := os.ReadFile(keybindingsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("tui: keybindings.yml: %w", err)
	}
	for action, val := range doc {
		if !isKnownAction(action) {
			continue // unknown actions are ignored (forward-compat: a future
			// build may add actions this one doesn't know yet)
		}
		chords, ok := val.([]any)
		if !ok {
			return nil, fmt.Errorf("tui: keybindings.yml %q: expected a list of chords", action)
		}
		// Remove the default bindings for this action, then re-add.
		m.clearAction(action)
		for _, c := range chords {
			s, ok := c.(string)
			if !ok {
				return nil, fmt.Errorf("tui: keybindings.yml %q: chord must be a string", action)
			}
			m.bindings[s] = action // an empty chord string disables the action
		}
	}
	return m, nil
}

func isKnownAction(name string) bool {
	for _, a := range BuiltinActions {
		if a == name {
			return true
		}
	}
	return false
}

func (m *KeyMap) clearAction(action string) {
	for chord, a := range m.bindings {
		if a == action {
			delete(m.bindings, chord)
		}
	}
}

// Resolve maps a key event to an action id ("" when unbound).
func (m *KeyMap) Resolve(ev *tcell.EventKey) string {
	chord := chordOf(ev)
	if chord == "" {
		return ""
	}
	return m.bindings[chord]
}

// Chord returns the display string for an action's first binding ("" if none).
func (m *KeyMap) Chord(action string) string {
	for _, chord := range m.sortedChords() {
		if m.bindings[chord] == action {
			return chord
		}
	}
	return ""
}

// Hotkeys renders a two-column action/chord table for /hotkeys. Every
// chord bound to an action is listed (not just one), because the table is
// the user's only view of a remap — hiding the aliases would make a
// working binding look broken.
func (m *KeyMap) Hotkeys() string {
	lines := make([]string, 0, len(BuiltinActions))
	maxLen := 0
	for _, a := range BuiltinActions {
		chords := m.Chords(a)
		if len(chords) == 0 {
			chords = []string{"—"} // no binding: action is effectively disabled
		}
		if len(a) > maxLen {
			maxLen = len(a)
		}
		lines = append(lines, fmt.Sprintf("  %-*s  %s", maxLen, a, strings.Join(chords, " / ")))
	}
	return "keybindings (" + keybindingsPath() + "):\n" + strings.Join(lines, "\n")
}

// Chords returns every chord bound to an action, ordered with the
// preferred (most portable) chord first: the primary defaults lead, then
// the remaining chords alphabetically.
func (m *KeyMap) Chords(action string) []string {
	var all []string
	for _, chord := range m.sortedChords() {
		if m.bindings[chord] == action {
			all = append(all, chord)
		}
	}
	var primary, rest []string
	for _, c := range all {
		if isPrimaryChord(action, c) {
			primary = append(primary, c)
			continue
		}
		rest = append(rest, c)
	}
	return append(primary, rest...)
}

// isPrimaryChord names the chord each action advertises first: the one
// that works in every terminal we support. Ctrl+J (0x0A) is deliverable
// everywhere, where Shift-Enter needs the kitty keyboard protocol.
func isPrimaryChord(action, chord string) bool {
	switch action {
	case "newline":
		return chord == "C-j"
	case "submit":
		return chord == "Enter"
	case "cancel":
		return chord == "Escape"
	case "quit":
		return chord == "C-c"
	case "clear-input":
		return chord == "C-u"
	case "app.session.tree":
		return chord == "A-t"
	}
	return false
}

func (m *KeyMap) sortedChords() []string {
	out := make([]string, 0, len(m.bindings))
	for c := range m.bindings {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// chordOf renders a key event as a "C-x" / "S-Tab" / "A-Backspace" style
// chord for table lookup. Only the chords used by the default and user keymaps
// are encoded; others return "".
func chordOf(ev *tcell.EventKey) string {
	var parts []string
	m := ev.Modifiers()
	if m&tcell.ModCtrl != 0 {
		parts = append(parts, "C")
	}
	// Shift distinguishes arrow/nav keys — and Enter on kitty/CSI-u
	// terminals, which report Shift+Enter as a distinct event. Terminals
	// that send plain Enter for Shift+Enter degrade to the Enter chord.
	if m&tcell.ModShift != 0 && ((ev.Key() >= tcell.KeyUp && ev.Key() <= tcell.KeyPgDn) || ev.Key() == tcell.KeyEnter) {
		parts = append(parts, "S")
	}
	if m&tcell.ModAlt != 0 {
		parts = append(parts, "A")
	}
	keyPart := keyName(ev)
	if keyPart == "" {
		return ""
	}
	parts = append(parts, keyPart)
	return strings.Join(parts, "-")
}

// keyName maps a key event to its chord name.
func keyName(ev *tcell.EventKey) string {
	// Ctrl+letter arrives as KeyCtrlA..KeyCtrlZ (not KeyRune+ModCtrl) in
	// real tcell events; normalize to the letter form so keybindings.yml
	// can just say "C-q".
	if k := ev.Key(); k >= tcell.KeyCtrlA && k <= tcell.KeyCtrlZ {
		return string(rune('a' + int(k-tcell.KeyCtrlA)))
	}
	// Function keys arrive as KeyF1..KeyF12 (not KeyRune); name them so a
	// default chord or a keybindings.yml entry can bind F1–F12. They reach
	// every terminal we target, unlike the Shift-Enter alias.
	if k := ev.Key(); k >= tcell.KeyF1 && k <= tcell.KeyF12 {
		return fmt.Sprintf("F%d", int(k-tcell.KeyF1)+1)
	}
	switch ev.Key() {
	case tcell.KeyEnter:
		return "Enter"
	case tcell.KeyEscape:
		return "Escape"
	case tcell.KeyTab:
		return "Tab"
	case tcell.KeyBacktab:
		return "Shift-Tab"
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		return "Backspace"
	case tcell.KeyUp:
		return "Up"
	case tcell.KeyDown:
		return "Down"
	case tcell.KeyLeft:
		return "Left"
	case tcell.KeyRight:
		return "Right"
	case tcell.KeyPgUp:
		return "PgUp"
	case tcell.KeyPgDn:
		return "PgDn"
	case tcell.KeyHome:
		return "Home"
	case tcell.KeyEnd:
		return "End"
	case tcell.KeyDelete:
		return "Delete"
	case tcell.KeyRune:
		r := ev.Rune()
		if r >= 'a' && r <= 'z' {
			return string(r)
		}
		if r >= 'A' && r <= 'Z' {
			return string(r)
		}
		return ""
	default:
		return ""
	}
}
