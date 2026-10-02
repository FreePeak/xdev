package tui

import (
	"os/exec"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// welcomeMenu is one row of the start-screen menu (grok welcome/menu.rs:
// label … shortcut, right-aligned within the column).
type welcomeMenu struct {
	Label string
	Key   string
}

// welcomeMenuItems returns the start-screen actions. Every row names a
// control that actually exists: the hint column is what to type (or the chord
// that is really bound), like grok's menu — a row advertising an unbound
// chord is a lie the user discovers at the keyboard.
func welcomeMenuItems() []welcomeMenu {
	return []welcomeMenu{
		{Label: "Resume session", Key: "/resume"},
		{Label: "New session", Key: "/new"},
		{Label: "Clear context", Key: "/clear"},
		{Label: "Quit", Key: "ctrl+c"},
	}
}

// xdevLogo is the welcome wordmark in Delta Corps Priest 1 — the same
// FIGlet face Omarchy draws its brand in (github.com/omacom/omarchy
// logo.txt / omarchy-ascii). Capital XDEV, condensed onto a fixed
// 41-cell grid (the font's native ~50 needs a 54-column terminal and
// vanishes in a common ~48-column window — the same problem Omarchy's
// own about art avoids by shipping a hand-fitted logo.txt).
//
// Solid block strokes with ▀▄▌▐ half-block joins — no shaded ░ noise —
// matching Omarchy logo.txt's clean silhouette. Every stem sits on one
// absolute column across all eight rows so nothing wobbles. The V is a
// deliberate taper: the stock face draws v like an open o ("XDeu" at
// logo size); the taper keeps stroke weight and baseline with the
// other three glyphs. E keeps three bars on a left stem (legible beside D) with Omarchy-style
// ▄/▀ soft ends so it stays artful rather than a slab.
//
// Same art at every size that fits; below the full mark's fit, the
// capital-X monogram (see logoArt).
var xdevLogo = []string{
	"▀███    ▐███▀ ███████▄  ▄███████ █▌    ▐█",
	"  ██▌   ███▀  ██    ▀██ ██       ██    ██",
	"   ██  ▐██    ██     ██ ██       ██    ██",
	"   ▀██▄██▀    ██     ██ ██████   ██    ██",
	"   ███▀██▄    ██     ██ ██        ██  ██",
	"  ▐██  ▀██    ██     ██ ██        ██  ██",
	" ▄██     ██▄  ██    ▄██ ██     ▄   ████",
	"███       ██▄ ███████▀  ▀███████    ██",
}

// xdevLogoMono is the capital-X monogram — narrow-pane fallback under
// the full mark's ~45-cell fit. Same face and stroke weight as the
// wordmark's leading X.
var xdevLogoMono = []string{
	"▀███    ▐███▀",
	"  ██▌   ███▀",
	"   ██  ▐██",
	"   ▀██▄██▀",
	"   ███▀██▄",
	"  ▐██  ▀██",
	" ▄██     ██▄",
	"███       ██▄",
}

// artWidth returns the widest art row in cells (runewidth).
func artWidth(art []string) int {
	w := 0
	for _, ln := range art {
		if lw := width(ln); lw > w {
			w = lw
		}
	}
	return w
}

// logoWidth returns the full wordmark width in cells (~41).
func logoWidth() int { return artWidth(xdevLogo) }

// logoArt returns the xdev logo for the given terminal size, or nil when
// nothing fits. Prefer the full wordmark; below its fit threshold fall back
// to the X monogram so a narrow pane still brands.
func logoArt(w, h int) []string {
	// 8 art rows + tagline + gap + 4 menu rows.
	if h < 14 {
		return nil
	}
	if w >= logoWidth()+4 {
		return xdevLogo
	}
	if mw := artWidth(xdevLogoMono); w >= mw+4 {
		return xdevLogoMono
	}
	return nil
}

// isFullWordmark reports whether art is the capital-XDEV mark (not the
// monogram and not empty). Slice identity is enough: logoArt returns the
// package vars directly.
func isFullWordmark(art []string) bool {
	return len(art) > 0 && &art[0] == &xdevLogo[0]
}

// --- Sheen sweep (welcome-screen logo) ---

// Sheen band geometry, ported from omarchy-branding-about-animation:
// two columns per row leans the band to ~45° on screen (a cell is
// about twice as tall as wide), ±2 columns is its half-width, and the
// rest pauses the sweep between passes. The phase advances one column
// per 33ms welcome tick (~30fps, see App.Run) — Omarchy About's cadence.
const (
	sheenSlant = 2
	sheenHalf  = 2
	sheenRest  = 16
)

// sheenInBand reports whether logo cell (row, col) sits under the
// sheen band at the given phase, for a logo of the given width and
// row count (full wordmark or monogram).
func sheenInBand(phase, row, col, logoW, rows int) bool {
	if rows == 0 {
		return false
	}
	period := logoW + (rows-1)*sheenSlant + 2*sheenHalf + 1 + sheenRest
	at := phase%period - 2*sheenHalf
	c := at - row*sheenSlant
	return col >= c-sheenHalf && col <= c+sheenHalf
}

// gitBranch returns the current git branch name, "" when not a repo.
func gitBranch(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// transcriptTop is the screen row the transcript viewport paints into: the
// persistent top bar owns row 0 whenever a transcript is on screen, and the
// session strip owns row 1 whenever more than one session is open. The
// selection geometry (anchoring, auto-scroll, row capture) shifts by it.
func (a *App) transcriptTop() int {
	if len(a.blocks) == 0 {
		return 0
	}
	if a.tabStripVisible() {
		return 2
	}
	return 1
}

// firstUserPrompt returns the first user message of the session,
// unclipped (no byte ceiling): the dock title may be long and only
// the front-end trims it for display. Caller holds a.mu.
func (a *App) firstUserPrompt() string {
	for _, b := range a.blocks {
		if b.Kind != KindUser {
			continue
		}
		return b.Text
	}
	return ""
}

// drawTopBar paints row 0 (grok top_bar.rs): while a turn is in flight the
// running spinner leads, then the git branch — the same theme frames and
// accent the tool rows and the composer's divider spin, so the indicator reads
// the same wherever it appears. The spinner leads because it is the bar's one
// thing that MOVES, and at the right end of a static branch it read as part
// of the branch name. Nothing else lives on this bar: the session's prompts
// used to ride it and were dropped (user request). Caller holds a.mu.
func (a *App) drawTopBar(s tcell.Screen) {
	dim := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	x := 1
	if a.st.Running {
		drawText(s, x, 0, a.spinFrame(),
			tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentRunning))))
		x += 2 // the frame and the space after it
	}
	if a.branch != "" {
		drawText(s, x, 0, "❯ "+a.branch, dim)
	}
}

// drawWelcome renders the start screen (grok welcome/mod.rs anatomy):
// top bar (git branch left), vertically centered logo + menu; the composer
// and status rows are drawn by the caller.
func (a *App) drawWelcome(s tcell.Screen, w, h int) {
	st := func(c theme.Color, bold bool) tcell.Style {
		st := tcell.StyleDefault.Foreground(a.cellColor(c))
		if bold {
			st = st.Bold(true)
		}
		return st
	}

	a.drawTopBar(s)

	// Monochrome like grok's welcome: white text, gray grue.
	whiteC, grayC := a.th.Get(theme.TextPrimary), a.th.Get(theme.Gray)

	// Logo + menu vertically centered in the content area (the composer
	// + status row occupy the bottom 4 rows).
	contentTop, contentH := 2, h-8
	logo := logoArt(w, contentH)
	menu := welcomeMenuItems()
	total := len(logo) + len(menu)
	if len(logo) > 0 {
		total += 2 // binary tagline + gap below the logo
	}
	if a.startupNotice != "" {
		total++ // grok's error/tip slot: a row of its own under the menu
	}
	y := contentTop + max(0, (contentH-total)/2)

	// One common left edge so the ragged-width pixel rows align
	// (per-line centering would wobble the letters).
	logoW := 0
	for _, ln := range logo {
		if lw := width(ln); lw > logoW {
			logoW = lw
		}
	}
	logoX := max(2, (w-logoW)/2)

	// Brand colour with the sheen riding across it (omarchy About
	// branding): the assistant accent for the wordmark, and the lit
	// band inverted — screen-bg ink on a TextPrimary plate — so the
	// sweep reads as one continuous slab of light instead of broken
	// bright cells in the art's gaps. The one colored element on an
	// otherwise monochrome screen, like omarchy's green logo.
	base := st(a.th.Get(theme.AccentAssistant), false)
	lit := tcell.StyleDefault.
		Foreground(a.cellColor(a.th.Get(theme.BgTerminal))).
		Background(a.cellColor(whiteC))
	for r, ln := range logo {
		c := 0
		for _, rn := range ln {
			style := base
			if sheenInBand(a.sheenPhase, r, c, logoW, len(logo)) {
				style = lit
			}
			s.SetContent(logoX+c, y+r, rn, nil, style)
			c++
		}
	}
	y += len(logo)
	// Full wordmark only — monogram is the same row count, so identity is
	// the art itself (logoArt returns the xdevLogo / xdevLogoMono slice).
	if isFullWordmark(logo) {
		tag := "01111000 01100100 01100101 01110110" // "xdev" in binary
		drawText(s, max(2, (w-width(tag))/2), y, tag, st(grayC, false))
		y++
	} else if len(logo) > 0 {
		y++ // keep the gap under the monogram even without the tagline
	}
	y++ // gap between logo and menu

	// Clear the band behind the menu + notice block (one column of margin): a
	// cell left over from the previous frame between the key and the edge reads
	// as garbage in a row that is otherwise aligned chrome.
	noticeRows := len(strings.Split(a.startupNotice, "\n"))
	if a.startupNotice == "" {
		noticeRows = 0
	}
	// Menu: label left, hotkey right-aligned in a centered column
	// (grok menu.rs). On a narrow window the column doesn't fit —
	// fall back to label + key inline. The column is as wide as the mark it
	// sits under, like grok's render_menu (logo_visual_width().max(30)): a
	// fixed 28 left the menu floating off the logo's own edges.
	colW := max(28, logoW)
	for _, m := range menu {
		if cw := width(m.Label) + width(m.Key) + 4; cw > colW {
			colW = cw
		}
	}
	if w >= colW+4 {
		x0 := max(2, (w-colW)/2)
		for yy := y; yy < min(h, y+len(menu)+noticeRows); yy++ {
			for xx := max(0, x0-1); xx < min(w, x0+colW+1); xx++ {
				s.SetContent(xx, yy, ' ', nil, st(whiteC, false))
			}
		}
		for _, m := range menu {
			drawText(s, x0, y, m.Label, st(a.th.Get(theme.TextPrimary), true))
			drawText(s, x0+colW-width(m.Key), y, m.Key, st(a.th.Get(theme.Gray), false))
			y++
		}
	} else {
		for _, m := range menu {
			drawText(s, 2, y, m.Label, st(a.th.Get(theme.TextPrimary), true))
			drawText(s, 2+width(m.Label)+2, y, m.Key, st(a.th.Get(theme.Gray), false))
			y++
		}
	}
	a.drawStartupNotice(s, w, y, st(grayC, false))
}

// drawStartupNotice paints the startup report (see App.SetStartupNotice) on
// its own rows below the menu — grok's error/tip slot — centred, clipped to
// the screen and truncated to width so a long agent list can never spill past
// the edge. Dim: it is a fact about the build, not a turn of the conversation.
func (a *App) drawStartupNotice(s tcell.Screen, w, y int, dim tcell.Style) {
	for _, ln := range strings.Split(a.startupNotice, "\n") {
		if y >= a.height {
			return
		}
		if w <= 4 { // too narrow to centre anything in
			return
		}
		txt := truncateCells(ln, w-4, "…")
		drawText(s, max(2, (w-width(txt))/2), y, txt, dim)
		y++
	}
}
