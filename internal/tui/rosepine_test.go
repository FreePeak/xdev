package tui

// The launch dark theme wears Rosé Pine's syntax and diff inks. These tests
// are the executable form of that claim, and each one fails against the palette
// it replaced (One Dark's #f92672 keyword and #e6db74 string, the TokyoNight
// green added marker) — which is the point: a palette migration only ever
// checked by eye drifts back on the next "small tweak" to the diff block.
//
// The expected values are PINNED LITERALS, never read back out of the theme.
// A test that asks the theme what colour it should have painted is a test that
// passes on the old palette too: the first version of this file did exactly
// that and went green against the reverted theme, which is the whole reason the
// constants are here rather than a `theme.Load(...)` lookup.
//
// Provenance: the values are Rosé Pine's main variant (MIT,
// github.com/rose-pine/rose-pine-theme) — base #191724, surface #1f1d2e,
// overlay #26233a, muted #6e6a86, subtle #908caa, text #e0def4, love #eb6f92,
// gold #f6c177, rose #ebbcba, pine #31748f, foam #9ccfd8, iris #c4a7e7.
// Fixtures are invented (a/f.go, a four-line go file) and written from the
// unified-diff and go grammars, not harvested from anything.
//
// SCOPE, deliberately: this file pins the DIFF inks and the SYNTAX inks —
// the tool-output and code surfaces the request named — and nothing else. The
// chrome, the body ink, the accent and the status row are still GrokNight's,
// on purpose: the terminal's own background already supplies the canvas, and
// moving chrome onto the palette is a different change with a different blast
// radius (every chrome test in the suite compares against those greys).

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// Rosé Pine main, by token name. One block so a palette change is one edit
// here and one comment update in theme.go, and so no test can name a colour
// the theme does not carry.
const (
	rpText   = "#e0def4" // text
	rpSubtle = "#908caa" // subtle
	rpMuted  = "#6e6a86" // muted — the comment ink (see theme.go)
	rpLove   = "#eb6f92" // love
	rpGold   = "#f6c177" // gold
	rpRose   = "#ebbcba" // rose
	rpPine   = "#31748f" // pine
	rpFoam   = "#9ccfd8" // foam
)

// TestLaunchDarkThemeIsRosePine pins every readable ink the launch theme owns.
// The chrome (canvas, borders, the neutral greys) stays GrokNight's on purpose
// — the terminal's own background already supplies the canvas, and chrome
// carrying a palette's identity is what made the two themes read as different
// products — so this asserts the inks and says nothing about bg_base.
func TestLaunchDarkThemeIsRosePine(t *testing.T) {
	th := theme.Load("groknight")
	if !th.Dark {
		t.Fatal("groknight must be dark")
	}
	want := map[string]string{
		// Syntax: the 9 fenced-code / tool-output roles, mapped onto Rosé
		// Pine's own groups (vscode themes/rose-pine-color-theme.json).
		theme.SyntaxComment:     rpMuted,
		theme.SyntaxKeyword:     rpPine,
		theme.SyntaxString:      rpGold,
		theme.SyntaxNumber:      rpRose, // constant.numeric, NOT nvim's gold
		theme.SyntaxType:        rpFoam,
		theme.SyntaxVariable:    rpText,
		theme.SyntaxFunction:    rpRose,
		theme.SyntaxOperator:    rpSubtle,
		theme.SyntaxPunctuation: rpSubtle,
		// The diff's three inks: upstream's own git groups.
		theme.ToolDiffAdded:   rpFoam,
		theme.ToolDiffRemoved: rpLove,
		theme.ToolDiffContext: rpSubtle,
	}
	for slot, hex := range want {
		got, ok := th.Slot(slot)
		if !ok {
			t.Errorf("%s is left to the terminal default; the launch theme must pin it", slot)
			continue
		}
		if wantC := theme.Hex(hex); got != wantC {
			t.Errorf("%s = %+v, want Rosé Pine %s", slot, got, hex)
		}
	}
}

// The nine syntax roles collapse onto six colours on purpose — a comment, an
// operator and punctuation are all `subtle` in Rosé Pine — so the contract is
// not "every role distinct" but "the classes a reader tells apart are apart":
// a comment, a string, a number and a keyword must be four different inks, or
// the highlighting says nothing. This is the same claim
// TestFencedGoBlockColorsItsTokens makes about the renderer; this one is about
// the palette underneath it, so it names the palette's own values.
func TestLaunchDarkThemeKeepsTokenClassesApart(t *testing.T) {
	th := theme.Load("groknight")
	distinct := []string{
		theme.SyntaxComment, theme.SyntaxKeyword,
		theme.SyntaxString, theme.SyntaxNumber, theme.SyntaxType,
	}
	seen := map[theme.Color]string{}
	for _, slot := range distinct {
		c, ok := th.Slot(slot)
		if !ok {
			t.Errorf("%s is unset", slot)
			continue
		}
		if prev, dup := seen[c]; dup {
			t.Errorf("%s collapsed onto %s at %+v: two token classes a reader tells apart wear one ink", slot, prev, c)
		}
		seen[c] = slot
	}
	// The diff's polarity pair must be two different inks for the same reason,
	// even though neither is a green or a red any more.
	add, _ := th.Slot(theme.ToolDiffAdded)
	rem, _ := th.Slot(theme.ToolDiffRemoved)
	if add == rem {
		t.Errorf("added and removed markers collapsed onto %+v", add)
	}
}

// A read of a .go file paints the source in Rosé Pine's syntax inks — the
// surface #582 built and the palette the user asked to see. This is the
// end-to-end half: the renderer resolves the theme's slots, so it fails if a
// slot stops being read, not only if its value changes.
func TestDefaultThemePaintsToolOutputInRosePine(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, _ := newTestApp(t, 100, 40)
	app.AddToolBlock("c1", "read", `{"path":"a.go"}`)
	app.FinishTool("c1", "read", false,
		"[pkg/a.go#1a2b]\n1:package main\n2:\n3:// note\n4:func main() { s := \"hi\"; _ = 42 }",
		ToolOutcome{})

	rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
	if !strings.Contains(text, "func main() {") {
		t.Fatalf("the source did not survive the render:\n%s", text)
	}
	// Keyed by the run's own text, which is the only honest key here: "main" is
	// not one of them because the lexer paints that word in two roles on this
	// fixture (`package main` an identifier, `func main` a function), and a
	// substring match records whichever came last — a fixture trap that reads
	// as a palette bug.
	want := map[string]string{
		"package": rpPine,
		"_":       rpText,
		"// note": rpMuted,
		"\"hi\"":  rpGold,
		"42":      rpRose,
	}
	got := map[string]tcell.Color{}
	for _, ln := range bodyRows(rows) {
		for _, r := range ln.runs {
			if r.chrome {
				continue
			}
			if _, wanted := want[r.text]; wanted {
				fg, _, _ := r.style.Decompose()
				got[r.text] = fg
			}
		}
	}
	for _, sub := range []string{"package", "_", "// note", "\"hi\"", "42"} {
		ink, ok := got[sub]
		if !ok {
			t.Errorf("%q never reached the screen as its own run; highlighting did nothing:\n%s", sub, text)
			continue
		}
		if c := app.cellColor(theme.Hex(want[sub])); ink != c {
			t.Errorf("%q painted %v, want Rosé Pine %s", sub, ink, want[sub])
		}
	}
}

// The diff's word band exists so the changed token reads louder than the rest
// of its row; a palette whose word band equalled its row band would leave the
// whole word-diff pass invisible. Pinned as literals for the same reason as the
// inks above: this must fail if the bands move, not describe where they are.
func TestDefaultThemeWordBandBeatsItsRowBand(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	th := theme.Load("groknight")
	row, ok := th.Slot(theme.ToolDiffAddedBg)
	if !ok {
		t.Fatal("the launch theme must pin the added row band")
	}
	word, ok := th.Slot(theme.ToolDiffAddedWordBg)
	if !ok {
		t.Fatal("the launch theme must pin the added word band")
	}
	remRow, _ := th.Slot(theme.ToolDiffRemovedBg)
	remWord, _ := th.Slot(theme.ToolDiffRemovedWordBg)
	if row == word || remRow == remWord {
		t.Fatalf("a word band equals its row band (%+v / %+v)", row, word)
	}
	if row == remRow {
		t.Fatalf("added and removed collapsed onto one band (%+v)", row)
	}
	// Claude Code's near-black tints, which are a stripe rather than a palette
	// identity — pinned so a "harmonise the band with the palette" tweak has to
	// argue with this line.
	for slot, hex := range map[string]string{
		theme.ToolDiffAddedBg:       "#022800",
		theme.ToolDiffRemovedBg:     "#3d0100",
		theme.ToolDiffAddedWordBg:   "#044700",
		theme.ToolDiffRemovedWordBg: "#5c0200",
	} {
		if c, _ := th.Slot(slot); c != theme.Hex(hex) {
			t.Errorf("%s = %+v, want the Claude Code tint %s", slot, c, hex)
		}
	}
}

// The transcript's diff and the sidebar's popup overlay are two renderers over
// one palette. A theme that reached one but not the other painted a diff that
// changed colour the moment the reader clicked it, so this drives the OVERLAY —
// the surface a sidebar click opens — and asks for the same two inks the
// transcript's box already wears. Asserted against the literals, for the same
// reason as everything above.
func TestDefaultThemePaintsTheSidebarOverlayDiffInRosePine(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	app, scr := newTestApp(t, 120, 30)

	app.mu.Lock()
	_, _, path := withDockRows(app)
	app.mu.Unlock()
	if path == "" {
		t.Skip("dock layout doesn't expose a clickable row")
	}
	app.mu.Lock()
	opened := app.openDiffOverlay(path)
	app.mu.Unlock()
	if !opened {
		t.Fatal("no diff overlay for the changed file")
	}
	app.draw()

	markers := map[rune]string{'+': rpFoam, '-': rpLove}
	seen := map[rune]bool{}
	for y := 0; y < app.height; y++ {
		for x := 0; x < app.width; x++ {
			ch, _, st, _ := scr.GetContent(x, y)
			hex, isMarker := markers[ch]
			if !isMarker {
				continue
			}
			fg, _, _ := st.Decompose()
			switch fg {
			case app.cellColor(theme.Hex(hex)):
				seen[ch] = true
			case app.cellColor(theme.Hex(rpFoam)), app.cellColor(theme.Hex(rpLove)),
				app.cellColor(theme.Hex(rpPine)), app.cellColor(theme.Hex(rpRose)),
				app.cellColor(theme.Hex(rpGold)):
				t.Errorf("overlay marker %q painted %v, which is neither Rosé Pine's %s nor the terminal default",
					string(ch), fg, hex)
			}
		}
	}
	for ch, hex := range markers {
		if !seen[ch] {
			t.Errorf("no %q marker painted %s anywhere in the overlay: the popup is not wearing the theme's diff inks", string(ch), hex)
		}
	}
}
