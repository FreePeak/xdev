package tui

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// BlockKind classifies one scrollback block.
type BlockKind int

const (
	KindUser BlockKind = iota
	KindAssistant
	KindThinking
	KindTool     // tool call (name + args summary), status-driven
	KindToolDone // tool result line
	KindSystem   // harness notices (errors, session info)
)

// Block is one scrollback entry. Text is the SOURCE; lines are re-wrapped
// on every draw (reflow on resize for free) with a small per-width cache.
type Block struct {
	Kind     BlockKind
	Text     string // tool call blocks: the raw JSON arguments
	ToolName string // tool blocks: the tool the model called
	Status   string // tool blocks: "running", "ok", "error"
	// CallID is the provider's id for the call this row belongs to. Same-name
	// calls run CONCURRENTLY (agent.MaxToolWorkers), and they finish in any
	// order, so the name alone cannot say which row a result belongs to —
	// the id can. Empty on a replayed row, which pairs back-to-back.
	CallID  string
	Dur     string // tool result blocks: formatted wall time
	Err     bool   // tool result blocks: error result
	Exit    int    // tool result blocks: process exit code, when HasExit
	HasExit bool   // tool result blocks: Exit is a real exit status
	// Truncated marks a result whose tool dropped output the model never saw.
	Truncated bool
	// Diff carries the unified diff of the file change a tool made, when
	// there was one to take. Text stays the model-visible output; the diff
	// exists so the result box can paint added/removed rows instead of a
	// wall of one colour. (edit/write attach it; a `git diff` captured in
	// bash output is painted by detection, not through this field.)
	Diff string
	// Expanded is a result/reasoning box's Ctrl+O state: render every row.
	Expanded bool
	// ThinkOff is a reasoning box's scroll: rows scrolled up from the newest
	// reasoning (0 = newest), mirroring the transcript's own offset.
	ThinkOff int
	stream   bool      // assistant still receiving deltas (dim cursor at tail)
	Ts       time.Time // block timestamp (user/assistant rows, tool start)
	thinkDur time.Duration
	// Sub holds the live activity of a `task` tool's children, in start
	// order. Only the `task` tool ever writes them, so every other block
	// is untouched.
	//
	// These are LINES of the call row, not blocks of their own: a child
	// has no lifecycle the transcript manages (no trim tier, no box, no
	// dock section, no /trajectory record), and a new BlockKind would drag
	// it through all of those for no gain. How many of them PAINT is
	// subVisible's answer; the block keeps them all, so a late settle
	// still lands on the child it belongs to.
	Sub []*SubActivity
	// Live marks a result box a running call is still filling: the tool's own
	// text is absent until it finishes, so without it a running command paints
	// a spinner and nothing else. A live box renders a bounded TAIL window
	// (what is happening now), carries no footer (there is no outcome yet),
	// and settles into an ordinary result when the call ends.
	Live bool
	// liveAt is the paint bucket of a live box's last flush: its stamp moves
	// at most once per livePaint, so a tool emitting thousands of chunks a
	// second costs the frames a reader can actually see. liveSeq backs the
	// render stamp, which has to move on every flush: a progress bar's chunks
	// change the same bytes to the same length.
	liveAt  time.Time
	liveSeq uint64
}

// SubActivity is one child of a `task` call, as the user sees it: which
// child, what it last did, and whether it is still working. Raw by design
// — the row's wording is the render's job, and the naming argument is
// extracted by toolDetail, the same helper the parent's own call rows use.
type SubActivity struct {
	Label  string        // the child's name (`name`, else agent, else "task #N")
	Agent  string        // resolved agent type, when one was named
	Model  string        // the child runs on the session's model
	Tool   string        // the tool it last called ("" before its first call)
	Args   string        // that call's raw JSON arguments
	Status string        // "running" | "ok" | "error"
	Calls  int           // finished tool calls, for the settled summary
	Dur    time.Duration // terminal wall time, set when Status settles
	Ts     time.Time     // when the child started, for its live elapsed
}

// subRowsMax is how many children a batch keeps on screen. ponytail: 3 + a
// "+N more" line. A batch of 8 all expanded would push the parent row off a
// normal terminal, and the report box below already names all 8.
const subRowsMax = 3

// subRow is the width a child row has left after its own glyph and indent.
func subRow(w int) int { return max(8, w-6) }

// Width returns the display width of s in cells.
func width(s string) int { return runewidth.StringWidth(s) }

// truncateCells shortens s to at most maxW display cells, appending ell
// (a single-width "…" by convention) when it had to cut.
func truncateCells(s string, maxW int, ell string) string {
	if width(s) <= maxW {
		return s
	}
	return runewidth.Truncate(s, maxW, ell)
}

// ansiSeq matches CSI (colour/cursor) and simple ESC-prefixed sequences;
// dropping only the ESC byte would leave the printable "[31m" as garbage.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z_]")

// sanitizeOutput makes tool output safe to frame, the way omp's we() +
// control-char strip do before any width math: tabs expand to fixed cells
// (omp's tab stop is 3 spaces) and C0/C1 control characters are dropped —
// raw ANSI escape sequences first, so no printable "[31m" litter survives.
// zero cells, so an unsanitized tab measures 0 but paints as an advance to
// the next 8-column stop: every right border in the box lands elsewhere.
// Newlines are the one control rune kept; carriage returns collapse with it.
func sanitizeOutput(s string) string {
	if strings.IndexByte(s, 0x1b) >= 0 {
		s = ansiSeq.ReplaceAllString(s, "")
	}
	// U+FFFD is the replacement character Go's encoding/json writes when a
	// vendor splices invalid UTF-8 into a JSON string. Dropping it here
	// keeps the thinking box (and every other framed block) free of
	// mojibake for english / mandarin / vietnamese text alike, even when a
	// delta slipped past the wire-layer cleanUTF8.
	if !strings.ContainsFunc(s, func(r rune) bool {
		return r == '\t' || r == '\r' || r == '\uFFFD' || r < 0x20 || (r >= 0x7f && r <= 0x9f)
	}) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("   ")
		case r == '\n':
			b.WriteRune('\n')
		case r == '\r':
			// CRLF: the \n carries the break.
		case r == '\uFFFD':
			// Drop the replacement character — never paint mojibake.
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fitWidth returns s padded (or truncated) to exactly n display cells, so a
// column of box rows share one right edge.
func fitWidth(s string, n int) string {
	if width(s) > n {
		s = truncateCells(s, n, "…")
	}
	return s + strings.Repeat(" ", max(0, n-width(s)))
}

// wrap breaks s into visual lines of at most maxW cells, preserving empty
// lines. A maxW <= 0 yields one line per source line.
func wrap(s string, maxW int) []string {
	if maxW <= 0 {
		return strings.Split(s, "\n")
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		line := strings.TrimRight(para, " \t")
		if width(line) <= maxW {
			out = append(out, line)
			continue
		}
		// Word wrap; hard-break words longer than maxW.
		// Advance rune-by-rune to find the byte index where display
		// width reaches maxW — always at a rune boundary. Using a
		// byte index directly would split multi-byte UTF-8 (CJK,
		// Thai, emoji) into garbled fragments.
		for width(line) > maxW {
			cut := 0
			w := 0
			for _, r := range line {
				rw := runewidth.RuneWidth(r)
				if w+rw > maxW {
					break
				}
				w += rw
				cut += utf8.RuneLen(r)
			}
			if cut == 0 {
				// A single rune wider than maxW (e.g. CJK at
				// maxW=1): hard-break it as a rune boundary.
				r := []rune(line)[0]
				cut = utf8.RuneLen(r)
			}
			if sp := strings.LastIndexAny(line[:cut], " \t"); sp > 0 {
				cut = sp
			}
			out = append(out, strings.TrimRight(line[:cut], " \t"))
			line = strings.TrimLeft(line[cut:], " ")
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// toolArgKeys name the argument that says what a call is ABOUT, in omp's
// precedence (command, path, input) extended with xdev's search and fetch
// tools. The first one present wins.
var toolArgKeys = []string{"command", "path", "file_path", "pattern", "query", "url", "input"}

// toolDetail renders one call's raw JSON arguments as the phrase omp prints
// after the tool name: the first line of the naming field, whitespace
// collapsed, with " …" when the argument continued. Unparseable arguments, or
// ones naming nothing, fall back to the flattened JSON so an unfamiliar tool
// still says something instead of rendering an empty row.
//
// The first line is returned WHOLE. It used to stop at 400 bytes on the
// reasoning that a row is one line wide, but the row wraps now — a command is
// worth reading in full — so there is no width left to guess at. The FIRST
// LINE only is still the rule, so a `write` body or a heredoc never becomes
// the row, and the transcript stays a conversation rather than a log.
//
// JSON gives no promise that a command is valid UTF-8: an `input` field the
// model built by slicing bytes holds a torn rune, and 711 xdev sessions carry
// U+FFFD for exactly that reason. The call row is the one render path with no
// sanitizer on it, so the tear used to reach the terminal — a lone continuation
// byte where every consumer downstream assumes runes. Invalid bytes are
// dropped, not replaced: the row's cell widths stay truthful, or the box
// borders land elsewhere.
func toolDetail(rawArgs string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(rawArgs), &fields) != nil {
		return utf8Only(strings.Join(strings.Fields(rawArgs), " "))
	}
	for _, k := range toolArgKeys {
		v, ok := fields[k]
		var s string
		if !ok || json.Unmarshal(v, &s) != nil {
			continue
		}
		// The whole first line, never a window of it: the call row WRAPS
		// (blockLines, case KindTool), so there is no width to cut to, and a
		// phrase cut at an arbitrary byte is neither readable nor copyable.
		// Invalid bytes are dropped before the first-line cut, so no rune can
		// be torn — the doc comment below says why that matters here.
		head, rest, multiline := strings.Cut(strings.TrimRight(utf8Only(s), "\n"), "\n")
		head = strings.Join(strings.Fields(head), " ")
		if head == "" {
			continue
		}
		if multiline && strings.TrimSpace(rest) != "" {
			head += " …"
		}
		return head
	}
	return utf8Only(strings.Join(strings.Fields(rawArgs), " "))
}

// utf8Only drops invalid bytes. Dropped rather than replaced (U+FFFD): the
// replacement char is one cell wide where the torn bytes were zero, and a row
// whose width is a lie paints its box border in the wrong column.
func utf8Only(s string) string { return strings.ToValidUTF8(s, "") }

// toolSummary splits the call row into the two runs it renders: the tool name
// and the whole of its detail. The detail is NOT clipped here — the row lays it
// out as rows until it is done (blockLines, case KindTool); a phrase cut to a
// guessed width is exactly the truncation that row now exists to not do.
func toolSummary(b *Block) (name, detail string) {
	return b.ToolName, toolDetail(b.Text)
}

// blockAccent picks the rail/accent slot for a block.
func (b *Block) accentSlot() string {
	switch b.Kind {
	case KindUser:
		return "accent_user"
	case KindThinking:
		return "accent_thinking"
	case KindTool, KindToolDone:
		return "accent_tool"
	case KindSystem:
		if strings.Contains(strings.ToLower(b.Text), "error") {
			return "accent_error"
		}
		return "accent_success"
	default:
		return "accent_assistant"
	}
}
