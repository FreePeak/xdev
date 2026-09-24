package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// cell is one styled run within a rendered line: a substring of the source
// text plus the style it should be painted with. chrome marks a run a mouse
// selection leaves out: a box's border, the pad cell inside it, the fill that
// aligns its right border. Chrome is painted, never copied — only the code that
// draws a run knows whether that run is the frame, which is why a rule row's
// label is a run of its own and never a frame glyph.
type cell struct {
	text   string
	style  tcell.Style
	chrome bool
	link   string // visible HTTP(S) target; empty for ordinary text
}

// line is a rendered visual line: styled runs plus a full-row background
// (zero = transparent/terminal bg).
type line struct {
	runs []cell
	bg   tcell.Color
}

// textline builds a single-run line.
func textline(text string, style tcell.Style) line {
	return line{runs: []cell{{text: text, style: style}}}
}

// --- markdown rendering (grok-build xai-grok-markdown semantics) ---

// mdStyle resolves the theme slots a piece of markdown needs.
type mdStyle struct {
	h1, h2, h3   tcell.Style // bold headings (teal/blue/purple)
	h4, h5       tcell.Style // gray bold headings
	h6           tcell.Style // gray non-bold heading
	body         tcell.Style // md_text
	bold, italic tcell.Style
	inlineCode   tcell.Style // bold, fg md_code
	muted        tcell.Style // bullets, quote bars, rules
	link         tcell.Style // underline, fg link_fg
	codeBg       tcell.Color
}

func (a *App) mdStyle() mdStyle {
	return mdStyle{
		h1:         tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.MdHeading1))).Bold(true),
		h2:         tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.MdHeading2))).Bold(true),
		h3:         tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.MdHeading3))).Bold(true),
		h4:         tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayBright))).Bold(true),
		h5:         tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray))).Bold(true),
		h6:         tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim))),
		body:       tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary))),
		bold:       tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary))).Bold(true),
		italic:     tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.TextSecondary))).Italic(true),
		inlineCode: tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.MdCode))).Bold(true),
		muted:      tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.MdMuted))),
		link:       tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.LinkFg))).Underline(true),
		codeBg:     a.cellColor(a.th.Get(theme.MdCodeBg)),
	}
}

// renderMarkdown converts markdown source to visual lines of styled runs,
// following the grok markdown renderer: fences/backticks/URLs hidden,
// bullets → •, blockquote > → │, hr → ───, headings colored+bold,
// fenced code lines carry the code-bg band, GFM pipe tables → sharp grid.
func (a *App) renderMarkdown(src string, w int) []line {
	src = strings.TrimRight(src, "\n")
	if src == "" {
		return []line{textline("", a.mdStyle().body)}
	}
	ms := a.mdStyle()

	inCode := false
	quoteDepth := 0
	srcLines := strings.Split(src, "\n")
	var out []line
	for i := 0; i < len(srcLines); i++ {
		raw := srcLines[i]
		trimmed := strings.TrimSpace(raw)

		// Fenced code blocks: hide the fence, band every content line.
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			out = append(out, line{runs: []cell{{text: raw, style: ms.body}}, bg: ms.codeBg})
			continue
		}

		// Thematic break: --- / *** / ___ → ─── muted.
		if isHR(trimmed) {
			out = append(out, textline(strings.Repeat(a.th.Box().Horizontal, min(3, w)), ms.muted))
			continue
		}

		// Headings: strip #s, color by level, and recognize links while
		// preserving the historical literal text for ordinary markers.
		if lvl, rest, ok := splitHeading(trimmed); ok {
			var st tcell.Style
			switch {
			case lvl == 1:
				st = ms.h1
			case lvl == 2:
				st = ms.h2
			case lvl == 3:
				st = ms.h3
			case lvl == 4:
				st = ms.h4
			case lvl == 5:
				st = ms.h5
			default:
				st = ms.h6
			}
			runs := a.inlineHeadingRuns(rest, ms)
			for i := range runs {
				if runs[i].link == "" {
					runs[i].style = st
				}
			}
			out = append(out, line{runs: runs})
			continue
		}

		// Blockquote: count leading > then strip; bar per depth.
		if strings.HasPrefix(trimmed, ">") {
			quoteDepth = strings.Count(trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, ">"))], ">")
			rest := strings.TrimLeft(trimmed, ">")
			rest = strings.TrimPrefix(rest, " ")
			ln := line{}
			ln.runs = append(ln.runs, cell{text: strings.Repeat("│ ", quoteDepth), style: ms.muted})
			ln.runs = appendRuns(ln.runs, a.inlineRuns(rest, ms), w)
			out = append(out, ln)
			continue
		}

		// Lists: - / * / + → • (muted marker); ordered keeps digits.
		if marker, rest, ok := splitListMarker(trimmed); ok {
			ln := line{}
			ln.runs = append(ln.runs, cell{text: marker + " ", style: ms.muted})
			ln.runs = appendRuns(ln.runs, a.inlineRuns(rest, ms), w)
			out = append(out, ln)
			continue
		}

		// GFM table: a pipe row followed by a |---|---| delimiter opens a
		// grid; the block swallows every following pipe row. Too-narrow
		// grids fall back to the raw lines (omp does the same).
		if strings.Contains(trimmed, "|") {
			if tbl, n := a.tableAt(srcLines, i, w, ms); n > 0 {
				out = append(out, tbl...)
				i += n - 1
				continue
			}
		}

		// Plain body line.
		ln := line{}
		ln.runs = appendRuns(ln.runs, a.inlineRuns(raw, ms), w)
		out = append(out, ln)
	}
	return out
}

// inlineRuns styles inline markdown: **bold** *italic* `code` [text](url).
// Markdown hides a link's target, but the target is retained on the rendered
// run for the TUI's mouse hit table; bare HTTP(S) URLs are styled as links too.
func (a *App) inlineRuns(s string, ms mdStyle) []cell {
	var runs []cell
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			runs = appendWebURLRuns(runs, buf.String(), ms.body, ms.link)
			buf.Reset()
		}
	}
	for len(s) > 0 {
		i := strings.IndexAny(s, "`*[")
		if i < 0 {
			buf.WriteString(s)
			break
		}
		buf.WriteString(s[:i])
		s = s[i:]
		var (
			content, rest, target string
			st                    tcell.Style
			closed, formatted     bool
		)
		switch {
		case strings.HasPrefix(s, "`"):
			after, _ := cutMarker(s, "`")
			content, rest, closed = cutClosing(after, "`")
			st = ms.inlineCode
		case strings.HasPrefix(s, "**"):
			after, _ := cutMarker(s, "**")
			content, rest, closed = cutClosing(after, "**")
			st, formatted = ms.bold, true
		case strings.HasPrefix(s, "["):
			after, _ := cutMarker(s, "[")
			if text, mid, ok := cutClosing(after, "]"); ok && strings.HasPrefix(mid, "(") {
				var tail string
				var ok bool
				target, tail, ok = cutLinkDestination(mid[1:])
				if ok {
					content, rest, closed, st = text, tail, true, ms.link
					if !isWebURL(target) {
						target = "" // rendered, but never clickable
					}
				}
			}
		case strings.HasPrefix(s, "*"):
			after, _ := cutMarker(s, "*")
			if !strings.HasPrefix(after, "*") {
				content, rest, closed = cutClosing(after, "*")
				st, formatted = ms.italic, true
			}
		}
		if !closed {
			buf.WriteByte(s[0])
			s = s[1:]
			continue
		}
		flush()
		if formatted {
			nested := a.inlineRuns(content, ms)
			if hasLink(nested) {
				for i := range nested {
					nested[i].style = withEmphasis(nested[i].style, st)
				}
				runs = append(runs, nested...)
				s = rest
				continue
			}
		}
		runs = append(runs, cell{text: content, style: st, link: target})
		s = rest
	}
	flush()
	return runs
}

// inlineHeadingRuns recognizes explicit links and bare URLs while leaving
// every other heading character literal, including backticks and asterisks.
func (a *App) inlineHeadingRuns(s string, ms mdStyle) []cell {
	var runs []cell
	for len(s) > 0 {
		if i := strings.Index(s, "["); i >= 0 {
			after, _ := cutMarker(s[i:], "[")
			if text, mid, ok := cutClosing(after, "]"); ok && strings.HasPrefix(mid, "(") {
				if target, tail, ok := cutLinkDestination(mid[1:]); ok && isWebURL(target) {
					runs = append(runs, cell{text: s[:i], style: ms.body}, cell{text: text, style: ms.link, link: target})
					s = tail
					continue
				}
			}
		}
		start := webURLStart(s)
		if start < 0 {
			return append(runs, cell{text: s, style: ms.body})
		}
		end := webURLEnd(s[start:])
		for end > 0 && strings.ContainsRune("`*", rune(s[start+end-1])) {
			end--
		}
		if end <= 0 || !isWebURL(s[start:start+end]) {
			return append(runs, cell{text: s, style: ms.body})
		}
		target := s[start : start+end]
		runs = append(runs, cell{text: s[:start], style: ms.body}, cell{text: target, style: ms.link, link: target})
		s = s[start+end:]
	}
	return runs
}

func hasLink(runs []cell) bool {
	for _, run := range runs {
		if run.link != "" {
			return true
		}
	}
	return false
}

// appendWebURLRuns splits plain prose into ordinary and HTTP(S) URL runs. Code
// fences and inline-code runs do not use this helper, so URLs in code remain
// ordinary text.
func appendWebURLRuns(dst []cell, text string, style, linkStyle tcell.Style) []cell {
	for len(text) > 0 {
		start := webURLStart(text)
		if start < 0 {
			return append(dst, cell{text: text, style: style})
		}
		if start > 0 {
			dst = append(dst, cell{text: text[:start], style: style})
		}
		end := webURLEnd(text[start:])
		if end <= 0 {
			return append(dst, cell{text: text[start:], style: style})
		}
		target := text[start : start+end]
		if isWebURL(target) {
			dst = append(dst, cell{text: target, style: linkStyle, link: target})
		} else {
			dst = append(dst, cell{text: target, style: style})
		}
		text = text[start+end:]
	}
	return dst
}

// withEmphasis adds an outer bold/italic mark without replacing an inner
// style's existing attributes.
func withEmphasis(style, outer tcell.Style) tcell.Style {
	_, _, attrs := outer.Decompose()
	if attrs&tcell.AttrBold != 0 {
		style = style.Bold(true)
	}
	if attrs&tcell.AttrItalic != 0 {
		style = style.Italic(true)
	}
	return style
}

// appendRuns appends wrapped runs, breaking at width w (greedy word wrap).
func appendRuns(dst []cell, runs []cell, w int) []cell {
	// Simple approach: concatenate as text; wrapping happens per-line in draw.
	return append(dst, runs...)
}

// isHR reports whether s is a thematic break (3+ of -, *, _).
func isHR(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c && s[i] != ' ' {
			return false
		}
	}
	return true
}

// splitHeading parses "# ... " → (level, rest, true).
func splitHeading(s string) (lvl int, rest string, ok bool) {
	if !strings.HasPrefix(s, "#") {
		return 0, "", false
	}
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n > len(s) || (n < len(s) && s[n] != ' ' && s[n] != '\t') {
		if n == len(s) {
			return n, "", true
		}
		return 0, "", false
	}
	return n, strings.TrimSpace(s[n:]), true
}

// splitListMarker parses "- x" / "* x" / "+ x" / "1. x" → (marker, rest, true).
func splitListMarker(s string) (marker, rest string, ok bool) {
	if s == "" {
		return "", "", false
	}
	switch s[0] {
	case '-', '*', '+':
		if len(s) == 1 || s[1] == ' ' {
			return "•", strings.TrimPrefix(s[1:], " "), true
		}
	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i < len(s) && (s[i] == '.' || s[i] == ')') && i+1 < len(s) && s[i+1] == ' ' {
			return s[:i+1], strings.TrimPrefix(s[i+1:], " "), true
		}
	}
	return "", "", false
}

// cutMarker returns s minus a leading marker, if present.
func cutMarker(s, marker string) (after string, ok bool) {
	if strings.HasPrefix(s, marker) {
		return s[len(marker):], true
	}
	return s, false
}

// cutClosing splits s at the next occurrence of closer → (content, after-closer, true).
func cutClosing(s, closer string) (content, after string, ok bool) {
	i := strings.Index(s, closer)
	if i < 0 {
		return "", s, false
	}
	return s[:i], s[i+len(closer):], true
}

// cutLinkDestination parses a CommonMark inline link destination, including
// balanced parentheses, backslash escapes, and an optional quoted title.
func cutLinkDestination(s string) (target, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t\n")
	if s == "" {
		return "", "", false
	}
	if s[0] == '<' {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				if i+1 >= len(s) || !isASCIIPunct(s[i+1]) {
					return "", "", false
				}
				b.WriteByte(s[i+1])
				i++
			case '\n':
				return "", "", false
			case '>':
				return finishLinkDestination(b.String(), strings.TrimLeft(s[i+1:], " \t\n"))
			default:
				b.WriteByte(s[i])
			}
		}
		return "", "", false
	}
	depth := 0
	var b strings.Builder
	i := 0
loop:
	for ; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) || !isASCIIPunct(s[i+1]) {
				return "", "", false
			}
			b.WriteByte(s[i+1])
			i++
		case '(':
			depth++
			b.WriteByte(s[i])
		case ')':
			if depth == 0 {
				break loop
			}
			depth--
			b.WriteByte(s[i])
		case ' ', '\t', '\n':
			break loop
		default:
			b.WriteByte(s[i])
		}
	}
	if depth != 0 {
		return "", "", false
	}
	return finishLinkDestination(b.String(), s[i:])
}

func finishLinkDestination(target, rest string) (string, string, bool) {
	rest = strings.TrimLeft(rest, " \t\n")
	if rest == "" {
		return "", "", false
	}
	if rest[0] == ')' {
		return target, rest[1:], true
	}
	if rest[0] != '"' && rest[0] != '\'' {
		return "", "", false
	}
	quote := rest[0]
	for i := 1; i < len(rest); i++ {
		if rest[i] == '\\' {
			i++
			continue
		}
		if rest[i] == quote {
			i++
			if i >= len(rest) || rest[i] != ')' {
				return "", "", false
			}
			return target, rest[i+1:], true
		}
	}
	return "", "", false
}

func isASCIIPunct(c byte) bool {
	return strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", rune(c))
}

// wrapLine wraps one rendered line to width w, returning extra visual lines.
// Continuation lines repeat their indent (2 spaces per quote level; the
// bullet indent for lists is approximated by continuing flush).
func wrapLine(ln line, w int) []line {
	if w <= 0 {
		return []line{ln}
	}
	// Measure total width; if it fits, done.
	tw := 0
	for _, r := range ln.runs {
		tw += width(r.text)
	}
	if tw <= w {
		return []line{ln}
	}
	var out []line
	var cur []cell
	curW := 0
	for _, r := range ln.runs {
		words := strings.SplitAfter(r.text, " ")
		for _, wd := range words {
			ww := width(wd)
			if curW > 0 && curW+ww > w && wd != " " {
				out = append(out, line{runs: cur, bg: ln.bg})
				cur = nil
				curW = 0
				wd = strings.TrimLeft(wd, " ")
				ww = width(wd)
				if wd == "" {
					continue
				}
			}
			// Hard-break words longer than the line.
			for ww > w {
				runes := []rune(wd)
				cut := 0
				cw := 0
				for cut < len(runes) && cw+width(string(runes[cut])) <= w {
					cw += width(string(runes[cut]))
					cut++
				}
				if cut == 0 {
					cut = 1
				}
				out = append(out, line{runs: []cell{{text: string(runes[:cut]), style: r.style, chrome: r.chrome, link: r.link}}, bg: ln.bg})
				wd = string(runes[cut:])
				ww = width(wd)
			}
			if ww > 0 {
				cur = append(cur, cell{text: wd, style: r.style, chrome: r.chrome, link: r.link})
				curW += ww
			}
		}
	}
	if len(cur) > 0 {
		out = append(out, line{runs: cur, bg: ln.bg})
	}
	return out
}

// --- GFM tables (omp semantics) ---
//
// A table renders as a full sharp-cornered grid with one rule between every
// pair of rows: top border, bold header, separators, bottom border. Column
// widths come from omp's fitter: a column wants its widest cell, the
// wrappable minimum is the longest word capped at 30, and the 3-per-column
// border overhead is subtracted from the terminal width first. Alignment
// colons are accepted and ignored (everything renders flush-left).

// tableFrame is the grid's border glyph set. Tables always draw sharp,
// whatever the chrome box style is; the ascii preset degenerates every
// junction to +.
type tableFrame struct {
	topLeft, teeDown, topRight     string
	teeLeft, cross, teeRight       string
	bottomLeft, teeUp, bottomRight string
	horizontal, vertical           string
}

func (a *App) tableFrame() tableFrame {
	b := a.th.BoxSharp()
	f := tableFrame{
		topLeft: b.TopLeft, teeDown: "┬", topRight: b.TopRight,
		teeLeft: "├", cross: "┼", teeRight: "┤",
		bottomLeft: b.BottomLeft, teeUp: "┴", bottomRight: b.BottomRight,
		horizontal: b.Horizontal, vertical: b.Vertical,
	}
	if f.horizontal == "-" {
		f.teeDown, f.teeUp, f.teeLeft, f.teeRight, f.cross = "+", "+", "+", "+", "+"
	}
	return f
}

// rule builds one border row (top / separator / bottom) over columns of the
// given content widths.
func (f tableFrame) rule(left, junction, right string, m []int) string {
	s := left + f.horizontal
	for i, cw := range m {
		if i > 0 {
			s += f.horizontal + junction + f.horizontal
		}
		s += strings.Repeat(f.horizontal, cw)
	}
	return s + f.horizontal + right
}

// tableRow splits one pipe-table line into cells. ok=false when the line
// carries no unescaped pipe (so it cannot belong to a table); `\|` is a
// literal pipe and the optional outer pipes are dropped.
func tableRow(s string) (cells []string, ok bool) {
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == '|':
			cur.WriteByte('|')
			i++
		case s[i] == '|':
			ok = true
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	cells = append(cells, strings.TrimSpace(cur.String()))
	if len(cells) > 1 && cells[0] == "" {
		cells = cells[1:]
	}
	if len(cells) > 1 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	return cells, ok
}

// isTableDelim reports whether s is the | --- | :-- | ---: | delimiter row of
// an n-column table.
func isTableDelim(s string, n int) bool {
	cells, pipe := tableRow(s)
	if !pipe || len(cells) != n {
		return false
	}
	for _, c := range cells {
		c = strings.Trim(c, ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

func runsWidth(runs []cell) int {
	n := 0
	for _, r := range runs {
		n += width(r.text)
	}
	return n
}

func runsString(runs []cell) string {
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.text)
	}
	return b.String()
}

// runsLongestWord measures the widest unbreakable stretch of a rendered cell
// (the floor for its column width).
func runsLongestWord(runs []cell) int {
	best := 0
	for _, wd := range strings.Fields(runsString(runs)) {
		if cw := width(wd); cw > best {
			best = cw
		}
	}
	return best
}

// tableAt renders the GFM table opening at lines[i]: header, delimiter, and
// the run of following pipe rows. It returns the grid lines and the number of
// source lines consumed; n=0 means the block is not a table or cannot fit w,
// and the caller must render the raw lines instead.
func (a *App) tableAt(lines []string, i, w int, ms mdStyle) (out []line, n int) {
	head, pipe := tableRow(strings.TrimSpace(lines[i]))
	r := len(head)
	if !pipe || r == 0 || i+1 >= len(lines) || !isTableDelim(strings.TrimSpace(lines[i+1]), r) {
		return nil, 0
	}
	over := 3*r + 1
	if w-over < r {
		return nil, 0 // too narrow for even a 1-cell grid: keep the raw text
	}

	grid := make([][]string, 0, 8)
	grid = append(grid, head)
	j := i + 2
	for ; j < len(lines); j++ {
		row, ok := tableRow(strings.TrimSpace(lines[j]))
		if !ok {
			break
		}
		for len(row) < r {
			row = append(row, "")
		}
		grid = append(grid, row[:r])
	}

	// Style every cell (inline markdown included); the header row is bold.
	runs := make([][][]cell, len(grid))
	u := make([]int, r) // widest rendered cell per column
	p := make([]int, r) // longest word per column, capped at 30, floored at 1
	const maxPref = 30
	for gi, row := range grid {
		runs[gi] = make([][]cell, r)
		for c, text := range row {
			cr := a.inlineRuns(text, ms)
			if gi == 0 {
				for k := range cr {
					cr[k].style = cr[k].style.Bold(true)
				}
			}
			runs[gi][c] = cr
			if wd := runsWidth(cr); wd > u[c] {
				u[c] = wd
			}
		}
	}
	for c := range r {
		lw := 1
		for gi := range runs {
			if wd := runsLongestWord(runs[gi][c]); wd > lw {
				lw = wd
			}
		}
		p[c] = min(max(lw, 1), maxPref)
	}

	// Fit the columns into w-over cells. c starts at the preferred widths and
	// shrinks proportionally when even those overflow; m is the final width.
	c := p
	sumC := 0
	for _, v := range c {
		sumC += v
	}
	if avail := w - over; sumC > avail {
		extra := avail - r
		sumQ := 0
		for _, v := range c {
			sumQ += max(0, v-1)
		}
		c = make([]int, r)
		used := 0
		for col, v := range p {
			q := 0
			if extra > 0 && sumQ > 0 {
				q = max(0, v-1) * extra / sumQ
			}
			c[col] = 1 + q
			used += q
		}
		// Hand out the rounding remainder one cell per column (omp).
		rem := extra - used
		for col := range c {
			if rem <= 0 {
				break
			}
			c[col]++
			rem--
		}
		sumC = r + extra
	}
	m := make([]int, r)
	sumU := 0
	for _, v := range u {
		sumU += v
	}
	if sumU+over <= w {
		for col := range m {
			m[col] = max(u[col], c[col])
		}
	} else {
		avail := w - over
		excess := 0
		for col := range c {
			excess += max(0, u[col]-c[col])
		}
		spare := avail - sumC
		used := 0
		for col := range m {
			f := 0
			if excess > 0 && spare > 0 {
				f = max(0, u[col]-c[col]) * spare / excess
			}
			m[col] = c[col] + f
			used += f
		}
		for left := avail - sumC - used; left > 0; {
			moved := false
			for col := range m {
				if left == 0 {
					break
				}
				if m[col] < u[col] {
					m[col]++
					left--
					moved = true
				}
			}
			if !moved {
				break
			}
		}
	}

	f := a.tableFrame()
	out = append(out, textline(f.rule(f.topLeft, f.teeDown, f.topRight, m), ms.muted))
	sep := func() { out = append(out, textline(f.rule(f.teeLeft, f.cross, f.teeRight, m), ms.muted)) }
	for gi := range runs {
		if gi > 0 {
			sep()
		}
		cellLines := make([][]line, r)
		k := 1
		for col := range m {
			cl := wrapLine(line{runs: runs[gi][col]}, m[col])
			cellLines[col] = cl
			if len(cl) > k {
				k = len(cl)
			}
		}
		for v := range k {
			ln := line{}
			ln.runs = append(ln.runs, cell{text: f.vertical + " ", style: ms.muted})
			for col := range m {
				var rc []cell
				if v < len(cellLines[col]) {
					rc = cellLines[col][v].runs
				}
				ln.runs = append(ln.runs, rc...)
				if pad := m[col] - runsWidth(rc); pad > 0 {
					ln.runs = append(ln.runs, cell{text: strings.Repeat(" ", pad)})
				}
				if col < r-1 {
					ln.runs = append(ln.runs, cell{text: " " + f.vertical + " ", style: ms.muted})
				}
			}
			ln.runs = append(ln.runs, cell{text: " " + f.vertical, style: ms.muted})
			out = append(out, ln)
		}
	}
	out = append(out, textline(f.rule(f.bottomLeft, f.teeUp, f.bottomRight, m), ms.muted))
	return out, j - i
}
