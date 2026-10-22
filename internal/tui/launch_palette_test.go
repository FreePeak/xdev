package tui

// The launch themes wear their own palettes, and these tests are the
// executable form of that claim. Each one fails against the palette it
// replaced — which is the point: a palette migration only ever checked by
// eye drifts back on the next "small tweak" to the diff block.
//
// The expected values are PINNED LITERALS, never read back out of the theme.
// A test that asks the theme what colour it should have painted is a test that
// passes on the old palette too: the first version of this file did exactly
// that and went green against the reverted theme, which is the whole reason
// the constants are here rather than a `theme.Load(...)` lookup.
//
// The two launch themes carry DIFFERENT palettes, and the file is named after
// neither of them:
//
//   - The DARK theme is omp's groknight (the file omp ships at
//     ~/.omp/agent/themes/groknight.json, vars expanded): a tan accent over
//     xAI's neutral greys, with omp's own syntax and diff tokens.
//
//   - The LIGHT theme is Rosé Pine Dawn's own groups (MIT,
//     github.com/rose-pine/rose-pine-theme) — omp ships no light counterpart
//     of groknight, so there is nothing to mirror.
//
// SCOPE, deliberately: this file pins the DIFF inks and the SYNTAX inks —
// the tool-output and code surfaces — and the dock's own three inks, on BOTH
// launch themes. The chrome, the body ink and the status row are not pinned
// here: the terminal's own background already supplies the canvas, and moving
// chrome onto a palette is a different change with a different blast radius.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// omp's groknight, by token name. The `omp` prefix is the DARK launch theme;
// `dawn` is the LIGHT one. Dawn is NOT main's values darkened: on a
// near-white canvas the darker end of each pair is the readable one, which is
// why the added marker is pine here and green there.
const (
	ompAccent    = "#e0af68" // accent — the identity ink
	ompMuted     = "#6c6c6c" // muted / textGhost
	ompSubtle    = "#6c6c6c" // the operator and punctuation ink
	ompText      = "#e1e1e1" // text
	ompFaint     = "#c8c8c8" // textFaint
	ompGreen     = "#9ece6a" // green — the added marker
	ompRed       = "#e15759" // red — the removed marker
	ompTeal      = "#73daca" // the string ink
	ompOrange    = "#ff9e64" // the number ink
	ompStatusPth = "#6c6c6c" // statusLinePath — the dock's path ink

	dawnText   = "#575279" // text
	dawnSubtle = "#797593" // subtle
	dawnMuted  = "#9893a5" // muted — the comment ink (see theme.go)
	dawnLove   = "#b4637a" // love
	dawnGold   = "#ea9d34" // gold
	dawnRose   = "#d7827e" // rose
	dawnPine   = "#286983" // pine
	dawnFoam   = "#56949f" // foam
	dawnAccent = "#7D4BC6" // the light theme's accent
	dawnPath   = "#767676" // statusLinePath
)

// TestLaunchDarkThemeIsOmpGroknight pins every readable ink the dark launch
// theme owns. The chrome (canvas, borders, the neutral greys) is not pinned
// here — the terminal's own background already supplies the canvas, and
// chrome carrying a palette's identity is what made the two themes read as
// different products — so this asserts the inks and says nothing about
// bg_base.
func TestLaunchDarkThemeIsOmpGroknight(t *testing.T) {
	th := theme.Load("groknight")
	if !th.Dark {
		t.Fatal("groknight must be dark")
	}
	want := map[string]string{
		// Syntax: the 9 fenced-code / tool-output roles, mapped onto omp's own
		// groknight tokens.
		theme.SyntaxComment:     ompMuted,
		theme.SyntaxKeyword:     ompAccent,
		theme.SyntaxString:      ompTeal,
		theme.SyntaxNumber:      ompOrange,
		theme.SyntaxType:        ompText,
		theme.SyntaxVariable:    ompFaint,
		theme.SyntaxFunction:    ompText,
		theme.SyntaxOperator:    ompSubtle,
		theme.SyntaxPunctuation: ompSubtle,
		// The diff's three inks: omp's own git groups.
		theme.ToolDiffAdded:   ompGreen,
		theme.ToolDiffRemoved: ompRed,
		theme.ToolDiffContext: ompMuted,
	}
	for slot, hex := range want {
		got, ok := th.Slot(slot)
		if !ok {
			t.Errorf("%s is left to the terminal default; the launch theme must pin it", slot)
			continue
		}
		if wantC := theme.Hex(hex); got != wantC {
			t.Errorf("%s = %+v, want omp's groknight %s", slot, got, hex)
		}
	}
}

// The nine syntax roles collapse onto six colours on purpose — a comment, an
// operator and punctuation share one ink, and a function shares another's —
// so the contract is not "every role distinct" but "the classes a reader
// tells apart are apart": a comment, a string, a number and a keyword must be
// four different inks, or the highlighting says nothing. This is the same
// claim TestFencedGoBlockColorsItsTokens makes about the renderer; this one is
// about the palette underneath it, so it names the palette's own values.
//
// Table over BOTH launch themes: the dark twin and the light one are separate
// tables that separate drift, and a distinctness contract that holds for only
// one of them is half a contract.
func TestLaunchThemesKeepTokenClassesApart(t *testing.T) {
	for _, name := range []string{"groknight", "grokday"} {
		th := theme.Load(name)
		distinct := []string{
			theme.SyntaxComment, theme.SyntaxKeyword,
			theme.SyntaxString, theme.SyntaxNumber, theme.SyntaxType,
		}
		seen := map[theme.Color]string{}
		for _, slot := range distinct {
			c, ok := th.Slot(slot)
			if !ok {
				t.Errorf("%s: %s is unset", name, slot)
				continue
			}
			if prev, dup := seen[c]; dup {
				t.Errorf("%s: %s collapsed onto %s at %+v: two token classes a reader tells apart wear one ink", name, slot, prev, c)
			}
			seen[c] = slot
		}
		// The diff's polarity pair must be two different inks for the same
		// reason, even though neither is a green or a red any more.
		add, _ := th.Slot(theme.ToolDiffAdded)
		rem, _ := th.Slot(theme.ToolDiffRemoved)
		if add == rem {
			t.Errorf("%s: added and removed markers collapsed onto %+v", name, add)
		}
	}
}

// A read of a .go file paints the source in the launch theme's own syntax
// inks — the surface #501 built and the palette the user asked to see. This
// is the end-to-end half: the renderer resolves the theme's slots, so it
// fails if a slot stops being read, not only if its value changes.
//
// Table over both launch themes. The fixture is identical and only the palette
// differs, so a renderer path that quietly resolved ONE theme's slots — or a
// token table filled in from the dark twin's values — fails here rather than
// shipping.
func TestLaunchThemesPaintToolOutputInTheirPalette(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, tc := range []struct {
		name string
		want map[string]string
	}{
		{"groknight", map[string]string{
			"package": ompAccent, "_": ompFaint, "// note": ompMuted,
			"\"hi\"": ompTeal, "42": ompOrange,
		}},
		{"grokday", map[string]string{
			"package": dawnPine, "_": dawnText, "// note": dawnMuted,
			"\"hi\"": dawnGold, "42": dawnRose,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := newTestApp(t, 100, 40)
			app.SetTheme(theme.Load(tc.name))
			app.AddToolBlock("c1", "read", `{"path":"a.go"}`)
			app.FinishTool("c1", "read", false,
				"[pkg/a.go#1a2b]\n1:package main\n2:\n3:// note\n4:func main() { s := \"hi\"; _ = 42 }",
				ToolOutcome{})

			rows, text := joinBox(t, app, app.blocks[len(app.blocks)-1], 96)
			if !strings.Contains(text, "func main() {") {
				t.Fatalf("the source did not survive the render:\n%s", text)
			}
			// Keyed by the run's own text, which is the only honest key here:
			// "main" is not one of them because the lexer paints that word in
			// two roles on this fixture (`package main` an identifier, `func
			// main` a function), and a substring match records whichever came
			// last — a fixture trap that reads as a palette bug.
			got := map[string]tcell.Color{}
			for _, ln := range rows {
				for _, r := range ln.runs {
					if r.chrome {
						continue
					}
					if _, wanted := tc.want[r.text]; wanted {
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
				if c := app.cellColor(theme.Hex(tc.want[sub])); ink != c {
					t.Errorf("%q painted %v, want %s", sub, ink, tc.want[sub])
				}
			}
		})
	}
}

// The diff's word band exists so the changed token reads louder than the rest
// of its row; a palette whose word band equalled its row band would leave the
// whole word-diff pass invisible. Pinned as literals for the same reason as
// the inks above: this must fail if the bands move, not describe where they
// are.
func TestLaunchThemesWordBandBeatsItsRowBand(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, tc := range []struct {
		name  string
		tints map[string]string
	}{
		// Claude Code's near-black tints on the dark canvas, its near-white
		// ones on the light one. Pinned so a "harmonise the band with the
		// palette" tweak has to argue with this table: the bands are a stripe,
		// not an identity.
		{"groknight", map[string]string{
			theme.ToolDiffAddedBg:       "#022800",
			theme.ToolDiffRemovedBg:     "#3d0100",
			theme.ToolDiffAddedWordBg:   "#044700",
			theme.ToolDiffRemovedWordBg: "#5c0200",
		}},
		{"grokday", map[string]string{
			theme.ToolDiffAddedBg:       "#dcffdc",
			theme.ToolDiffRemovedBg:     "#ffdcdc",
			theme.ToolDiffAddedWordBg:   "#b2ffb2",
			theme.ToolDiffRemovedWordBg: "#ffc7c7",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := theme.Load(tc.name)
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
			for slot, hex := range tc.tints {
				if c, _ := th.Slot(slot); c != theme.Hex(hex) {
					t.Errorf("%s = %+v, want the Claude Code tint %s", slot, c, hex)
				}
			}
		})
	}
}

// The transcript's diff and the sidebar's popup overlay are two renderers over
// one palette. A theme that reached one but not the other painted a diff that
// changed colour the moment the reader clicked it, so this drives the OVERLAY —
// the surface a sidebar click opens — and asks for the same two inks the
// transcript's box already wears. Asserted against the literals, for the same
// reason as everything above.
func TestLaunchThemesPaintTheSidebarOverlayDiffInTheirPalette(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, tc := range []struct {
		name    string
		markers map[rune]string // the palette's two diff inks
	}{
		{"groknight", map[rune]string{'+': ompGreen, '-': ompRed}},
		{"grokday", map[rune]string{'+': dawnPine, '-': dawnLove}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, scr := newTestApp(t, 120, 30)
			app.SetTheme(theme.Load(tc.name))

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

			seen := map[rune]bool{}
			for y := 0; y < app.height; y++ {
				for x := 0; x < app.width; x++ {
					ch, _, st, _ := scr.GetContent(x, y)
					hex, isMarker := tc.markers[ch]
					if !isMarker {
						continue
					}
					fg, _, _ := st.Decompose()
					switch fg {
					case app.cellColor(theme.Hex(hex)):
						seen[ch] = true
					case app.cellColor(theme.Hex(dawnPine)), app.cellColor(theme.Hex(dawnLove)):
						// The OTHER variant's pair: the overlay wearing the
						// wrong launch theme's inks is exactly the drift this
						// subtest exists to catch.
						t.Errorf("overlay marker %q painted the other variant's ink %v, want %s",
							string(ch), fg, hex)
					}
				}
			}
			for ch, hex := range tc.markers {
				if !seen[ch] {
					t.Errorf("no %q marker painted %s anywhere in the overlay: the popup is not wearing the theme's diff inks", string(ch), hex)
				}
			}
		})
	}
}

// The sidebar is a surface the palette reaches, and this is the end-to-end
// half: drawDock resolves the three inks it wears — the accent on a section
// heading, the status line's path ink on a changed file's path, and the
// comment ink on a note the panel wrote about itself. Asserted against the
// pinned literals, for the same reason as everything above — a test that asks
// the theme what it should have painted passes on the palette it replaced.
//
// The fixtures are the test app's own (dockTestApp), never a real repository.
func TestLaunchThemesPaintTheSidebarInTheirPalette(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, tc := range []struct {
		name  string
		label string // a section heading: the accent
		path  string // a changed file's path: statusLinePath
		note  string // the identity footer: syntax_comment
	}{
		{"groknight", ompAccent, ompStatusPth, ompMuted},
		{"grokday", dawnAccent, dawnPath, dawnMuted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, scr, _ := dockTestApp(t, 160, 40)
			app.SetTheme(theme.Load(tc.name))
			app.SetDockMode(DockShow)
			app.SetVersion("0.4.127")
			app.SetLocation("/tmp/somewhere")
			app.AddToolBlock("", "edit", `{"path":"internal/tui/dock.go"}`)
			app.FinishTool("", "edit", false, "edited",
				ToolOutcome{Diff: "--- a/x\n+++ b/internal/tui/dock.go\n@@ -1,2 +1,3 @@\n ctx\n+added\n-removed\n"})
			app.draw()

			app.mu.Lock()
			left := app.width - dockCols
			top, _ := app.dockGrid()
			headRows := app.dock.titleRows()
			rows := append([]dockRow(nil), app.dock.lines...)
			app.mu.Unlock()

			// Read the ink the panel actually painted, per row, off the screen: the
			// panel is one window, so a row's first non-blank cell carries its ink.
			inkAt := func(y int) tcell.Color {
				for x := left + dockPad; x < app.width; x++ {
					ch, _, st, _ := scr.GetContent(x, y)
					if ch == ' ' || ch == 0 {
						continue
					}
					fg, _, _ := st.Decompose()
					return fg
				}
				return tcell.ColorDefault
			}

			// One row per role, found by the row's own shape: a heading has head
			// set, a changed file carries a path, and the footer's last row names
			// the build. Asserted against the literals, so this fails if a slot
			// stops being read rather than only if its value changes.
			want := map[string]tcell.Color{
				"heading": app.cellColor(theme.Hex(tc.label)),
				"path":    app.cellColor(theme.Hex(tc.path)),
				"note":    app.cellColor(theme.Hex(tc.note)),
			}
			seen := map[string]bool{}
			for i, r := range rows {
				y := top + headRows + i
				switch {
				case r.head:
					if got := inkAt(y); got == want["heading"] {
						seen["heading"] = true
					} else {
						t.Errorf("section heading %q painted %v, want %s", r.text, got, tc.label)
					}
				case r.path != "":
					if got := inkAt(y); got == want["path"] {
						seen["path"] = true
					} else {
						t.Errorf("changed-file row %q painted %v, want %s", r.text, got, tc.path)
					}
				case strings.HasPrefix(r.text, "xdev "):
					if got := inkAt(y); got == want["note"] {
						seen["note"] = true
					} else {
						t.Errorf("footer row %q painted %v, want %s", r.text, got, tc.note)
					}
				}
			}
			for _, role := range []string{"heading", "path", "note"} {
				if !seen[role] {
					t.Errorf("no %s row painted the palette's ink anywhere in the panel", role)
				}
			}
		})
	}
}
