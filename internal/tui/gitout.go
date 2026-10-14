package tui

// Git listing colouring for a bash result — toolhl.go's sibling arm.
//
// Why git needs its own arm rather than the shell one: `git status`, `git log`
// and `git status --short --branch` print no quoted argument and no shell
// control word, so LooksLikeShell refuses them BY CONSTRUCTION — its
// shellInkWords list is the whole test — and every row of the listing a reader
// scans most painted one body ink. (A `git diff` needs nothing from this file:
// app.go routes a unified diff to the diff renderer before any of this runs, so
// its +/- rows keep the bands they already had.)
//
// The judgement is a SHAPE test, not "the command was git". A `git --no-pager
// log`, a `git` inside a pipeline and a wrapper script all print the same rows,
// and a listing that arrives with the word "git" nowhere in its text is still
// the listing — the renderer has the result's own text and that is ground
// truth, exactly as langForToolOutput reads a snapshot's own header.
//
// The bar is two rows carrying git's own vocabulary, over at most gitSampleRows
// sampled. One is not enough because one shape ("<hex> text") is a shape an
// ordinary log line can have too; two of them in the same result is a listing.
// A result that clears nothing paints exactly as it painted before this file
// existed — one run, body ink — which is the fallback every arm of toolhl.go
// keeps, and a false negative costs nothing but the colour.
//
// The detector is a deliberate SUPERSET of the painter. The painter answers
// "which ink does this row's own fact wear?", so it claims the four families
// below. The detector answers "is this git output at all?", so it also counts
// two families that are unmistakably git but carry no fact worth a colour of
// their own — status's section headings and a full-form log's metadata lines —
// and those paint flat. Evidence is not the same question as ink.

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// gitSampleRows is how much of a bash result LooksLikeGit reads, and
// gitMinHits the bar it has to clear. The scan short-circuits the moment the
// bar is clear, so a large result costs a bounded number of single-row shape
// tests — the same bound, and the same reasoning, as shellSampleRows.
const (
	gitSampleRows = 200
	gitMinHits    = 2
)

// LooksLikeGit reports whether a bash result reads as a git listing.
func LooksLikeGit(text string) bool {
	rows, hits := 0, 0
	for line := range strings.SplitSeq(text, "\n") {
		if rows >= gitSampleRows {
			break
		}
		if strings.TrimSpace(line) == "" {
			continue // a blank row carries no evidence either way
		}
		rows++
		if gitEvidence(line) {
			hits++
			if hits >= gitMinHits {
				return true
			}
		}
	}
	return false
}

// gitEvidence reports whether one row carries git's own vocabulary: a shape the
// painter claims, or one of the evidence-only families.
func gitEvidence(src string) bool {
	return gitRowShape(src) || statusSectionHeader(src) != "" ||
		statusTrackingLine(src) != "" || logMetaLabel(src) != ""
}

// gitRowShape reports whether a row is one of the four families the painter
// gives an ink to: a porcelain status row, a long-form status entry, a history
// row's commit hash, or a status branch header.
func gitRowShape(src string) bool {
	if _, _, ok := splitStatusRow(src); ok {
		return true
	}
	if _, _, _, ok := splitStatusEntry(src); ok {
		return true
	}
	if _, _, _, ok := splitLogRow(src); ok {
		return true
	}
	_, _, ok := splitBranchHeader(src)
	return ok
}

// gitRow paints one row of a git listing, or returns it in the box's own ink.
//
// Each row is decided on its own shape, so a listing that mixes the families —
// the "## main" header of a `git status --short --branch` over its porcelain
// rows — paints each row right, and a row that matches nothing keeps the body
// ink. runsString of the result is always src, on every path: highlighting may
// only change which ink a byte wears.
//
// No ink here is chosen outside codeStyle: a label wears the comment slot
// (the quiet chrome ink a snapshot's row prefix and the sidebar's own note rows
// already use), a fact wears the slot its role names. That is what keeps the
// NO_COLOR promise structural — styleFor answers the body ink when the palette
// is off, so the whole arm degrades to one flat run without a second check.
func (a *App) gitRow(src string, cs codeStyle, bodySt, dimSt tcell.Style) line {
	// Porcelain: the two state columns in the keyword ink, the path in the
	// same function ink a grep hit and a glob row give a path.
	if state, path, ok := splitStatusRow(src); ok {
		return line{runs: []cell{
			{text: state, style: cs.styleFor(tokKeyword)},
			{text: path, style: cs.styleFor(tokFunc)},
		}}
	}
	// Long form: the indent is chrome, the label is git's own word, the path is
	// a path.
	if indent, label, path, ok := splitStatusEntry(src); ok {
		return line{runs: []cell{
			{text: indent, style: cs.styleFor(tokPlain)},
			{text: label, style: cs.styleFor(tokComment)},
			{text: path, style: cs.styleFor(tokFunc)},
		}}
	}
	// A history row: the hash is the row's identifier, so it wears the number
	// ink a search row gives its line number, and the label git itself printed
	// ("commit ") is chrome.
	if label, hash, rest, ok := splitLogRow(src); ok {
		runs := make([]cell, 0, 3)
		if label != "" {
			runs = append(runs, cell{text: label, style: cs.styleFor(tokComment)})
		}
		runs = append(runs, cell{text: hash, style: cs.styleFor(tokNumber)})
		if rest != "" {
			runs = append(runs, cell{text: rest, style: bodySt})
		}
		return line{runs: runs}
	}
	// The branch header: the words git wrote are chrome, the branch is the fact.
	// The fact is the row's only run that is not chrome, so it is the one a
	// NO_COLOR build is allowed to keep — it is an ATTRIBUTE (bold), and
	// attributes survive the switch that drops colour (diffStyle's own note).
	// Every other run here wears a colour slot, so styleFor answers the body
	// ink when the palette is off and the row degrades to flat.
	if prefix, name, ok := splitBranchHeader(src); ok {
		return line{runs: []cell{
			{text: prefix, style: cs.styleFor(tokComment)},
			{text: name, style: cs.styleFor(tokType).Bold(true)},
		}}
	}
	return textline(src, bodySt)
}

// gitStatusCode reports whether c is one of porcelain's own state characters,
// one per column: the index state and the working-tree state. '?' and '!' are
// the untracked and ignored states git prints in the first column.
func gitStatusCode(c byte) bool {
	switch c {
	case ' ', 'M', 'A', 'D', 'R', 'C', 'U', 'T', '?', '!':
		return true
	}
	return false
}

// splitStatusRow splits porcelain's "<XY> <path>" row into its two state
// columns and the rest (the separating space included, so the bytes survive).
//
// Both columns must be a status code and at least one must say something: git
// never prints two spaces, which is what keeps a row of a table — "  value" —
// out of this shape. A `git status --porcelain=v2` row opens with its own
// version digit and is not this shape; it paints flat, deliberately.
func splitStatusRow(src string) (state, path string, ok bool) {
	if len(src) < 4 || src[2] != ' ' {
		return "", "", false
	}
	if !gitStatusCode(src[0]) || !gitStatusCode(src[1]) {
		return "", "", false
	}
	if src[0] == ' ' && src[1] == ' ' {
		return "", "", false
	}
	if strings.TrimSpace(src[3:]) == "" {
		return "", "", false
	}
	return src[:2], src[2:], true
}

// statusEntryLabels is git's own word list for the long form's entry rows. The
// set is closed on purpose: a row of prose that happens to read "note:   x" is
// not a status entry, and the only cost of a miss is a flat row.
var statusEntryLabels = map[string]bool{
	"modified": true, "deleted": true, "new file": true, "renamed": true,
	"copied": true, "typechange": true, "unmerged": true, "both modified": true,
	"both deleted": true, "both added": true, "added by us": true,
	"deleted by them": true, "added by them": true, "deleted by us": true,
}

// splitStatusEntry splits a long form entry row — "\tmodified:   path", which
// arrives with its tab already expanded to the renderer's three spaces — into
// the indent, the label and the path.
//
// The label must be one of git's own words, and the row must carry a path after
// it: "modified:" alone is a header, not an entry.
func splitStatusEntry(src string) (indent, label, path string, ok bool) {
	i := 0
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	if i == 0 || i >= len(src) {
		return "", "", "", false
	}
	j := strings.IndexByte(src[i:], ':')
	if j < 0 {
		return "", "", "", false
	}
	word := src[i : i+j]
	if !statusEntryLabels[strings.ToLower(word)] {
		return "", "", "", false
	}
	rest := src[i+j+1:]
	if strings.TrimSpace(rest) == "" {
		return "", "", "", false
	}
	return src[:i], src[i : i+j+1], rest, true
}

// statusSectionHeaders are the headings the long form prints above its entries.
// They are evidence and nothing else: the painter leaves them flat, because a
// heading in the body ink already reads as a heading under a branch header.
var statusSectionHeaders = []string{
	"Changes to be committed:",
	"Changes not staged for commit:",
	"Untracked files:",
	"Unmerged paths:",
}

// statusSectionHeader returns the heading a row is, or "".
func statusSectionHeader(src string) string {
	trimmed := strings.TrimLeft(src, " \t")
	for _, h := range statusSectionHeaders {
		if trimmed == h {
			return h
		}
	}
	return ""
}

// statusTrackingLine reports the sentence the long form prints under its branch
// header about the upstream ("Your branch is up to date with 'origin/main'.",
// and the diverged "Your branch and 'origin/main' have diverged,"). It is
// evidence without a fact of its own — the branch header above it already names
// the branch — so the painter leaves it flat.
func statusTrackingLine(src string) string {
	trimmed := strings.TrimLeft(src, " \t")
	if strings.HasPrefix(trimmed, "Your branch ") {
		return trimmed
	}
	return ""
}

// logMetaLabels are a full-form log's metadata lines. Like the status headings
// they are evidence without a fact of their own.
var logMetaLabels = []string{"Author:", "Date:", "Merge:", "AuthorDate:", "CommitDate:"}

// logMetaLabel returns the metadata label a row opens with, or "".
func logMetaLabel(src string) string {
	trimmed := strings.TrimLeft(src, " \t")
	for _, l := range logMetaLabels {
		if strings.HasPrefix(trimmed, l) && strings.TrimSpace(trimmed[len(l):]) != "" {
			return l
		}
	}
	return ""
}

// splitLogRow splits a history row's leading commit hash from the rest: git
// log's "commit <sha>" and --oneline's "<sha> <subject>".
//
// The hash is a 7..40 character hex run, at a word boundary and followed by a
// space or the end of the row — git's own minimum abbreviation is 7. A shorter
// run is an ordinary word ("faced", "decade"), which is why the length is part
// of the shape rather than a nicety.
func splitLogRow(src string) (label, hash, rest string, ok bool) {
	s := src
	if after, found := strings.CutPrefix(src, "commit "); found {
		label, s = "commit ", after
	}
	n := 0
	for n < len(s) && n < 40 && isHexByte(s[n]) {
		n++
	}
	if n < 7 {
		return "", "", "", false
	}
	if n < len(s) && s[n] != ' ' {
		return "", "", "", false
	}
	return label, s[:n], s[n:], true
}

// splitBranchHeader splits status's branch header: the long form git prints
// with no arguments ("On branch main") and the short one --branch adds
// ("## main...origin/main [ahead 1]"). The tracking half stays part of the
// name: it is one fact about one branch, and splitting it would be a second
// guess for one ink.
func splitBranchHeader(src string) (prefix, name string, ok bool) {
	for _, p := range []string{"On branch ", "## "} {
		if rest, found := strings.CutPrefix(src, p); found && strings.TrimSpace(rest) != "" {
			return p, rest, true
		}
	}
	return "", "", false
}

func isHexByte(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f')
}
