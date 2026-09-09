package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// cell is one styled run within a rendered line: a substring of the source
// text plus the style it should be painted with.
type cell struct {
	text  string
	style tcell.Style
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
// fenced code lines carry the code-bg band.
func (a *App) renderMarkdown(src string, w int) []line {
	src = strings.TrimRight(src, "\n")
	if src == "" {
		return []line{textline("", a.mdStyle().body)}
	}
	ms := a.mdStyle()

	inCode := false
	quoteDepth := 0
	var out []line
	for _, raw := range strings.Split(src, "\n") {
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
			out = append(out, textline(strings.Repeat("─", min(3, w)), ms.muted))
			continue
		}

		// Headings: strip #s, color by level.
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
			out = append(out, textline(rest, st))
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

		// Plain body line.
		ln := line{}
		ln.runs = appendRuns(ln.runs, a.inlineRuns(raw, ms), w)
		out = append(out, ln)
	}
	return out
}

// inlineRuns styles inline markdown: **bold** *italic* `code` [text](url)
// (url hidden, text underlined). Nested markers are not re-parsed; content
// inside markers renders with the marker style.
func (a *App) inlineRuns(s string, ms mdStyle) []cell {
	var runs []cell
	var buf strings.Builder
	flush := func(st tcell.Style) {
		if buf.Len() > 0 {
			runs = append(runs, cell{text: buf.String(), style: st})
			buf.Reset()
		}
	}
	rest := s
	for rest != "" {
		// `code`
		if after, ok := cutMarker(rest, "`"); ok {
			content, remainder, ok2 := cutClosing(after, "`")
			if ok2 {
				buf.WriteString(beforeOf(rest, "`"))
				flush(ms.body)
				runs = append(runs, cell{text: content, style: ms.inlineCode})
				rest = remainder
				continue
			}
		}
		// **bold**
		if after, ok := cutMarker(rest, "**"); ok {
			content, remainder, ok2 := cutClosing(after, "**")
			if ok2 {
				buf.WriteString(beforeOf(rest, "**"))
				flush(ms.body)
				runs = append(runs, cell{text: content, style: ms.bold})
				rest = remainder
				continue
			}
		}
		// [text](url) — url hidden, text underlined.
		if after, ok := cutMarker(rest, "["); ok {
			if text, mid, ok2 := cutClosing(after, "]"); ok2 && strings.HasPrefix(mid, "(") {
				if _, remainder, ok3 := cutClosing(mid[1:], ")"); ok3 {
					buf.WriteString(beforeOf(rest, "["))
					flush(ms.body)
					runs = append(runs, cell{text: text, style: ms.link})
					rest = remainder
					continue
				}
			}
		}
		if after, ok := cutMarker(rest, "*"); ok && !strings.HasPrefix(after, "*") {
			content, remainder, ok2 := cutClosing(after, "*")
			if ok2 {
				buf.WriteString(beforeOf(rest, "*"))
				flush(ms.body)
				runs = append(runs, cell{text: content, style: ms.italic})
				rest = remainder
				continue
			}
		}
		// No marker: consume one rune.
		r := []rune(rest)[0]
		buf.WriteRune(r)
		rest = string([]rune(rest)[1:])
	}
	flush(ms.body)
	return runs
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

// beforeOf returns the part of s before the first occurrence of marker
// (used with cutMarker results to preserve unmatched text).
func beforeOf(s, marker string) string {
	if i := strings.Index(s, marker); i >= 0 {
		return s[:i]
	}
	return ""
}

// cutClosing splits s at the next occurrence of closer → (content, after-closer, true).
func cutClosing(s, closer string) (content, after string, ok bool) {
	i := strings.Index(s, closer)
	if i < 0 {
		return "", s, false
	}
	return s[:i], s[i+len(closer):], true
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
				out = append(out, line{runs: []cell{{text: string(runes[:cut]), style: r.style}}, bg: ln.bg})
				wd = string(runes[cut:])
				ww = width(wd)
			}
			if ww > 0 {
				cur = append(cur, cell{text: wd, style: r.style})
				curW += ww
			}
		}
	}
	if len(cur) > 0 {
		out = append(out, line{runs: cur, bg: ln.bg})
	}
	return out
}
