package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/config"
)

// leaderTimeout is how long a pending prefix chord stays armed, matching
// opencode's leader_timeout (2000ms). An expired prefix would otherwise stay
// armed until the next keystroke, turning "I changed my mind" into a chord.
const leaderTimeout = 2 * time.Second

// leaderToken is the "<leader>" spelling a chord may contain. It expands to
// leaderToken is the "<leader>" spelling a chord may contain. It expands to
// the map's leader chord at load, so a keybindings.yml written in opencode's
// syntax works unchanged: `session.new: "<leader>n"`.
const leaderToken = "<leader>"

// KeyMap is the remappable keybinding layer (M10 #11, parity-session-ux §3).
// keybindings.yml maps action-ids to chords; an empty list disables the
// action entirely. Unmapped actions fall back to DefaultKeyMap so a partial
// config never leaves the editor dead.

// KeyMap resolves chords to action ids and back.
type KeyMap struct {
	// bindings maps a chord to an action id. A two-chord binding is stored
	// space-joined ("C-x n"), which is what Resolve probes for.
	bindings map[string]string
	// actions lists the known action ids for /hotkeys display.
	actions []string
	// leader is the prefix chord ("C-x"), or "" when the map binds no pair.
	leader string
	// pending is the armed prefix and its deadline. Kept out of the table and
	// mutated only through Resolve, so the rest of the map stays immutable.
	pending    string
	pendingDue time.Time
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
	"effort-cycle",    // Shift-Tab: lean → standard → full → lean (session effort)
	"thinking-toggle", // request-side reasoning off ⇄ auto (omp alt+t; no default chord)
	"session.tab.next", "session.tab.previous",
	"session.tab.next_unread", "session.tab.previous_unread",
	"session.tab.reopen",      // C-shift-T: reopen the last closed session
	"app.settings",            // Alt+,: the settings overlay (grok settings panel)
	"send-now",                // F6: interrupt the live turn and run a queued message now (#157)
	"app.subagent.transcript", // Alt+B: the newest subagent's own transcript
	// Session lifecycle (opencode session_new / session_list / session_delete),
	// reachable single-key or behind the leader prefix.
	"session.list", "session.new", "session.delete",

	// (contextual: the chord is menu-prev while the slash dropdown is open)
}

// tabSelectDigits are the keys tab 1..10 answer to — 1..9 then 0, exactly
// as opencode spells it (ctrl+1..9, <leader>1..9, ctrl+0 for the tenth).
// init() appends the ten "switch to tab N" ids to BuiltinActions: only the
// digit differs, so a literal ten-line list would be a table something could
// disagree with.
const tabSelectDigits = "1234567890"

// tabSelectPrefix is the action-id prefix for "switch to tab N"; the App's
// dispatch arm cuts the N back off it (tabSelectIndex, tabstrip.go).
const tabSelectPrefix = "session.tab.select."

// tabSelectAction is the action id for "switch to tab N", opencode's
// session_tab_select_N.
func tabSelectAction(n int) string {
	return tabSelectPrefix + strconv.Itoa(n)
}

func init() {
	for i := range len(tabSelectDigits) {
		BuiltinActions = append(BuiltinActions, tabSelectAction(i+1))
	}
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
			// Quit. C-d is deliberately NOT here: opencode reads C-d as
			// session_delete (close this tab) and keeps exit on C-c, and a
			// tab strip with a × on every tab is only honest if the keyboard
			// can close one. C-d closes the current tab; on the last tab
			// that falls back to quit, so one chord still ends the app.
			"C-c": "quit",
			"C-d": "session.delete",
			// opencode's own alias for the same close, and the one chord of
			// that set every terminal delivers.
			"A-w": "session.delete",
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
			// The subagent a `task` call spawned, in that child's own
			// transcript (the panel /hub's Enter opens). Alt+S is the dock's
			// section cycle and A-t is the session tree, so the deliverable
			// chord left in this class is Alt+B ("agent"), the same class
			// Alt+A (the hub) and Alt+M (models) already use. Opening nothing
			// is silence, like ctrl+o on an empty transcript.
			"A-b": "app.subagent.transcript",
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
			// F6: send now (#157). A prompt typed while a turn is running
			// joins the pending list; this interrupts the turn and runs the
			// oldest pending message immediately. It sits next to F5 for the
			// same reason F5 does — both are "act on the live run" keys, and
			// function keys are first-class chords here, so keybindings.yml
			// can move it freely.
			"F6": "send-now",
			// The context dock (#291 §1). Ctrl+B is what the issue asked for and
			// it is taken — scroll-page-up since the pager chords landed — so the
			// panel rides the Alt+letter class the model and hub selectors already
			// use, and keybindings.yml can move it like any other action.
			"A-s": "dock-cycle",
			"A-,": "app.settings",
			"C-t": "dock-fold",

			// Shift-Tab cycles session EFFORT (lean → standard → full). Every
			// terminal sends it as KeyBacktab, which chordOf renders "Shift-Tab".
			// The thinking toggle has no default chord.
			"Shift-Tab": "effort-cycle",
			// Session tabs (opencode session.tab.next / .previous). Alt+letter
			// class matches model-select / hub / dock; ] and [ are the natural
			// "next / prev" pair and reach every terminal we target.
			"A-]": "session.tab.next",
			"A-[": "session.tab.previous",
			"A-}": "session.tab.next_unread", // Shift+] with Alt
			"A-{": "session.tab.previous_unread",
			// opencode's other two spellings of the same pairs, and the ones
			// that survive a config pasted from its tui.json: ESC[1;3B /
			// ESC[1;3A reach tcell as KeyDown/KeyUp + ModAlt ("A-Down" /
			// "A-Up"), and ESC[1;4B / ESC[1;4A carry Shift as well, which
			// chordOf writes before Alt ("S-A-Down" / "S-A-Up"). Those four
			// strings were read out of tcell's own input processor, not
			// guessed — spelled any other way, the binding is listed and dead.
			"A-Down":   "session.tab.next",
			"A-Up":     "session.tab.previous",
			"S-A-Down": "session.tab.next_unread",
			"S-A-Up":   "session.tab.previous_unread",
			// Ctrl+Tab / Ctrl+Shift+Tab are opencode's PRIMARY tab chords
			// ("switch to next open tab" / previous); Alt+] / Alt+[ stay bound
			// because Ctrl+Tab needs a terminal that speaks CSI-u/kitty or
			// modifyOtherKeys to report at all, and a binding the terminal
			// cannot send is a dead key. tcell delivers the pair as
			// KeyTab+ModCtrl and KeyTab+ModCtrl+ModShift (it normalizes
			// Tab+Shift to KeyBacktab only when Ctrl is absent), so the
			// chords are spelled exactly as chordOf renders them.
			"C-Tab":       "session.tab.next",
			"C-Shift-Tab": "session.tab.previous",
			// C-Shift-T is opencode's session_tab_reopen ("reopen last
			// closed tab"). Shift+letter reaches tcell as KeyCtrlT with
			// ModShift, and chordOf writes the letter as lower case with
			// ModShift DROPPED for non-nav keys — so the chord an event
			// renders is exactly the same string C-T already owns. That is
			// why the binding is spelled C-S-t, and it is the same trap
			// TestWrittenChordsReachTheDeliveredChord guards: spelled
			// "C-Shift-T" it would be listed in /hotkeys and never fire.
			// (tcell's CSI-u decoder does report C-Shift-T as its own
			// sequence, which chordOf renders "C-S-t" too.)
			"C-S-t": "session.tab.reopen",
			// history-next, abort and complete share chords with menu/history
			// actions or have no default: context disambiguates at dispatch.
			// They remain settable from keybindings.yml.
			// "abort" → C-c (shared with quit); "complete" → Tab (shared with
			// menu-accept). These share chords because context disambiguates.
			// history-next has no default (Up/Down already recall when the
			// editor is in history mode); "thinking-toggle" lost Shift-Tab
			// to effort-cycle above. Both stay settable from keybindings.yml.
		},
		actions: append([]string(nil), BuiltinActions...),
	}
	// C-x is opencode's leader, and its four pairs are opencode's:
	// C-x n new session, C-x l session list, C-x w close session,
	// C-x q exit. opencode also spells C-x d close; both are bound so
	// neither muscle memory is wrong. A keybindings.yml entry for any of
	// these actions REPLACES its chords, prefix pairs included — the same
	// rule every other action already had.
	m.leader = "C-x"
	m.bindings[m.leader+" n"] = "session.new"
	m.bindings[m.leader+" l"] = "session.list"
	m.bindings[m.leader+" w"] = "session.delete"
	m.bindings[m.leader+" d"] = "session.delete"
	m.bindings[m.leader+" q"] = "quit"
	// <leader>1..9 / <leader>0 are opencode's other spelling of the same
	// jump ("switch to tab N", alongside ctrl+1..9). Both exist there, so
	// both exist here: a remap replaces each action's chords wholesale, and
	// losing the ctrl half would make the action unreachable from a
	// keybindings.yml that only names the leader pair.
	for i := range len(tabSelectDigits) {
		m.bindings[m.leader+" "+string(tabSelectDigits[i])] = tabSelectAction(i + 1)
		m.bindings["C-"+string(tabSelectDigits[i])] = tabSelectAction(i + 1)
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
			// A chord is a list on the wire (opencode's spelling: "C-d",
			// "A-w" and "<leader>w" side by side) and one chord after
			// expansion. Split on the comma so both reach the table the
			// same way, and expand "<leader>" into the leader chord.
			for _, alt := range strings.Split(s, ",") {
				alt = m.expandChord(strings.TrimSpace(alt))
				if alt == "" {
					continue // an empty chord disables that half
				}
				m.bindings[alt] = action
			}
		}
	}
	return m, nil
}

// expandChord turns one written chord into the stored form: opencode's
// "<leader>w" becomes "C-x w", everything else is normalized and trimmed.
// The space is the stored form's own — the table joins a prefix and its
// follow-up with one — so a comma-joined list and a YAML list reach the
// same place.
func (m *KeyMap) expandChord(chord string) string {
	chord = normalizeChord(chord)
	if rest, ok := strings.CutPrefix(chord, leaderToken); ok {
		return m.leader + " " + rest
	}
	return chord
}

// normalizeChord accepts opencode's spelling of the same key: opencode writes
// "ctrl+d" / "alt+w" / "ctrl+shift+tab" / "f5" / "<leader>w"; the table stores
// the form chordOf renders — "C-d" / "A-w" / "C-Shift-Tab" / "F5" / "C-x w".
// A config pasted from an opencode tui.json must resolve without being
// hand-edited, so BOTH halves are folded here — once, at load.
func normalizeChord(chord string) string {
	// Split on the LAST "+": every modifier precedes the key, and Cut's
	// first-plus split read "shift+ctrl+down" as ONE unknown modifier, so the
	// chord was stored verbatim — loaded without complaint, bound to nothing.
	cut := strings.LastIndex(chord, "+")
	if cut < 0 {
		// No modifier. The KEY still needs the table's spelling, or a bare
		// "f5" (opencode writes it that way) sits in the table as "f5" while
		// every event renders "F5". A letter, a digit and punctuation are
		// already spelled the way chordOf writes them.
		if v, ok := writtenKeyNames[strings.ToLower(chord)]; ok {
			return v
		}
		return chord
	}
	var c, a, s bool
	for _, mod := range strings.Split(chord[:cut], "+") {
		switch strings.ToLower(mod) {
		case "ctrl", "c":
			c = true
		case "alt", "a", "meta":
			a = true
		case "shift", "s":
			s = true
		default:
			return chord // not a modifier we know: leave the chord alone
		}
	}
	// The KEY half is spelled opencode's way too, and the table's spelling is
	// not a lowercasing: chordOf writes Tab as "Tab", Backtab as "Shift-Tab",
	// an arrow as "Down". Folding only the modifiers left "ctrl+shift+tab" in
	// the table as "C-shift+tab" — a keybindings.yml line that loads without
	// complaint and binds nothing.
	key := chord[cut+1:]
	if v, ok := writtenKeyNames[strings.ToLower(key)]; ok {
		key = v
	}
	// Shift+Tab arrives as Backtab, whose rendered name ALREADY carries the
	// Shift: the chord is "Shift-Tab" (or "C-Shift-Tab"), never "S-Tab".
	if key == "Tab" && s {
		key, s = "Shift-Tab", false
	}
	// Modifiers are emitted in chordOf's own C, S, A order, so the order they
	// were written in stops mattering: "alt+ctrl+down" and "ctrl+alt+down"
	// are the same chord and normalize to the same key.
	out := ""
	if c {
		out += "C-"
	}
	if s {
		out += "S-"
	}
	if a {
		out += "A-"
	}
	return out + key
}

// writtenKeyNames maps a written key name to the spelling chordOf renders, so
// opencode's lowercase config reaches the chord an event actually delivers.
// "backtab" is deliberately absent: it resolves through "shift+tab", and an
// entry of its own would let "alt+backtab" ask for "A-Shift-Tab".
var writtenKeyNames = map[string]string{
	"tab": "Tab", "enter": "Enter", "return": "Enter", "escape": "Escape", "esc": "Escape",
	"backspace": "Backspace", "delete": "Delete", "del": "Delete",
	"home": "Home", "end": "End", "pgup": "PgUp", "pageup": "PgUp",
	"pgdown": "PgDn", "pagedown": "PgDn",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"f1": "F1", "f2": "F2", "f3": "F3", "f4": "F4", "f5": "F5", "f6": "F6",
	"f7": "F7", "f8": "F8", "f9": "F9", "f10": "F10", "f11": "F11", "f12": "F12",
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
// Resolve maps a key event to an action id ("" when unbound).
//
// Two keystrokes resolve when a prefix is armed: a chord that completes a
// bound pair fires that action, and anything else disarms the prefix and
// resolves on its own — so Ctrl+X then a letter is a chord when one exists,
// and Ctrl+N is still "next line" when none does. That fallback is what lets
// C-x stay the leader without C-x losing whatever it meant before.
func (m *KeyMap) Resolve(ev *tcell.EventKey) string {
	return m.resolve(chordOf(ev), time.Now())
}

// resolve is Resolve with an injected clock, so the prefix timeout is
// testable without a sleep.
func (m *KeyMap) resolve(chord string, now time.Time) string {
	m.ExpirePending(now)
	if m.pending != "" {
		pending := m.pending
		m.pending = ""
		if action, ok := m.bindings[pending+" "+chord]; ok {
			return action
		}
		return m.bindings[chord] // the follow-up stands on its own
	}
	if chord == m.leader && m.leaderArmed() {
		m.pending, m.pendingDue = chord, now.Add(leaderTimeout)
		return ""
	}
	return m.bindings[chord]
}

// leaderArmed reports whether any binding extends the leader prefix, so a map
// with no two-chord action never swallows the key.
func (m *KeyMap) leaderArmed() bool {
	prefix := m.leader + " "
	for chord := range m.bindings {
		if strings.HasPrefix(chord, prefix) {
			return true
		}
	}
	return false
}

// ExpirePending disarms a prefix whose timeout has passed, reporting whether
// it did. The UI loop calls it on its tick so a half-typed pair goes away on
// its own, and Resolve calls it so a late second key is not misread.
func (m *KeyMap) ExpirePending(now time.Time) bool {
	if m.pending == "" || !now.After(m.pendingDue) {
		return false
	}
	m.pending = ""
	return true
}

// Chord returns the display string for an action's preferred binding ("" if
// none). It reads Chords, not the raw table, so the hint a status row shows
// is the portable one rather than whichever chord sorted first.
func (m *KeyMap) Chord(action string) string {
	if all := m.Chords(action); len(all) > 0 {
		return all[0]
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
	case "session.delete":
		// C-d is the chord every terminal sends without a modifier prefix
		// dance, and it is the one opencode reads as session_delete.
		return chord == "C-d"
	case "session.new", "session.list":
		// The leader pair is the only binding these have.
		return strings.Contains(chord, " ")
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
		if r >= '0' && r <= '9' {
			// Digits are bindable for the same reason punctuation is:
			// opencode's "switch to tab N" chords are Ctrl+1..9 and
			// <leader>1..9, and a keyName that returned "" left every one
			// of them a listed chord the runtime silently ignored.
			return string(r)
		}
		// Punctuation is bindable too: a chord like Alt+, (the settings
		// panel) is a real sequence every terminal sends as ESC + the byte,
		// and refusing to name it would leave the binding in the table
		// unresolvable — a listed chord the runtime ignores.
		if r < utf8.RuneSelf && unicode.IsPunct(r) {
			return string(r)
		}
		return ""
	default:
		return ""
	}
}
