package tui

// The todo list's markers, and the ink each one wears.
//
// A task's state was carried by a four-character marker ("[x]", "[/]", "[ ]",
// "[!]", "[-]") and NOTHING else, on both surfaces that show a task list: the
// context dock's TASKS section (whose rows come from tool.TodoTool.View, the
// same snapshot /plan show prints) and the todo tool's own result box in the
// transcript. A reader scanning a 20-row list had to read five glyphs to find
// the one thing the list is for — what is done, what is being worked on, what
// is stuck — and the marker is exactly the cell that says it.
//
// One home for the answer, because there are two surfaces. The table is keyed by
// the marker GLYPH rather than by tool.TodoStatus: internal/tui does not import
// internal/tool on the paint path, and the glyph is what the renderer actually
// holds — it receives a finished string from View() or from the tool's result
// text, not a typed status. TestTodoMarkersMatchTheToolsOwnGlyphs pins the two
// against each other through the real tool, so the day internal/tool renames a
// marker this file fails rather than painting a list flat in silence.
//
// The inks are syntax palette slots — the same vocabulary every other coloured
// surface in the TUI uses — and each answers "what IS this state?":
//
//	[x] completed    comment       done work recedes, the way a diff's context
//	                               rows do
//	[/] in progress  keyword       the live one, the loudest ink on the row,
//	                               which is what the eye hunts for
//	[!] blocked      string        the palette's own alarm ink (Rosé Pine gold),
//	                               and NOT the error red: a blocked task is
//	                               waiting on a human, not broken
//	[-] dropped      punctuation   the quietest ink that is not already spent
//	[ ] pending      plain         NO ink of its own: pending is the list's
//	                               resting state, not its news. It wears the
//	                               row's own ink deliberately — the palette has
//	                               no sixth ink that is not already saying
//	                               something else, and two states painted the
//	                               same colour is a claim they are the same
//	                               thing. The marker still SPLITS, so the
//	                               brackets stay a marker rather than becoming
//	                               part of the task's text.
//
// The zero value is the flat fallback every arm of this feature keeps: NO_COLOR
// or a theme that pins no syntax_* slot answers the row's own body ink for every
// kind including tokPlain, so the list paints exactly as it painted before this
// file existed.
//
// The todo tool indents its rows (and /plan show copies the same indented
// form), so the marker may follow leading whitespace and the indent stays before
// it in the rendered row, wearing whatever the row is already painted in. The
// dock's rows are never indented, and the same painter serves both.

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// todoMarkerInk maps a task marker's glyph to the token kind whose ink it
// wears. tokPlain is the fifth entry rather than an absence: it is what makes
// "[ ]" a MARKER (the row splits, the brackets are their own run) while wearing
// the row's own ink.
var todoMarkerInk = map[string]tokKind{
	"[x]": tokComment,
	"[/]": tokKeyword,
	"[!]": tokString,
	"[-]": tokPunct,
	"[ ]": tokPlain,
}

// todoMarkerLen is the marker's width in cells: every glyph is "[X]".
const todoMarkerLen = 3

// todoRowInk returns the token kind and the marker's cell offset for one
// task-list row's leading marker, or (zero, 0, false) when the row does not
// open with one.
//
// The marker must be followed by a space, so "[-] a task" is claimed and "[-]x"
// is not: a row of the plan document that happens to open with the same three
// characters is prose, and the plan's rows go through this same painter only
// if one of them opens with a marker.
func todoRowInk(src string) (tokKind, int, bool) {
	i := 0
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	if i+todoMarkerLen+1 > len(src) || src[i+todoMarkerLen] != ' ' {
		return tokPlain, 0, false
	}
	k, ok := todoMarkerInk[src[i:i+todoMarkerLen]]
	if !ok {
		return tokPlain, 0, false
	}
	return k, i, true
}

// todoRow paints one task-list row: the marker in its state's ink, the rest of
// the row in the body ink, preserving any leading indent. A row that does not
// open with a marker keeps the body ink whole — a phase name, the heading, the
// summary line.
//
// runsString of the result is always src: the marker's bytes are unchanged, only
// the ink they wear is.
func (a *App) todoRow(src string, cs codeStyle, bodySt, dimSt tcell.Style) line {
	k, indent, ok := todoRowInk(src)
	if !ok {
		return textline(src, bodySt)
	}
	runs := make([]cell, 0, 4)
	if indent > 0 {
		runs = append(runs, cell{text: src[:indent], style: bodySt})
	}
	runs = append(runs, cell{text: src[indent : indent+todoMarkerLen], style: cs.styleFor(k)})
	runs = append(runs, cell{text: src[indent+todoMarkerLen:], style: bodySt})
	return line{runs: runs}
}

// todoSampleRows is how much of a result LooksLikeTodo reads. One marker row is
// already decisive — no other tool prints one — so this is a bound rather than
// a threshold: a 200k-line result costs at most 200 rows of shape tests, the
// same bound and the same reasoning as shellSampleRows and gitSampleRows.
const todoSampleRows = 200

// LooksLikeTodo reports whether a result reads as a task list: a row opening
// with one of the five markers, after any leading whitespace.
//
// It is a SHAPE test rather than a tool-name test, so the same arm covers the
// todo tool's own result and any surface that prints the list verbatim (/plan
// show's copy, a resumed session's replay). The tool's own ERROR result carries
// no marker row and so paints flat through the ordinary path, which is what
// keeps a "Task %q not found" message out of this arm.
//
// A markdown checklist ("- [x] a task") is NOT claimed: its marker is not at
// the row's start even after the indent.
func LooksLikeTodo(text string) bool {
	rows := 0
	for line := range strings.SplitSeq(text, "\n") {
		if rows >= todoSampleRows {
			break
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		rows++
		if _, _, ok := todoRowInk(line); ok {
			return true
		}
	}
	return false
}

// todoMarkerAt splits a row's (possibly indented) marker from its text, for the
// dock painter, which draws cell runs rather than []cell. ok is false for every
// row that is not a task — a heading, a note, a blank.
func todoMarkerAt(src string) (marker, rest string, k tokKind, ok bool) {
	kind, indent, ok := todoRowInk(src)
	if !ok {
		return "", src, tokPlain, false
	}
	return src[indent : indent+todoMarkerLen], src[indent+todoMarkerLen:], kind, true
}
