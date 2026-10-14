package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// wrapLine was rewritten from a strings.SplitAfter walk (one substring and one
// slice entry per word) to a byte-index walk, and its over-long-word break from
// a []rune walk to cutAt. Both are pure reshapes: the same runs must come out,
// in the same order, with the same text and the same styles, for every line a
// renderer can hand it. A drift here is invisible in a screenshot and shows up
// as a word wrapped one cell early, so the old implementation is kept here as
// the reference and compared run for run.
func wrapLineReference(ln line, w int) []line {
	if w <= 0 {
		return []line{ln}
	}
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

func sameLines(a, b []line) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].bg != b[i].bg || a[i].inset != b[i].inset || len(a[i].runs) != len(b[i].runs) {
			return false
		}
		for j := range a[i].runs {
			if a[i].runs[j] != b[i].runs[j] {
				return false
			}
		}
	}
	return true
}

func TestWrapLineMatchesReference(t *testing.T) {
	st := tcell.StyleDefault
	style := func(text string) cell { return cell{text: text, style: st} }
	link := func(text string) cell { return cell{text: text, style: st, link: "https://example.com"} }
	chrome := func(text string) cell { return cell{text: text, style: st, chrome: true} }

	fixtures := []line{
		{runs: []cell{style("")}},
		{runs: []cell{style("short")}},
		{runs: []cell{style("exactly ten")}},
		{runs: []cell{style("one two three four five six seven eight nine ten eleven")}},
		{runs: []cell{style("supercalifragilisticexpialidociousandthensomemoreletterstobreak")}},
		{runs: []cell{style(strings.Repeat("word ", 60))}},
		{runs: []cell{style(strings.Repeat("a", 40) + " " + strings.Repeat("b", 40))}},
		// Wide runes, combining marks, emoji, zero-width: the shapes where a
		// per-rune walk and a table lookup can disagree about a cut.
		{runs: []cell{style(strings.Repeat("漢字", 20))}},
		{runs: []cell{style("é" + strings.Repeat("e\u0301", 30))}},
		{runs: []cell{style(strings.Repeat("👍", 20))}},
		{runs: []cell{style("a\u200bb " + strings.Repeat("x", 30))}},
		{runs: []cell{style("  leading spaces and then a very long tail " + strings.Repeat("tail ", 20))}},
		{runs: []cell{style("trailing spaces   ")}},
		{runs: []cell{style(strings.Repeat(" ", 30))}},
		// Several runs in one line: the wrap must carry each run's own style
		// and link across the break, which is where a byte walk could drop one.
		{runs: []cell{style("plain "), link("https://example.com/a/very/long/path"), style(" tail")}},
		{runs: []cell{chrome("│ "), style(strings.Repeat("body ", 30)), chrome(" │")}},
		{runs: []cell{link(strings.Repeat("x", 50)), style(" after")}},
		{runs: []cell{style("a"), style("b"), style("c"), style(strings.Repeat("long-run ", 20))}},
	}
	for _, w := range []int{1, 2, 3, 5, 8, 10, 20, 40, 114} {
		for i, ln := range fixtures {
			got := wrapLine(ln, w)
			want := wrapLineReference(ln, w)
			if !sameLines(got, want) {
				t.Fatalf("wrapLine(fixture %d, w=%d) drifted from the reference:\n got %s\nwant %s",
					i, w, joinedLines(got), joinedLines(want))
			}
		}
	}
}
