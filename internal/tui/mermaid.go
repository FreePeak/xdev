package tui

// Mermaid ASCII rendering: a ```mermaid fence in assistant text draws as a
// diagram instead of a code band.
//
// Two diagram types render, and the split is deliberate. A sequence diagram is
// a transcript of calls, so it is a lifeline grid: participants are columns,
// messages are rows, and the arrow is the one glyph that carries meaning. A
// flowchart is a graph, so it needs a layout — nodes are ranked by longest
// path from a source, ordered inside a rank by the barycentre of their
// predecessors, and each edge drops a straight elbow between its endpoints.
// Every other mermaid type (class, state, ER, gantt, xychart), and any source
// this parser cannot fully read, falls back to today's code band: a diagram
// drawn wrong is worse than one drawn as source, because a reader cannot tell
// that a line was dropped.
//
// A parse either succeeds whole or is refused whole (parseMermaid returns nil).
// A diagram that does not fit the terminal is redrawn smaller — a sequence
// diagram narrows its columns, then collapses to a call log, and a flowchart
// shrinks its node labels, then falls back to an indented outline — so a
// render never silently loses an edge. The one thing a render may drop is
// geometry the terminal cannot show, and it says so on the last row.
//
// Everything here is bounded the way the rest of the transcript is: a diagram
// occupies at most mermaidMaxRows rows and mermaidMaxNodes nodes, so a
// generated graph cannot flood the scrollback.

import (
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// Bounds on one rendered diagram. None is reachable by a hand-written
// diagram; they exist so a generated one cannot grow without limit.
const (
	mermaidMaxRows     = 240 // visual rows a rendered diagram may occupy
	mermaidMaxLabel    = 32  // display cells a node label may occupy
	mermaidMaxNodes    = 80  // nodes one diagram may declare
	mermaidMaxMessages = 200 // sequence messages one diagram may declare
	mermaidFenceTag    = "mermaid"
)

// --- styling -------------------------------------------------------------

// mmStyles resolves the theme slots a diagram paints with. The mapping is onto
// existing tokens rather than new ones, so a custom theme colours a diagram by
// the same rules that colour a code fence, and the ascii symbol preset (below)
// swaps the whole glyph vocabulary in one expression.
func (a *App) mmStyle() mmStyles {
	return mmStyles{
		wire:  tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.MdMuted))),
		arrow: tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.MdCode))),
		text:  tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary))),
		head:  tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.MdCode))).Bold(true),
		note:  tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim))).Italic(true),
	}
}

type mmStyles struct {
	wire, arrow, text, head, note tcell.Style
}

// Roles index the style each canvas cell paints with, so the drawing code
// writes glyphs and roles and never carries a tcell.Style through a helper.
const (
	mmWire = iota
	mmArrow
	mmText
	mmHead
	mmNote
)

// mmGlyphs is the drawing vocabulary. head/headDot are indexed by direction
// (right, left, down, up); the ascii preset substitutes letters and carets for
// the triangles, because a terminal that cannot draw a box should not be asked
// to draw an arrowhead either.
type mmGlyphs struct {
	hline, vline, dotted, dottedV rune
	head, headDot                 [4]rune
	life                          rune
	noteL, noteR                  rune
	tl, tr, bl, br, bh, bv        rune
}

const (
	dirRight = iota
	dirLeft
	dirDown
	dirUp
)

func (a *App) mmGlyphs() mmGlyphs {
	if a.th != nil && a.th.SymbolPreset() == "ascii" {
		return mmGlyphs{
			hline: '-', vline: '|', dotted: '.', dottedV: ':', life: ':',
			head:    [4]rune{'>', '<', 'v', '^'},
			headDot: [4]rune{'>', '<', 'v', '^'},
			noteL:   '[', noteR: ']',
			tl: '+', tr: '+', bl: '+', br: '+', bh: '-', bv: '|',
		}
	}
	return mmGlyphs{
		hline: '─', vline: '│', dotted: '┄', dottedV: '┆', life: '┆',
		head:    [4]rune{'▶', '◀', '▼', '▲'},
		headDot: [4]rune{'▷', '◁', '▽', '△'},
		noteL:   '║', noteR: '║',
		tl: '┌', tr: '┐', bl: '└', br: '┘', bh: '─', bv: '│',
	}
}

// --- parsing -------------------------------------------------------------

// mmMessage is one sequence event: an arrow between two participants, or a
// note pinned to one participant (or spanning two, for `Note over A,B`).
// kind is '>' solid, ')' dotted, 'x' lost; a note ignores it.
type mmMessage struct {
	from, to int
	kind     byte
	label    string
	note     bool
	spansTwo bool
}

type mmNode struct {
	id, label string
	group     int // subgraph index, -1 for a node that is in no subgraph
}

// mmGroup is one `subgraph`. A group is a frame around the nodes it holds, so
// it costs the drawing a box and a title, not a layout pass of its own.
type mmGroup struct {
	id, label string
}

type mmEdge struct {
	from, to int
	label    string
	dotted   bool
}

// mmGraph is a parsed flowchart: nodes in declaration order and its edges.
type mmGraph struct {
	nodes  []mmNode
	edges  []mmEdge
	groups []mmGroup
}

// mermaidBlock is a parsed diagram, in one of the kinds drawFlow draws. parseMermaid
// returns nil for anything it does not fully understand — that nil is the
// caller's signal to keep the code band.
type mermaidBlock struct {
	seq      bool
	parts    []mmNode
	messages []mmMessage
	graph    mmGraph
	dir      string // flowchart direction: TD or LR
}

// parseMermaid parses mermaid source into a diagram this file can draw. It
// refuses anything it cannot parse in full rather than drawing a partial one.
func parseMermaid(src string) *mermaidBlock {
	lines := mermaidBody(src)
	if len(lines) == 0 {
		return nil
	}
	switch head := strings.TrimSpace(lines[0]); {
	case strings.HasPrefix(head, "sequenceDiagram"):
		return parseSequence(lines)
	case strings.HasPrefix(head, "stateDiagram"):
		return parseState(lines)
	case strings.HasPrefix(head, "flowchart"), strings.HasPrefix(head, "graph"):
		return parseFlowchart(lines)
	}
	return nil
}

// parseState reads a `stateDiagram-v2`. A state diagram is a flowchart spelled
// with different punctuation: a transition is `A --> B: event` rather than
// `A -->|event| B`, `[*]` is the start marker, `state "Long" as id` names a
// state, and `state id { ... }` frames its members as a subgraph does. So each
// line is translated to the flow spelling and handed to the flow reader — one
// graph, one layout, one drawer.
func parseState(lines []string) *mermaidBlock {
	out := []string{"flowchart TD"}
	open := 0 // unclosed `state id {` frames, closed at the end
	for _, raw := range lines[1:] {
		ln := strings.TrimSpace(raw)
		switch {
		case ln == "" || strings.HasPrefix(ln, "%%"):
		case strings.HasPrefix(ln, "direction "):
			if d := strings.ToUpper(afterSpace(ln)); d == "LR" || d == "TD" {
				out = append(out, "direction "+d)
			}
		case hasAnyPrefix(ln, "classDef ", "class ", "note "):
			// Styling has no terminal rendering; the shape does.
		case strings.HasPrefix(ln, "state "):
			rest := afterSpace(ln)
			if i := strings.IndexByte(rest, '{'); i >= 0 {
				if id := strings.TrimSpace(rest[:i]); validName(id) {
					out = append(out, "subgraph "+id)
					open++
				}
				continue
			}
			// `state "Long name" as id` names a state. A bare `state id`
			// says what a node reference already says, so it is dropped.
			i := strings.Index(strings.ToLower(rest), " as ")
			if i < 0 {
				continue
			}
			id := strings.TrimSpace(rest[i+4:])
			if !validName(id) {
				return nil
			}
			out = append(out, id+"["+mermaidText(strings.Trim(strings.TrimSpace(rest[:i]), `"`))+"]")
		case ln == "}" && open > 0:
			open--
			out = append(out, "end")
		default:
			ln = stateMarker(ln)
			if findFlowArrow(ln) < 0 {
				if !validName(ln) {
					return nil
				}
				out = append(out, ln)
				continue
			}
			// The text after the colon is the event that fires the
			// transition, so it becomes the edge label: `A --> B: start`
			// is the flow spelling `A -->|start| B`.
			ev := ""
			if i := strings.IndexByte(ln, ':'); i >= 0 {
				ev = mermaidText(ln[i+1:])
				ln = strings.TrimSpace(ln[:i])
			}
			at, end, _, ok := nextArrow(ln)
			if !ok {
				return nil
			}
			arrow := ln[at:end]
			ln = strings.TrimSpace(ln[:at]) + arrow + "|" + ev + "| " + strings.TrimSpace(ln[end:])
			out = append(out, ln)
		}
	}
	for ; open > 0; open-- {
		out = append(out, "end")
	}
	return parseFlowchart(out)
}

// mermaidBody strips the fence, drops `%%` comments, and trims leading blank
// lines so the diagram's type is the first line. Indentation is kept: a
// mermaid source may indent, and trimming it would hide nothing.
func mermaidBody(src string) []string {
	src = strings.TrimSpace(src)
	if strings.HasPrefix(src, "```") {
		src = strings.TrimPrefix(src, "```")
		if i := strings.IndexByte(src, '\n'); i >= 0 {
			src = src[i+1:]
		}
		if i := strings.LastIndex(src, "```"); i >= 0 {
			src = src[:i]
		}
	}
	var out []string
	for _, ln := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "%%") {
			continue
		}
		out = append(out, strings.TrimRight(ln, " \t"))
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	return out
}

// parseSequence reads a sequence diagram: participant declarations, then
// messages. A participant is declared by `participant`/`actor`, or implicitly
// by being named on a message — the implicit form is what most generated
// diagrams use, and it must render without a declaration.
func parseSequence(lines []string) *mermaidBlock {
	b := &mermaidBlock{seq: true}
	idx := map[string]int{}
	over := false
	participant := func(name string) int {
		name = strings.TrimSpace(name)
		if i, ok := idx[name]; ok {
			return i
		}
		if len(b.parts) >= mermaidMaxNodes {
			over = true
			return 0 // bounded: an over-wide diagram falls back, not hangs
		}
		i := len(b.parts)
		idx[name] = i
		b.parts = append(b.parts, mmNode{id: name, label: name})
		return i
	}
	for _, raw := range lines[1:] {
		ln := strings.TrimSpace(raw)
		switch {
		case ln == "":
		case hasAnyPrefix(ln, "participant ", "actor "):
			name, label := splitDecl(afterSpace(ln))
			if label != "" {
				b.parts[participant(name)].label = label
			}
		case hasAnyPrefix(ln, "autonumber", "activate ", "deactivate ", "box ", "end", "else", "opt ", "par ", "and ", "critical ", "break "):
			// Display directives with no terminal rendering; the messages
			// they wrap still render.
		case hasAnyPrefix(ln, "loop ", "alt ", "rect "):
			// A control block is a labelled frame around the messages it
			// wraps, and the renderer draws a label row rather than a frame.
			// The `end` that closes it costs nothing: the rows already carry
			// the messages in source order, so the block is legible as its
			// header plus its rows.
			b.messages = append(b.messages, mmMessage{
				note: true, from: -1, to: -1, label: mermaidText(afterSpace(ln)),
			})
		case hasPrefixFold(ln, "note "):
			b.messages = append(b.messages, parseNote(ln, participant))
		default:
			m, ok := parseSeqMessage(ln, participant)
			if !ok {
				return nil // a line this parser cannot read: fall back whole
			}
			if len(b.messages) >= mermaidMaxMessages {
				return nil
			}
			b.messages = append(b.messages, m)
		}
	}
	// A participant the source never mentions carries no message, so a
	// column that is only there because of a `participant` line adds width
	// and nothing else. Such a column is dropped — the head it would have
	// drawn is exactly the failure the first pass already showed.
	used := make([]bool, len(b.parts))
	for _, m := range b.messages {
		if m.from >= 0 {
			used[m.from] = true
		}
		if m.to >= 0 {
			used[m.to] = true
		}
	}
	kept := make([]mmNode, 0, len(b.parts))
	remap := make([]int, len(b.parts))
	for i, p := range b.parts {
		if used[i] {
			remap[i] = len(kept)
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	for i := range b.messages {
		if b.messages[i].from >= 0 {
			b.messages[i].from = remap[b.messages[i].from]
		}
		if b.messages[i].to >= 0 {
			b.messages[i].to = remap[b.messages[i].to]
		}
	}
	b.parts = kept
	if over || len(b.messages) == 0 {
		return nil
	}
	return b
}

// splitDecl reads `id as Label` or `id Label`, the two forms a participant
// declaration takes: the first token is the id messages reference, the rest is
// the label the column head shows.
func splitDecl(rest string) (id, label string) {
	rest = strings.TrimSpace(rest)
	if i := strings.Index(strings.ToLower(rest), " as "); i >= 0 {
		return strings.TrimSpace(rest[:i]), strings.TrimSpace(rest[i+4:])
	}
	if f := strings.Fields(rest); len(f) > 1 {
		return f[0], strings.Join(f[1:], " ")
	}
	return rest, ""
}

// parseNote reads `Note over A,B: text` and `Note right of A: text`. A note
// that names no known participant still renders as text: it is a comment in
// the transcript, and losing it would misreport the source.
func parseNote(ln string, participant func(string) int) mmMessage {
	rest := afterSpace(ln)
	kind, rest := cutWord(rest)
	spans := [2]int{-1, -1}
	// `over A,B:` names its participants before the colon; `right of A:` and
	// `left of A:` name one after a two-word phrase. Both spellings are read
	// the same way: cut the names at the colon, keep the rest as the text.
	switch kind {
	case "over":
		names, text := cutAtColon(rest)
		for i, n := range strings.Split(names, ",") {
			if i > 1 {
				break
			}
			spans[i] = participant(n)
		}
		if spans[1] < 0 {
			spans[1] = spans[0]
		}
		rest = text
	default: // right of / left of
		place, tail := cutWord(rest)
		if place != "of" {
			// An unknown note form: keep the text, pin nothing. A note is a
			// comment in the transcript, so dropping it would misreport the
			// source even when its placement is unreadable.
			return mmMessage{note: true, from: -1, to: -1, label: mermaidText(tail)}
		}
		name, text := cutAtColon(tail)
		rest = text
		if n := strings.Split(name, ",")[0]; n != "" {
			spans[0], spans[1] = participant(n), participant(n)
		}
	}
	return mmMessage{
		note: true, from: spans[0], to: spans[1],
		spansTwo: spans[0] >= 0 && spans[0] != spans[1],
		label:    mermaidText(rest),
	}
}

// cutAtColon splits `A,B: text` at its first colon. A note's participant
// list carries no colon of its own, so the first one ends the list — and
// cutting before splitting on words is what stops a colon inside the note's
// own text from being taken for the separator.
func cutAtColon(s string) (head, tail string) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return strings.TrimSpace(s), ""
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
}

// parseSeqMessage reads one `A->>B: text` / `A-->>B: text` / `A--)B: text`
// arrow. The arrow token is the whole grammar: a dash, an optional second dash
// for a dotted line, then the head (`>` solid, `)` dotted, `x` lost). A line
// without that shape is one this parser does not claim to read.
func parseSeqMessage(ln string, participant func(string) int) (mmMessage, bool) {
	// Split at the arrow head first, then read the tails: `A->>B` is a
	// left token (`A->`), a head (`>`) and a right token (`>B`), and the
	// dashes in the left token are what say the line is dotted. Searching for
	// the head is what keeps a label containing a dash (`a--b`) from being
	// mistaken for the arrow.
	h := -1
	for i := 1; i < len(ln); i++ {
		if ln[i] == '>' || ln[i] == ')' || ln[i] == 'x' {
			h = i
			break
		}
	}
	if h <= 0 {
		return mmMessage{}, false
	}
	kind := ln[h]
	// The left token is `A->` / `A-->` / `A-)`: the participant name, then the
	// dashes and the extra head glyph. Stripping only the arrow punctuation
	// keeps a name that itself ends in a dash readable.
	from := strings.TrimRight(ln[:h], " \t")
	// The `+` of `A->>+B` opens an activation and the `-` of `B-->>-A` closes
	// one. There is no activation bar to draw in a terminal, so the marker is
	// dropped rather than taken for part of a participant name.
	from = strings.TrimRight(from, "->x+)0123456789")
	// The right token is `>B: text`: the head glyph, the participant, and the
	// label after the first colon. Cutting at the first colon is what keeps a
	// colon inside the label itself from splitting the participant name.
	to, label := cutColon(ln[h+1:])
	to = strings.TrimLeft(to, ">-+")
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" || !validName(from) || !validName(to) {
		return mmMessage{}, false
	}
	return mmMessage{
		from: participant(from), to: participant(to),
		kind: kind, label: mermaidText(label),
	}, true
}

// stateMarker rewrites the `[*]` marker — the diagram's start state, or its
// end state when it sits at the end of a transition — into a plain node id:
// `[*] --> A` opens at START, `A --> [*]` closes at DONE. Only an endpoint is
// rewritten, so a `[*]` written inside a state name is left alone.
func stateMarker(ln string) string {
	if rest := strings.TrimPrefix(ln, "[*]"); rest != ln {
		return "START" + rest
	}
	if rest := strings.TrimSuffix(ln, "[*]"); rest != ln {
		return rest + "DONE"
	}
	return ln
}

// parseFlowchart reads a flowchart/graph: `direction`, node declarations
// (`A[Label]`, `A(Label)`, `A((Label))`, `A{Label}`, `A>Label]`, bare `A`) and
// edges (`-->`, `---`, `-.->`, `==>`, `--x`) that may carry `|label|` or an
// inline `-- label -->`. A `subgraph` is a frame around its own members, so
// one is drawn rather than refused.
func parseFlowchart(lines []string) *mermaidBlock {
	b := &mermaidBlock{graph: mmGraph{}, dir: "TD"}
	idx := map[string]int{}
	// The group stack: `subgraph` pushes, `end` pops. Nesting is tracked so
	// `end` closes the group it opened and an unclosed one costs nothing —
	// the frame is drawn around whatever landed in it.
	var stack []int
	cur := func() int {
		if len(stack) == 0 {
			return -1
		}
		return stack[len(stack)-1]
	}
	node := func(id string) int {
		if i, ok := idx[id]; ok {
			return i
		}
		i := len(b.graph.nodes)
		idx[id] = i
		b.graph.nodes = append(b.graph.nodes, mmNode{id: id, group: cur()})
		return i
	}
	relabel := func(i int, text string) {
		if text != "" {
			b.graph.nodes[i].label = text
		}
	}
	for _, raw := range lines[1:] {
		ln := strings.TrimSpace(raw)
		switch {
		case ln == "":
		case hasAnyPrefix(ln, "subgraph ", "subgraph["):
			// The header is `subgraph id`, `subgraph [Label]` or
			// `subgraph id[Label]`, and the label may be quoted and hold
			// brackets of its own — so the shape is read with splitNodeDecl,
			// which is the reader that already handles those.
			id, label := afterSpace(ln), ""
			if name, text, ok := splitNodeDecl(id); ok {
				id, label = name, text
			} else if text, name := splitDecl(id); text == "" {
				id, label = name, text
			}
			if id == "" {
				return nil
			}
			stack = append(stack, len(b.graph.groups))
			b.graph.groups = append(b.graph.groups, mmGroup{id: id, label: firstNonEmpty(label, id)})
		case ln == "end" && len(stack) > 0:
			stack = stack[:len(stack)-1]
		case strings.HasPrefix(ln, "direction "):
			if d := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(ln, "direction "))); d == "LR" || d == "TD" {
				b.dir = d
			}
		case hasAnyPrefix(ln, "classDef ", "class ", "style ", "linkStyle ", "click "):
			// Styling and links have no terminal rendering; the shape does.
		default:
			for _, stmt := range splitTopLevel(ln) {
				if !b.addFlowStmt(stmt, node, relabel) {
					return nil
				}
			}
		}
	}
	if len(b.graph.nodes) == 0 || len(b.graph.nodes) > mermaidMaxNodes {
		return nil
	}
	return b
}

// firstNonEmpty returns s when it is non-empty, else alt.
func firstNonEmpty(s, alt string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return alt
}

// addFlowStmt adds one flowchart statement: an edge chain (`A --> B --> C`,
// with `|label|` or `-- label -->` on any hop), a node on its own (`A[Label]`),
// or a declaration trailing an edge (`A --> B` then `C[D]`). It reports false
// for a statement it could not read, which the caller turns into a whole-block
// fallback.
func (b *mermaidBlock) addFlowStmt(stmt string, node func(string) int, relabel func(int, string)) bool {
	stmt = strings.TrimSpace(stmt)
	if stmt == "" {
		return true
	}
	if findFlowArrow(stmt) < 0 {
		// No arrow: the statement is one or more bare declarations. Splitting
		// on `;` already happened in the caller, so what is left is a single
		// reference such as `A` or `B[Build request]`.
		id, text, ok := splitNodeDecl(stmt)
		if !ok {
			return false
		}
		relabel(node(id), text)
		return true
	}
	return b.addFlowChain(stmt, node, relabel)
}

// addFlowChain walks an edge chain, splitting it at each arrow: every hop is
// `head arrow label? target`, and a target that is itself followed by an arrow
// is the next hop's head. So `A --> B --> C` is two edges off three nodes.
func (b *mermaidBlock) addFlowChain(stmt string, node func(string) int, relabel func(int, string)) bool {
	rest := strings.TrimSpace(stmt)
	hopped := false
	for {
		at, end, arrowLabel, ok := nextArrow(rest)
		if !ok {
			if rest == "" {
				return hopped // the last hop already consumed everything
			}
			// A trailing declaration with no arrow of its own (`A --> B`
			// then `C[D]`). With nothing hopped before it, the statement
			// held no edge at all.
			if !hopped {
				return false
			}
			id, text, ok := splitNodeDecl(rest)
			if !ok {
				return false
			}
			relabel(node(id), text)
			return true
		}
		dotted := flowDotted(rest[at:end])
		// `A -- yes --> B` hangs the label off the head side. A bare `-->`
		// leaves nothing there but whitespace, and a `--` inside a node label
		// (`A[pre-trade]`) is not a delimiter — flowLabelDelim decides.
		headText := strings.TrimSpace(rest[:at])
		if i := flowLabelDelim(headText); i >= 0 {
			if lab := flowLabelText(headText[i:]); lab != "" {
				arrowLabel = lab
			}
			headText = strings.TrimSpace(headText[:i])
		}
		id, text, ok := splitNodeDecl(headText)
		if !ok {
			return false
		}
		src := node(id)
		relabel(src, text)
		rest = strings.TrimSpace(rest[end:])
		// `A -->|yes| B` writes the label after the arrow instead.
		if strings.HasPrefix(rest, "|") {
			j := strings.IndexByte(rest[1:], '|')
			if j < 0 {
				return false
			}
			arrowLabel = mermaidText(rest[1 : 1+j])
			rest = strings.TrimSpace(rest[2+j:])
		}
		// The target is what sits between this arrow and the next one, so
		// that offset is what splits `A --> B --> C` into its hops. `rest`
		// is left alone, because the next pass has to find that same arrow
		// again with the target as its head.
		target := rest
		if next, _, _, more := nextArrow(rest); more {
			target = strings.TrimSpace(rest[:next])
		}
		id, text, ok = splitNodeDecl(target)
		if !ok {
			return false
		}
		dst := node(id)
		relabel(dst, text)
		b.graph.edges = append(b.graph.edges, mmEdge{
			from: src, to: dst, label: arrowLabel, dotted: dotted,
		})
		if len(b.graph.edges) > mermaidMaxNodes*2 {
			return false
		}
		hopped = true
	}
}

// nextArrow returns the byte range of the first edge arrow in s that sits
// outside every bracket and quote, plus the label written inside it. It
// reports false when there is no arrow, which is what tells the caller a
// statement is a bare node declaration rather than an edge.
//
// The labelled dotted form is the reason this is a scan and not a token list:
// `A -. isolated .-> B` has its closing `.->` as far away as the label is long,
// so the run of dots after the label is what ends the arrow.
func nextArrow(s string) (at, end int, label string, ok bool) {
	top := flowTop(s)
	for i := 0; i < len(s); i++ {
		if !top[i] {
			continue
		}
		if s[i] == '.' {
			// `.->` is the short form of `-.->`: dotted line, solid head.
			if strings.HasPrefix(s[i:], ".->") {
				return i, i + 3, "", true
			}
			continue
		}
		if s[i] != '-' {
			continue
		}
		// The unlabelled tokens, longest first so `-.->` wins over `-.` and
		// `---` over `--`.
		for _, tok := range []string{"-.->", "-->", "==>", "---", "--x", "--o"} {
			if strings.HasPrefix(s[i:], tok) {
				return i, i + len(tok), "", true
			}
		}
		// `-. label .->` opens on a dash-dot and closes on the next top-level
		// `.->`, so the label is whatever sits between them.
		if strings.HasPrefix(s[i:], "-.") {
			if close, width := nextClose(s, i+2, top); close > 0 {
				return i, close + width, mermaidText(s[i+2 : close]), true
			}
		}
		// `-- label -->` opens on a run of dashes and closes on the next
		// top-level `-->`.
		if close, width := nextClose(s, i+2, top); close > 0 {
			return i, close + width, mermaidText(s[i+2 : close]), true
		}
	}
	return 0, 0, "", false
}

// nextClose returns the offset of the next top-level `.->` or `-->` at or
// after from, and its width, or (0, 0). Both spellings open on their own
// character — `.->` on a dot, `-->` on a dash — so both are probed at every
// top-level byte rather than one.
func nextClose(s string, from int, top []bool) (int, int) {
	for i := from; i < len(s); i++ {
		if !top[i] {
			continue
		}
		for _, tok := range []string{".->", "-->"} {
			if strings.HasPrefix(s[i:], tok) {
				return i, len(tok)
			}
		}
	}
	return 0, 0
}

// flowLabelDelim returns the offset of the last `--`, `-.` or `==` run in an
// edge head, or -1. A run inside a node label (`A[pre-trade]`) does not
// delimit anything, so the scan tracks bracket depth; and a single `-` is an
// id character (`pre-trade`), so only a run of two or more opens a label.
func flowLabelDelim(s string) int {
	at, depth := -1, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '(', '{':
			depth++
		case ']', ')', '}':
			depth--
		case '-', '.', '=':
			if depth == 0 && i+1 < len(s) && s[i+1] == s[i] {
				at = i
				i++ // a run of three is one delimiter
			}
		}
	}
	return at
}

// flowDotted reports whether an arrow token draws as a dotted line. It looks
// at the token's closing head, not at its whole text: an inline label brings
// its own letters with it, and the `o` in `A -- go --> B` is not a dotted head.
func flowDotted(arrow string) bool {
	for _, tok := range []string{".->", "--x", "--o", "x--", "o--"} {
		if strings.HasSuffix(arrow, tok) {
			return true
		}
	}
	return false
}

// flowLabelText reads the label off the closing dashes of an inline edge
// (`A -- yes --> B` has `yes ` left at the head). The dashes and dots are the
// arrow the scan already consumed, so they are trimmed off the tail.
func flowLabelText(s string) string {
	return mermaidText(strings.TrimRight(strings.TrimLeft(s, " 	-."), " 	-."))
}

// flowTop marks each byte of a statement that sits outside every bracket and
// quote. The arrow scan consults it so a `->` inside a node label is never
// taken for an edge.
func flowTop(s string) []bool {
	top := make([]bool, len(s))
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '(' || c == '{':
			depth++
		case c == ']' || c == ')' || c == '}':
			depth--
		default:
			top[i] = depth == 0
		}
	}
	return top
}

// findFlowArrow reports whether s holds an edge, by asking nextArrow.
func findFlowArrow(s string) int {
	at, _, _, ok := nextArrow(s)
	if !ok {
		return -1
	}
	return at
}

// splitNodeDecl reads a node reference: `A`, `A[Label]`, `A(Label)`,
// `A((Label))`, `A{Label}`, `A>Label]`, `A[[Label]]`, `A[/Label/]`, or a
// quoted `A["Label"]`. Unbalanced brackets are the one case a shape renderer
// must not guess at, so they are refused.
func splitNodeDecl(s string) (id, label string, ok bool) {
	s = strings.TrimSpace(s)
	open := strings.IndexAny(s, "[({<")
	if open <= 0 {
		if s == "" || strings.ContainsAny(s, "]}>)") {
			return "", "", false
		}
		return s, "", true
	}
	id = strings.TrimSpace(s[:open])
	if id == "" || !validName(id) {
		return "", "", false
	}
	rest := s[open:]
	closer := map[byte]byte{'[': ']', '(': ')', '{': '}', '<': '>'}[rest[0]]
	if strings.HasPrefix(rest, "((") {
		closer = ')' // a double shape: the inner ) closes it, not the last
		if i := strings.IndexByte(rest[1:], ')'); i >= 0 {
			return id, mermaidText(strings.Trim(rest[2:1+i], "/")), true
		}
	}
	end := strings.LastIndexByte(rest, closer)
	if end <= 0 {
		return "", "", false
	}
	return id, mermaidText(strings.Trim(strings.TrimSpace(rest[1:end]), "/|")), true
}

// splitTopLevel splits a flowchart line on `;`, ignoring separators inside
// brackets or quotes — `A[x;y] --> B` is one statement.
//
// The depth guard tracks the brackets a label may actually use: mermaid writes
// a label as `[text]`, `(text)`, `((text))` or `{text}`, and `>`/]` close
// those. Arrowheads are consumed before this runs, so a `>` left over from
// `-->` can only belong to a shape — counting it as an opener is what made
// every label after an arrow swallow the rest of the line.
func splitTopLevel(s string) []string {
	var out []string
	depth, quote, start := 0, byte(0), 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '(' || c == '{':
			depth++
		case c == ']' || c == ')' || c == '}':
			depth--
		case c == ';' && depth <= 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// validName reports whether s can be a node or participant id: letters,
// digits, and the punctuation mermaid allows inside a bare id.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if !strings.ContainsRune("_-.", r) {
			return false
		}
	}
	return true
}

// mermaidText normalises a label: `<br/>` becomes a space (a terminal box has
// no inline line break) and the quotes and code ticks a mermaid label may
// carry are dropped. It deliberately does NOT strip `_` or `*` on their own:
// the labels agents write are full of snake_case and file:line, and eating an
// underscore turns `GET /get_ids` into `GET /getids` — a diagram that lies
// about the thing it documents. Whole-label emphasis (`**bold**`) is trimmed
// instead, which is the only case worth losing marks for.
func mermaidText(s string) string {
	for _, m := range []string{"<br/>", "<br />", "<br>", "<b>", "</b>", "<i>", "</i>"} {
		s = strings.ReplaceAll(s, m, " ")
	}
	s = strings.Join(strings.Fields(s), " ")
	for _, m := range []string{"**", "__", "`"} {
		s = strings.Trim(s, m)
	}
	return strings.Trim(s, `"'`)
}

// --- entry point ---------------------------------------------------------

// mermaidLines renders a mermaid fence's body, or nil when the block is not a
// diagram this file draws (an unsupported type, a source that failed to
// parse, or an empty diagram). The caller keeps the code band on nil.
func (a *App) mermaidLines(src string, w int) []line {
	if w < 8 {
		return nil
	}
	blk := parseMermaid(src)
	if blk == nil {
		return nil
	}
	gl := a.mmGlyphs()
	var rows []string
	var roles [][]byte
	if blk.seq {
		rows, roles = drawSequence(blk, w, gl)
	} else {
		rows, roles = drawFlow(blk, w, gl)
	}
	if len(rows) == 0 {
		return nil
	}
	st := [...]tcell.Style{
		mmWire:  a.mmStyle().wire,
		mmArrow: a.mmStyle().arrow,
		mmText:  a.mmStyle().text,
		mmHead:  a.mmStyle().head,
		mmNote:  a.mmStyle().note,
	}
	// The canvas is indexed by RUNE, not byte: cutting a row into runs by
	// byte offset slices a multi-byte box-drawing character in half, and the
	// transcript then paints a replacement character where the wire should
	// be. So each row is walked as []rune and the run's text is built from
	// whole runes.
	out := make([]line, 0, len(rows))
	for r, row := range rows {
		ln := line{runs: make([]cell, 0, 4)}
		for c := 0; c < len(roles[r]); {
			role := roles[r][c]
			end := c
			for end < len(roles[r]) && roles[r][end] == role {
				end++
			}
			ln.runs = append(ln.runs, cell{
				text:  string([]rune(row)[c:end]),
				style: st[role],
			})
			c = end
		}
		out = append(out, ln)
	}
	return out
}

// --- the canvas ----------------------------------------------------------

// grid is the character canvas a diagram is drawn on: one rune per cell plus
// one role per cell, so a drawing helper writes glyphs and roles and never
// carries a style.
type grid struct {
	cells [][]rune
	roles [][]byte
}

func newGrid() *grid { return &grid{} }

// paint writes a glyph unconditionally: the box, the wire and the head are
// each drawn in one pass and the last writer is the visible one.
func (g *grid) paint(x, y int, r rune, role byte) {
	if x < 0 || y < 0 || y >= mermaidMaxRows {
		return
	}
	for len(g.cells) <= y {
		g.cells = append(g.cells, []rune{})
		g.roles = append(g.roles, []byte{})
	}
	row := g.cells[y]
	for len(row) <= x {
		row = append(row, ' ')
		g.roles[y] = append(g.roles[y], mmText)
	}
	// The growth may have reallocated, so the canvas takes the row back
	// before the write: writing through a stale header would drop every
	// glyph past the old capacity and leave the diagram blank.
	g.cells[y] = row
	row[x] = r
	g.roles[y][x] = role
}

// text writes a run of text, one row per source line, starting at (x, y). It
// returns the row after the last one written.
func (g *grid) text(x, y int, s string, role byte) int {
	for _, seg := range strings.Split(s, "\n") {
		for i, r := range []rune(seg) {
			g.paint(x+i, y, r, role)
		}
		y++
	}
	return y
}

// textUnder writes text only into blank cells, so a message label that runs
// across a participant's column interrupts that lifeline instead of being
// sliced in half by it.
func (g *grid) textUnder(x, y int, s string, role byte) int {
	for _, seg := range strings.Split(s, "\n") {
		for i, r := range []rune(seg) {
			if g.blank(x+i, y) {
				g.paint(x+i, y, r, role)
			}
		}
		y++
	}
	return y
}

// blank reports whether the canvas cell (x, y) is empty or does not exist yet.
func (g *grid) blank(x, y int) bool {
	if x < 0 || y < 0 || y >= len(g.cells) {
		return true
	}
	return x >= len(g.cells[y]) || g.cells[y][x] == ' '
}

// rows returns the canvas as strings and their per-cell roles, each row
// trimmed of the blank right margin the canvas grew it to independently.
func (g *grid) rows() ([]string, [][]byte) {
	out := make([]string, 0, len(g.cells))
	roles := make([][]byte, 0, len(g.cells))
	for y := range g.cells {
		runes := g.cells[y]
		last := len(runes)
		for last > 0 && runes[last-1] == ' ' {
			last--
		}
		out = append(out, string(runes[:last]))
		rr := make([]byte, last)
		copy(rr, g.roles[y][:last])
		roles = append(roles, rr)
	}
	return out, roles
}

// hspan draws a horizontal run of r from x for n cells.
func (g *grid) hspan(x, y, n int, r rune, role byte) {
	for i := 0; i < n; i++ {
		g.paint(x+i, y, r, role)
	}
}

// vspan draws a vertical run of r from y for n rows.
func (g *grid) vspan(x, y, n int, r rune, role byte) {
	for i := 0; i < n; i++ {
		g.paint(x, y+i, r, role)
	}
}

// --- sequence layout -----------------------------------------------------

// seqGutter is the blank column between two lifelines; seqLifeMin is the
// narrowest a lifeline column may be.
const (
	seqGutter  = 1
	seqLifeMin = 3
)

// drawSequence lays a sequence diagram out as a lifeline grid: a boxed head
// per participant, a lifeline per column, and one row per message whose label
// follows the arrow head.
//
// A message's wire is drawn on its own row above its label, because an arrow
// sharing a row with its own text cannot draw a head at the end of the line
// that text then crosses. A self-message becomes a two-row hook off its own
// lifeline, and a note becomes a `║ … ║` band on the participant it covers.
func drawSequence(blk *mermaidBlock, w int, gl mmGlyphs) ([]string, [][]byte) {
	n := len(blk.parts)

	// A lifeline column must fit its participant's whole name: a column that
	// can only show `ord…` tells the reader nothing, and shrinking four such
	// columns to fit is how a diagram becomes four truncated boxes. So the
	// width is the labels' own width, and a diagram that does not fit at that
	// width becomes a call log instead (drawSequenceLog) — the geometry goes,
	// the content stays.
	colW := make([]int, n)
	for i, p := range blk.parts {
		colW[i] = max(width(p.label)+2, seqLifeMin)
	}
	if sumInts(colW)+(n-1)*seqGutter > w {
		return drawSequenceLog(blk, w)
	}

	// Life x: the centre of each column, so a head box straddles its lifeline.
	lifeX := make([]int, n)
	x := 0
	for i := range colW {
		lifeX[i] = x + colW[i]/2
		x += colW[i] + seqGutter
	}
	// (the head boxes, the message rows and the lifelines are painted below)
	g := newGrid()
	// Heads: a three-row box per participant, label centred on the lifeline.
	for i, p := range blk.parts {
		label := p.label
		if inner := colW[i] - 2; width(label) > inner {
			label = truncateCells(label, inner, "…")
		}
		x0 := lifeX[i] - colW[i]/2
		g.paint(x0, 0, gl.tl, mmWire)
		g.paint(x0+colW[i]-1, 0, gl.tr, mmWire)
		g.hspan(x0+1, 0, colW[i]-2, gl.bh, mmWire)
		g.paint(x0, 1, gl.bv, mmWire)
		g.paint(x0+colW[i]-1, 1, gl.bv, mmWire)
		g.text(x0+1+(colW[i]-2-width(label))/2, 1, label, mmHead)
		g.paint(x0, 2, gl.bl, mmWire)
		g.paint(x0+colW[i]-1, 2, gl.br, mmWire)
		g.hspan(x0+1, 2, colW[i]-2, gl.bh, mmWire)
	}

	// Messages start below a blank row under the heads.
	y := 4
	for _, m := range blk.messages {
		if y >= mermaidMaxRows-2 {
			break
		}
		if m.note {
			y = drawSeqNote(g, blk, m, lifeX, y, w, gl)
			continue
		}
		from, to := lifeX[m.from], lifeX[m.to]
		if from == to {
			y = drawSeqSelf(g, m, from, y, w, gl)
			continue
		}
		// The wire is drawn on its own row above the label: an arrow that
		// shares a row with its own text cannot draw the head at the end of
		// the line the text then crosses.
		wireY := y
		labelY := y + 1
		glyph, head := gl.hline, gl.head[dirRight]
		if to < from || m.kind == ')' || m.kind == 'x' {
			glyph = gl.dotted
		}
		if to < from {
			head = gl.head[dirLeft]
			if m.kind == ')' || m.kind == 'x' {
				head = gl.headDot[dirLeft]
			}
		} else if m.kind == ')' || m.kind == 'x' {
			head = gl.headDot[dirRight]
		}
		lo, hi := min(from, to), max(from, to)
		g.hspan(lo, wireY, hi-lo, glyph, mmWire)
		g.paint(to, wireY, head, mmArrow)
		// The label hangs off the end the head points at, so a reply reads
		// back toward its sender the way the source wrote it.
		lx := lo
		if to > from {
			lx = to + 2
		} else {
			lx = max(0, from-width(m.label)-2)
		}
		budget := max(w-lx, 1)
		segs := wrap(m.label, budget)
		g.textUnder(lx, labelY, segs[0], mmText)
		for i, seg := range segs[1:] {
			if labelY+i+1 >= mermaidMaxRows-1 {
				break
			}
			g.textUnder(lx, labelY+i+1, seg, mmText)
		}
		// The wire of the NEXT message is two rows below the last label row,
		// so no arrow ever shares a row with a label.
		y = labelY + len(segs) + 1
	}
	// Lifelines last: painted with textUnder, so a label that runs across a
	// participant's column interrupts the dashes instead of being cut by them.
	for i := range blk.parts {
		for row := 3; row < y; row++ {
			if g.blank(lifeX[i], row) {
				g.paint(lifeX[i], row, gl.life, mmWire)
			}
		}
	}
	if y >= mermaidMaxRows-1 {
		g.text(0, mermaidMaxRows-1, "… diagram truncated (mermaidMaxRows)", mmNote)
	}
	return g.rows()
}

// drawSeqSelf draws a self-message as a two-row hook off one lifeline: out
// along the top row to the head, back along the bottom row.
func drawSeqSelf(g *grid, m mmMessage, x, y, w int, gl mmGlyphs) int {
	g.paint(x+1, y, gl.tl, mmWire)
	g.hspan(x+2, y, 2, gl.hline, mmWire)
	g.paint(x+4, y, gl.head[dirRight], mmArrow)
	g.paint(x+1, y+1, gl.bl, mmWire)
	g.hspan(x+2, y+1, 2, gl.hline, mmWire)
	g.paint(x+4, y+1, gl.br, mmWire)
	label := m.label
	if budget := max(w-(x+6), 1); width(label) > budget {
		label = truncateCells(label, budget, "…")
	}
	g.text(x+6, y, label, mmText)
	return y + 3
}

// drawSeqNote draws a note as a `║ … ║` band across the participants it
// covers. A note that names no participant is plain italic text, still on its
// own row so the message order around it stays readable.
func drawSeqNote(g *grid, blk *mermaidBlock, m mmMessage, lifeX []int, y, w int, gl mmGlyphs) int {
	if m.from < 0 {
		g.text(0, y, "║ "+m.label+" ║", mmNote)
		return y + 2
	}
	name := blk.parts[m.from].label
	lo := lifeX[m.from]
	if m.spansTwo && m.to < len(lifeX) {
		lo, hi := min(lifeX[m.from], lifeX[m.to]), max(lifeX[m.from], lifeX[m.to])
		body := name + ": " + m.label
		if width(body) <= hi-lo-2 {
			// It fits between the two lifelines: centre it and close the
			// band on the second participant, which is what the source said.
			g.paint(lo, y, gl.noteL, mmNote)
			g.paint(hi, y, gl.noteR, mmNote)
			g.text(lo+1+(hi-lo-1-width(body))/2, y, body, mmNote)
			return y + 2
		}
	}
	body := name + ": " + m.label
	g.paint(lo, y, gl.noteL, mmNote)
	budget := max(w-lo-3, 1)
	if width(body) > budget {
		body = truncateCells(body, budget, "…")
	}
	g.text(lo+2, y, body, mmNote)
	g.paint(lo+2+width(body), y, gl.noteR, mmNote)
	return y + 2
}

// drawSequenceLog is the narrow-terminal collapse: one row per message with
// the endpoints spelled out. It is a fallback inside the renderer rather than
// the code band, because the information the diagram carried — who called
// whom, in what order, saying what — survives even when the geometry does not.
func drawSequenceLog(blk *mermaidBlock, w int) ([]string, [][]byte) {
	g := newGrid()
	y := 0
	// Each message is two or more rows: the endpoints and the arrow, then the
	// label on its own indented rows. Inlining the label after the arrow
	// (`from ->> to: label`) looks tidier at 120 columns and destroys the
	// content at 40 — the label ends up hard-broken mid-word with nothing to
	// say where the break belongs. Here the label wraps as prose, at whatever
	// width is left, and nothing is ever truncated.
	put := func(role byte, s string, indent int) {
		for _, seg := range wrap(s, max(w-indent, 1)) {
			if y >= mermaidMaxRows-1 {
				return
			}
			g.text(indent, y, seg, role)
			y++
		}
	}
	for _, m := range blk.messages {
		if y >= mermaidMaxRows-1 {
			break
		}
		if m.note {
			head := "note"
			if m.from >= 0 {
				head = blk.parts[m.from].label
			}
			put(mmNote, "║ "+strings.TrimSpace(head+": "+m.label)+" ║", 0)
			continue
		}
		from, to := blk.parts[m.from].label, blk.parts[m.to].label
		head := "──▶"
		switch {
		case m.kind == ')' || m.kind == 'x':
			head = "╌╌▶"
		case m.from == m.to:
			head = "▶"
		}
		put(mmText, from+" "+head+" "+to, 0)
		if m.label != "" {
			put(mmText, m.label, 2)
		}
	}
	return g.rows()
}

// sumInts and shrinkCols are the fit arithmetic: a diagram that does not fit
// the terminal gives up its slack before it gives up a node, down to a floor
// every column keeps.
func sumInts(v []int) int {
	n := 0
	for _, x := range v {
		n += x
	}
	return n
}

// shrinkCols takes n columns of preferred width and fits them into avail
// cells, each keeping at least floor. It returns nil when even the floors do
// not fit, which is the caller's signal to use a different layout.
func shrinkCols(pref []int, avail, floor int) []int {
	n := len(pref)
	if avail < n*floor {
		return nil
	}
	extra := avail - n*floor
	sumQ, sumExtra := 0, 0
	for _, v := range pref {
		if q := v - floor; q > 0 {
			sumQ += q
		}
	}
	out := make([]int, n)
	for i, v := range pref {
		q := 0
		if sumQ > 0 {
			q = max(0, v-floor) * extra / sumQ
		}
		out[i] = floor + q
		sumExtra += q
	}
	// Hand the rounding remainder out one cell at a time.
	for rem := extra - sumExtra; rem > 0; rem-- {
		out[rem%len(out)]++
	}
	return out
}

// --- flow layout ---------------------------------------------------------

// Box geometry for a flowchart node: 3 rows tall (top, label, bottom), and as
// wide as its label plus the two border columns.
const (
	flowBoxH  = 3 // box rows: top rule, label, bottom rule
	flowGapX  = 3 // columns between two boxes in the same rank
	flowGapY  = 4 // rows between two ranks: label row, elbow row, head row
	flowGapLR = 3 // columns between two ranks in LR
)

type flowBox struct{ x, y, w int }

func (b flowBox) midX() int { return b.x + b.w/2 }
func (b flowBox) midY() int { return b.y + flowBoxH/2 }

// drawFlow lays a flowchart out by rank. Every node sits one layer past its
// deepest predecessor (the classic longest-path ranking), nodes inside a rank
// are ordered by the barycentre of the predecessors above them to keep edges
// from crossing more than they must, and each edge is drawn as a straight
// elbow: down (or right) out of the source, one turn, and a head on the
// target's border.
func drawFlow(blk *mermaidBlock, w int, gl mmGlyphs) ([]string, [][]byte) {
	n := len(blk.graph.nodes)
	// Node labels get a share of the width: a fan-out of k nodes on one rank
	// must fit k boxes side by side, so the per-node budget shrinks with the
	// widest rank. Truncation happens after a floor, never before.
	for _, budget := range []int{0, w/(n+1) - 3, w/(n*2+3) - 3} {
		if budget < 0 {
			budget = 0
		}
		rows, roles, ok := drawFlowAt(blk, w, gl, budget)
		if ok {
			return rows, roles
		}
	}
	// A graph that cannot be laid out inside the terminal (too many nodes for
	// its width) falls back to an indented outline: the topology as text,
	// which is the one drawing of a graph that never overflows a column.
	return drawFlowOutline(blk, w)
}

// drawFlowAt draws the graph with each node label capped at budget. It
// reports ok=false when the drawing still does not fit w or the row budget,
// so the caller can try a smaller label and finally the outline.
func drawFlowAt(blk *mermaidBlock, w int, gl mmGlyphs, budget int) ([]string, [][]byte, bool) {
	n := len(blk.graph.nodes)
	boxes := make([]flowBox, n)
	labels := make([]string, n)
	for i, nd := range blk.graph.nodes {
		lab := nd.label
		if lab == "" {
			lab = nd.id
		}
		if budget > 0 && width(lab) > budget {
			lab = truncateCells(lab, budget, "…")
		}
		labels[i] = lab
		boxes[i] = flowBox{w: width(lab) + 2}
	}
	if len(blk.graph.edges) == 0 && n == 1 {
		// A single node with no edges still draws its own box.
	}
	ranks := flowOrder(blk.graph, n)
	groups := map[int][]int{}
	maxRank := 0
	for i, r := range ranks {
		groups[r] = append(groups[r], i)
		maxRank = max(maxRank, r)
	}
	// Total canvas size, then placement: a rank's row (TD) or column (LR)
	// grows as its members are laid out, and the shorter ranks are centred
	// on the widest one.
	if blk.dir == "LR" {
		x := 0
		for r := 0; r <= maxRank; r++ {
			ids := groups[r]
			if len(ids) == 0 {
				continue
			}
			rankW := 0
			for _, i := range ids {
				rankW += boxes[i].w + flowGapX
			}
			rankW -= flowGapX
			y := 0
			for _, i := range ids {
				boxes[i].x, boxes[i].y = x, y
				y += flowBoxH + flowGapX
			}
			// Centre this rank's column on the tallest one.
			span := (len(ids)-1)*(flowBoxH+flowGapX) + flowBoxH
			widest := (maxRank+1)*flowBoxH + maxRank*flowGapX - flowGapX
			if d := (widest - span) / 2; d > 0 {
				for _, i := range ids {
					boxes[i].y += d
				}
			}
			x += rankW + flowGapLR
		}
	} else {
		total := 0
		for r := 0; r <= maxRank; r++ {
			ids := groups[r]
			if len(ids) == 0 {
				continue
			}
			rankW := 0
			for _, i := range ids {
				rankW += boxes[i].w + flowGapX
			}
			rankW -= flowGapX
			for _, i := range ids {
				boxes[i].y = r * (flowBoxH + flowGapY)
			}
			total = max(total, rankW)
			_ = ids
		}
		for r := 0; r <= maxRank; r++ {
			ids := groups[r]
			rankW := 0
			for _, i := range ids {
				rankW += boxes[i].w + flowGapX
			}
			rankW -= flowGapX
			x := (total - rankW) / 2
			for _, i := range ids {
				boxes[i].x = x
				x += boxes[i].w + flowGapX
			}
		}
	}
	canvasW := 0
	for _, b := range boxes {
		canvasW = max(canvasW, b.x+b.w)
	}
	canvasH := 0
	for _, b := range boxes {
		canvasH = max(canvasH, b.y+flowBoxH)
	}
	if canvasW > w || canvasH > mermaidMaxRows-1 {
		return nil, nil, false
	}

	g := newGrid()
	for i, b := range boxes {
		g.paint(b.x, b.y, gl.tl, mmWire)
		g.paint(b.x+b.w-1, b.y, gl.tr, mmWire)
		g.hspan(b.x+1, b.y, b.w-2, gl.bh, mmWire)
		g.paint(b.x, b.y+1, gl.bv, mmWire)
		g.paint(b.x+b.w-1, b.y+1, gl.bv, mmWire)
		g.text(b.x+1+(b.w-2-width(labels[i]))/2, b.y+1, labels[i], mmHead)
		g.paint(b.x, b.y+2, gl.bl, mmWire)
		g.paint(b.x+b.w-1, b.y+2, gl.br, mmWire)
		g.hspan(b.x+1, b.y+2, b.w-2, gl.bh, mmWire)
	}
	for _, e := range blk.graph.edges {
		if e.from == e.to {
			continue // a self-loop has no honest terminal drawing
		}
		drawFlowEdge(g, boxes[e.from], boxes[e.to], e, blk.dir, gl)
	}
	rows, roles := g.rows()
	return rows, roles, true
}

// drawFlowEdge draws one edge as an elbow: straight out of the source's
// facing side, one turn on the free row (or column) between the two boxes, and
// a head on the target's border. A label rides the turn — above the horizontal
// run in TD, beside the vertical run in LR.
func drawFlowEdge(g *grid, from, to flowBox, e mmEdge, dir string, gl mmGlyphs) {
	wire, head := gl.vline, gl.head[dirDown]
	if e.dotted {
		wire, head = gl.dottedV, gl.headDot[dirDown]
	}
	if dir == "LR" {
		wire, head = gl.hline, gl.head[dirRight]
		if e.dotted {
			wire, head = gl.dotted, gl.headDot[dirRight]
		}
	}
	if dir == "LR" {
		fx, tx := from.x+from.w-1, to.x
		turn := (fx + tx) / 2
		if turn <= fx {
			turn = fx + 1
		}
		g.hspan(fx, from.midY(), max(turn-fx, 0), wire, mmWire)
		g.vspan(turn, min(from.midY(), to.midY()), abs(to.midY()-from.midY())+1, wire, mmWire)
		g.hspan(turn, to.midY(), max(tx-turn, 0), wire, mmWire)
		g.paint(tx-1, to.midY(), head, mmArrow)
		if e.label != "" {
			lx, ly := turn+2, min(from.midY(), to.midY())
			if lx+width(e.label) > w4(tx) {
				lx = max(0, tx-1-width(e.label))
			}
			g.text(lx, ly, e.label, mmText)
		}
		return
	}
	// TD: the source box ends at fy and the next box starts at ty, leaving
	// four free rows between them (flowGapY = 4). One carries the label, one
	// carries the horizontal run, and the last two carry the drop into the
	// target. Giving the label its own row is what stops it from landing on
	// the source's bottom border and being painted over by it.
	fy, ty := from.y+flowBoxH-1, to.y
	labelY, turn := fy+1, fy+2
	side, back := from.midX(), to.midX()
	if back != side {
		lo, hi := min(side, back), max(side, back)
		g.vspan(side, fy, 1, wire, mmWire)
		g.hspan(lo, turn, hi-lo, gl.hline, mmWire)
		if e.dotted {
			g.hspan(lo, turn, hi-lo, gl.dotted, mmWire)
		}
	}
	if e.label != "" {
		g.text(side+2, labelY, e.label, mmText)
	}
	g.vspan(back, turn, max(ty-turn, 0), wire, mmWire)
	g.paint(back, ty-1, head, mmArrow)
}

// w4 is a tiny helper kept for the LR label clamp: the label must not run
// past the target box, so it is capped at the target's left edge.
func w4(tx int) int { return tx }

// drawFlowOutline is the last-resort drawing: one row per node, its label,
// and the edges out of it, indented under it. It is a topology listing, not a
// layout, and it is used only when a layout cannot fit the terminal at all.
func drawFlowOutline(blk *mermaidBlock, w int) ([]string, [][]byte) {
	g := newGrid()
	y := 0
	out := make([][]int, len(blk.graph.nodes))
	for i, e := range blk.graph.edges {
		if e.from < len(out) {
			out[e.from] = append(out[e.from], i)
		}
	}
	arrow := "─▶"
	for i, nd := range blk.graph.nodes {
		if y >= mermaidMaxRows-1 {
			break
		}
		label := nd.label
		if label == "" {
			label = nd.id
		}
		y = g.text(0, y, "["+label+"]", mmHead)
		for _, ei := range out[i] {
			e := blk.graph.edges[ei]
			if e.from == e.to {
				continue
			}
			tgt := blk.graph.nodes[e.to]
			name := tgt.label
			if name == "" {
				name = tgt.id
			}
			text := "  " + arrow + " " + name
			if e.label != "" {
				text += " — " + e.label
			}
			for _, seg := range wrap(text, w) {
				if y >= mermaidMaxRows-1 {
					break
				}
				y = g.text(0, y, seg, mmText)
			}
		}
	}
	return g.rows()
}

// flowOrder returns each node's rank (longest distance from a source) and, in
// place, the declaration order of the nodes inside a rank after a barycentre
// sweep: each node moves to the average position of the predecessors above it,
// which is what keeps a fan-out from crossing its own edges.
func flowOrder(g mmGraph, n int) []int {
	rank := make([]int, n)
	indeg := make([]int, n)
	out := make([][]int, n)
	for _, e := range g.edges {
		if e.from == e.to || e.from >= n || e.to >= n {
			continue
		}
		out[e.from] = append(out[e.from], e.to)
		indeg[e.to]++
	}
	queue := make([]int, 0, n)
	for i := range rank {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	seen := 0
	for h := 0; h < len(queue); h++ {
		u := queue[h]
		seen++
		for _, v := range out[u] {
			rank[v] = max(rank[v], rank[u]+1)
			if indeg[v]--; indeg[v] == 0 {
				queue = append(queue, v)
			}
		}
	}
	if seen < n {
		// A cycle: every node in it keeps a rank of at least 1 so the layout
		// terminates with the cycle drawn downward.
		for i := range rank {
			if indeg[i] > 0 {
				rank[i] = max(rank[i], 1)
			}
		}
	}
	return rank
}

// --- small string helpers ------------------------------------------------

// afterSpace returns s with its first token (and the space after it) removed.
func afterSpace(s string) string {
	_, rest := cutWord(s)
	return rest
}

// cutWord splits off s's first whitespace-delimited word.
func cutWord(s string) (word, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i+1:], " \t")
}

// cutColon splits a message's label off its `: text` tail. A label with no
// colon (a bare arrow with no text) is empty, not the whole line.
func cutColon(s string) (label, rest string) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
}

func hasPrefixFold(s, p string) bool { return strings.HasPrefix(strings.ToLower(s), p) }

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(strings.ToLower(s), strings.ToLower(p)) {
			return true
		}
	}
	return false
}
