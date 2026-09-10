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
		welcomeMenu{Label: "Quit", Key: "ctrl+q"},
	)
	return items
}

// xdevLogo is the one welcome logo: ANSI-shadow "XDev". It renders
// identically at every terminal size — the only size-dependent choice
// is whether it fits at all (see logoArt).
var xdevLogo = []string{
	"██╗  ██╗  ██████╗",
	"██║ ██╔╝  ██╔══██╗",
	"█████╔╝  ██║  ██║   █████╗   ██║   ██║",
	"██╔═██╗  ██║  ██║   ██╔══╝   ╚██╗ ██╔╝",
	"██║  ██╗  ██████╔╝  ███████╗   ╚████╔╝",
	"╚═╝  ╚═╝  ╚═════╝   ╚══════╝    ╚═══╝",
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
// when the terminal can't fit it: content shorter than the 6 art rows
// plus tagline and menu, or narrower than the 38-cell art plus
// margins. One logo at every size — no variant swapping, so the
// artwork never changes shape between terminal sizes.
func logoArt(w, h int) []string {
	if h < 12 || w < logoWidth()+4 {
		return nil
	}
	return xdevLogo
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

// ensureLife (re)seeds the backdrop after size changes: a random soup
// at ~28% density, pre-run a few generations so the screen doesn't
// open on pure noise. Caller holds a.mu.
func (a *App) ensureLife(w, top, bot int) {
	h := bot - top + 1
	if a.life.w == w && a.life.h == h {
		return
	}
	a.life = newLifeGrid(w, h)
	for y := range h {
		for x := range w {
			a.life.c[y][x] = rand.Float64() < 0.28
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

// drawLife paints the live cells behind the logo: bright dense cores,
// mid sparse clusters, dim isolated cells. Caller holds a.mu.
func (a *App) drawLife(s tcell.Screen, top int, head, mid, dim tcell.Style) {
	for y := range a.life.h {
		for x := range a.life.w {
			if !a.life.c[y][x] {
				continue
			}
			st := mid
			switch n := a.life.neighbors(x, y); {
			case n >= 4:
				st = head
			case n <= 1:
				st = dim
			}
			s.SetContent(x, top+y, lifeGlyph, nil, st)
		}
	}
}

// lifeArea returns the backdrop area between the top bar and the
// composer; ok=false when the terminal is too small for one.
func lifeArea(w, h int) (top, bot int, ok bool) {
	top, bot = 1, h-5
	return top, bot, bot > top+3 && w >= 8
}

// cwdShort renders the top-bar location: last two path components.
func cwdShort(cwd string) string {
	parts := strings.Split(strings.TrimRight(cwd, "/"), "/")
	if len(parts) > 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return cwd
}

// gitBranch returns the current git branch name, "" when not a repo.
func gitBranch(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// drawWelcome renders the start screen (grok welcome/mod.rs anatomy):
// top bar (cwd:branch left, model right), vertically centered logo +
// menu; the composer and shortcuts rows are drawn by the caller.
func (a *App) drawWelcome(s tcell.Screen, w, h int) {
	st := func(c theme.Color, bold bool) tcell.Style {
		st := tcell.StyleDefault.Foreground(a.cellColor(c))
		if bold {
			st = st.Bold(true)
		}
		return st
	}

	// Top bar: cwd:branch left, model right (grok top_bar.rs).
	left := "❯ " + a.cwdLabel
	if a.branch != "" {
		left += ":" + a.branch
	}
	drawText(s, 1, 0, left, st(a.th.Get(theme.GrayDim), false))
	right := a.st.Model
	drawText(s, w-len(right)-2, 0, right, st(a.th.Get(theme.GrayDim), false))

	// CRT phosphor greens; deepened on the light theme so they stay
	headC, midC, dimC := theme.Hex("#00ff41"), theme.Hex("#00a828"), theme.Hex("#005c22")
	if !a.th.Dark {
		headC, midC, dimC = theme.Hex("#006e24"), theme.Hex("#00511b"), theme.Hex("#003812")
	}

	// Life-grid backdrop between the top bar and the composer; the
	// logo and menu are drawn over it.
	top, bot, ok := lifeArea(w, h)
	if ok {
		a.ensureLife(w, top, bot)
		a.drawLife(s, top, st(headC, false), st(midC, false), st(dimC, false))
	}

	// Logo + menu vertically centered in the content area (the composer
	// + shortcuts occupy the bottom 4 rows).
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
	for i, ln := range logo {
		c := midC
		if i < len(logo)/2 {
			c = headC
		}
		drawText(s, logoX, y, ln, st(c, false))
		y++
	}
	if len(logo) > 0 {
		tag := "01111000 01100100 01100101 01110110" // "xdev" in binary
		drawText(s, max(2, (w-width(tag))/2), y, tag, st(midC, false))
		y++
	}
	y++ // gap between logo and menu

	// Menu: label left, hotkey right-aligned in a centered column
	// (grok menu.rs).
	colW := 28
	for _, m := range menu {
		if cw := width(m.Label) + width(m.Key) + 4; cw > colW {
			colW = cw
		}
	}
	x0 := max(2, (w-colW)/2)
	for _, m := range menu {
		drawText(s, x0, y, m.Label, st(a.th.Get(theme.TextPrimary), true))
		drawText(s, x0+colW-width(m.Key), y, m.Key, st(a.th.Get(theme.Gray), false))
		y++
	}
}
