package tui

import (
	"math/rand/v2"
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

// welcomeMenuItems returns the start-screen actions for the current
// session state.
func welcomeMenuItems(hasHistory bool) []welcomeMenu {
	items := []welcomeMenu{}
	if hasHistory {
		items = append(items, welcomeMenu{Label: "Resume session", Key: "ctrl+r"})
	}
	items = append(items,
		welcomeMenu{Label: "New session", Key: "/new"},
		welcomeMenu{Label: "Clear context", Key: "/clear"},
		welcomeMenu{Label: "Quit", Key: "ctrl+c"},
	)
	return items
}

// xdevLogo is the one welcome logo: "XDEV" in the FIGlet font
// Delta Corps Priest 1 — the same font the Omarchy wordmark is drawn
// in (omarchy-ascii). Solid block strokes with ▀▄▌▐ half-blocks for
// the rounded joins; no shaded ░ noise, so the mark reads cleanly at
// any size.
//
// X, D and e are the font's own glyphs, drawn here on a fixed 41-cell
// grid instead of taken at the font's native 49: 49 plus margins needs
// a 53-column terminal, and a common ~48-column window showed no logo
// at all. Every stem sits on one absolute column across all eight
// rows (the X's arms slide one column per row); nothing is resampled,
// so no stroke wobbles.
//
// The v is the one deliberate departure: Delta Corps Priest 1 draws v
// as an open o (identical to its u but for the stem), which reads as
// "U" at logo size, so here it tapers to a point at the same stroke
// weight and baseline. It renders identically at every terminal size;
// the only size-dependent choice is whether it fits at all (see
// logoArt).
var xdevLogo = []string{
	"▀███    ▐███▀ ███████▄   ▄████▄  █▌    ▐█",
	"  ██▌   ███▀  ██    ▀██ ██    ██ ██    ██",
	"   ██  ▐██    ██     ██ ██    █▀ ██    ██",
	"   ▀██▄██▀    ██     ██ ▄██▄▄▄   ██    ██",
	"   ███▀██▄    ██     ██ ▀▀██▀▀▀   ██  ██",
	"  ▐██  ▀██    ██     ██ ██    █▄  ██  ██",
	" ▄██     ██▄  ██    ▄██ ██    ██   ████",
	"███       ██▄ ███████▀  ████████    ██",
}

// logoWidth returns the widest art row in cells.
func logoWidth() int {
	w := 0
	for _, ln := range xdevLogo {
		if lw := width(ln); lw > w {
			w = lw
		}
	}
	return w
}

// logoArt returns the xdev logo for the given terminal size, or nil
// when the terminal can't fit it: content shorter than the 8 art rows
// plus tagline, gap and menu, or narrower than the 41-cell art plus
// margins. One logo at every size — no variant swapping, so the
// artwork never changes shape between terminal sizes.
func logoArt(w, h int) []string {
	if h < 14 || w < logoWidth()+4 {
		return nil
	}
	return xdevLogo
}

// --- Sheen sweep (welcome-screen logo) ---

// Sheen band geometry, ported from omarchy-branding-about-animation:
// two columns per row leans the band to ~45° on screen (a cell is
// about twice as tall as wide), ±2 columns is its half-width, and the
// rest pauses the sweep between passes. The phase advances one column
// per 33ms welcome tick (~30fps, see App.Run), so a pass over the
// 41-cell mark takes ~2.5s including its rest.
const (
	sheenSlant = 2
	sheenHalf  = 2
	sheenRest  = 16
)

// sheenInBand reports whether logo cell (row, col) sits under the
// sheen band at the given phase, for a logo of the given width.
func sheenInBand(phase, row, col, logoW int) bool {
	rows := len(xdevLogo)
	if rows == 0 {
		return false
	}
	// One period: band fully off the left (centre -2*half), across
	// the art and off the right of the lowest row (+logoW plus the
	// slant's worth), then the rest.
	period := logoW + (rows-1)*sheenSlant + 2*sheenHalf + 1 + sheenRest
	at := phase%period - 2*sheenHalf
	c := at - row*sheenSlant
	return col >= c-sheenHalf && col <= c+sheenHalf
}

// --- Conway's Game of Life backdrop (welcome screen only) ---

// lifeGlyph is the rune painted for a live cell ('▓' shaded block —
// narrower-shade sibling of the logo's '█', and width-1 so it can't
// punch holes in the art).
const lifeGlyph = '▓'

// lifeGrid is one Game-of-Life generation: a w×h cell grid, wrapped
// toroidally so edges stay lively.
type lifeGrid struct {
	w, h int
	c    [][]bool
}

func newLifeGrid(w, h int) lifeGrid {
	c := make([][]bool, h)
	for y := range h {
		c[y] = make([]bool, w)
	}
	return lifeGrid{w: w, h: h, c: c}
}

// at reports the cell at (x, y), toroidally wrapped.
func (g lifeGrid) at(x, y int) bool {
	return g.c[((y%g.h)+g.h)%g.h][((x%g.w)+g.w)%g.w]
}

// neighbors counts the eight wrapped neighbors of (x, y).
func (g lifeGrid) neighbors(x, y int) int {
	n := 0
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if (dx != 0 || dy != 0) && g.at(x+dx, y+dy) {
				n++
			}
		}
	}
	return n
}

// step advances g one generation under the standard B3/S23 rules.
func (g *lifeGrid) step() {
	next := make([][]bool, g.h)
	for y := range g.h {
		next[y] = make([]bool, g.w)
		for x := range g.w {
			n := g.neighbors(x, y)
			next[y][x] = n == 3 || (n == 2 && g.c[y][x])
		}
	}
	g.c = next
}

// ensureLife (re)seeds the backdrop after size changes: a sparse
// random soup (~15% — the inset band reads as a small monochrome
// texture, not a wall of cells), pre-run a few generations so the
// screen doesn't open on pure noise. Caller holds a.mu.
func (a *App) ensureLife(w, top, bot int) {
	h := bot - top + 1
	if a.life.w == w && a.life.h == h {
		return
	}
	a.life = newLifeGrid(w, h)
	for y := range h {
		for x := range w {
			a.life.c[y][x] = rand.Float64() < 0.15
		}
	}
	for range 6 {
		a.life.step()
	}
}

// stepLife advances the world one generation. Two random mutations per
// step keep still lifes from freezing the backdrop forever.
// ponytail: population-based respawn would be the upgrade path if the
// churn ever looks too noisy. Self-heals an unseeded or resized grid
// via ensureLife, mirroring the old stepRain. Caller holds a.mu.
func (a *App) stepLife(w, top, bot int) {
	a.ensureLife(w, top, bot)
	a.life.step()
	for range 2 {
		x, y := rand.IntN(a.life.w), rand.IntN(a.life.h)
		a.life.c[y][x] = !a.life.c[y][x]
	}
}

// drawLife paints the live cells in shades of gray behind the logo:
// lighter dense cores, mid sparse clusters, dim isolated cells.
// left insets the band from the screen edge. Caller holds a.mu.
func (a *App) drawLife(s tcell.Screen, left, top int, dense, mid, dim tcell.Style) {
	for y := range a.life.h {
		for x := range a.life.w {
			if !a.life.c[y][x] {
				continue
			}
			st := mid
			switch n := a.life.neighbors(x, y); {
			case n >= 4:
				st = dense
			case n <= 1:
				st = dim
			}
			s.SetContent(left+x, top+y, lifeGlyph, nil, st)
		}
	}
}

// lifeArea returns the backdrop's grid width and row range: a band
// inset from the screen edges — a slim strip around the centered
// logo — and skipped entirely on terminals too small to spare it.
func lifeArea(w, h int) (gw, top, bot int, ok bool) {
	return w - 20, 3, h - 8, w >= 48 && h >= 18
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
// persistent top bar owns row 0 whenever a transcript is on screen. The
// selection geometry (anchoring, auto-scroll, row capture) shifts by it.
func (a *App) transcriptTop() int {
	if len(a.blocks) == 0 {
		return 0
	}
	return 1
}

// lastPrompt returns the newest user prompt collapsed to one line — the top
// bar's "where was I" anchor once the prompt itself scrolls away. Tabs and
// control bytes are sanitized so the width math matches what is painted.
// Caller holds a.mu.
func (a *App) lastPrompt() string {
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind != KindUser {
			continue
		}
		s, _, _ := strings.Cut(sanitizeOutput(b.Text), "\n")
		return strings.Join(strings.Fields(s), " ")
	}
	return ""
}

// drawTopBar paints row 0 (grok top_bar.rs): the git branch left; with a
// transcript it also carries the last user prompt, so the request the screen is
// answering never scrolls out of sight. Neither the working directory nor the
// model name is shown here — the status row and the composer's info divider
// carry them, and the prompt gets the freed width. Caller holds a.mu.
func (a *App) drawTopBar(s tcell.Screen, w int, withPrompt bool) {
	dim := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.GrayDim)))
	promptSt := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.Gray)))
	left := ""
	if a.branch != "" {
		left = "❯ " + a.branch
	}
	x := 1
	if left != "" {
		drawText(s, x, 0, left, dim)
		x += width(left)
	}
	prompt := ""
	if withPrompt {
		prompt = a.lastPrompt()
	}
	if prompt != "" {
		sep := " · "
		if left == "" {
			sep = "❯ "
		}
		if room := w - 2 - x - width(sep) - 1; room > 1 {
			if width(prompt) > room {
				prompt = truncateCells(prompt, room, "…")
			}
			drawText(s, x, 0, sep, dim)
			drawText(s, x+width(sep), 0, prompt, promptSt)
		}
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

	a.drawTopBar(s, w, false)

	// Monochrome like grok's welcome: white text, gray grue.
	whiteC, grayC, dimC := a.th.Get(theme.TextPrimary), a.th.Get(theme.Gray), a.th.Get(theme.GrayDim)

	// Life band between the top bar and the composer; the logo and
	// menu are drawn over it. Skipped on small terminals.
	gw, top, bot, ok := lifeArea(w, h)
	if ok {
		a.ensureLife(gw, top, bot)
		a.drawLife(s, 10, top, st(a.th.Get(theme.GrayBright), false), st(grayC, false), st(dimC, false))
	}

	// Logo + menu vertically centered in the content area (the composer
	// + status row occupy the bottom 4 rows).
	contentTop, contentH := 2, h-8
	logo := logoArt(w, contentH)
	menu := welcomeMenuItems(len(a.blocks) > 0)
	total := len(logo) + len(menu)
	if len(logo) > 0 {
		total += 2 // binary tagline + gap below the logo
	}
	y := contentTop + max(0, (contentH-total)/2)

	// One common left edge so the ragged-width block-letter rows align
	// (per-line centering would wobble the letters).
	logoW := 0
	for _, ln := range logo {
		if lw := width(ln); lw > logoW {
			logoW = lw
		}
	}
	logoX := max(2, (w-logoW)/2)
	if len(logo) > 0 {
		// Clear the band behind the logo block (a margin around the
		// art) so '▓' cells never sit flush against the '░' shading —
		// that collision read as noise, not letterforms. Clamped to
		// the screen: small widths can put logoX-3 < 0.
		for yy := max(0, y-1); yy < min(h, y+len(logo)+1); yy++ {
			for xx := max(0, logoX-3); xx < min(w, logoX+logoW+3); xx++ {
				s.SetContent(xx, yy, ' ', nil, st(whiteC, false))
			}
		}
	}

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
			if sheenInBand(a.sheenPhase, r, c, logoW) {
				style = lit
			}
			s.SetContent(logoX+c, y+r, rn, nil, style)
			c++
		}
	}
	y += len(logo)
	if len(logo) > 0 {
		tag := "01111000 01100100 01100101 01110110" // "xdev" in binary
		drawText(s, max(2, (w-width(tag))/2), y, tag, st(grayC, false))
		y++
	}
	y++ // gap between logo and menu

	// Menu: label left, hotkey right-aligned in a centered column
	// (grok menu.rs). On a narrow window the column doesn't fit —
	// fall back to label + key inline.
	colW := 28
	for _, m := range menu {
		if cw := width(m.Label) + width(m.Key) + 4; cw > colW {
			colW = cw
		}
	}
	if w < colW+4 {
		for _, m := range menu {
			drawText(s, 2, y, m.Label, st(a.th.Get(theme.TextPrimary), true))
			drawText(s, 2+width(m.Label)+2, y, m.Key, st(a.th.Get(theme.Gray), false))
			y++
		}
		return
	}
	x0 := max(2, (w-colW)/2)
	for _, m := range menu {
		drawText(s, x0, y, m.Label, st(a.th.Get(theme.TextPrimary), true))
		drawText(s, x0+colW-width(m.Key), y, m.Key, st(a.th.Get(theme.Gray), false))
		y++
	}
}
