package tui

import (
	"github.com/gdamore/tcell/v2"
	"math/rand/v2"
	"os/exec"
	"strings"

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

// logoArt returns the xdev logo for the given terminal height:
// ANSI-shadow "XDEV" block art on tall terminals, the 5-row block
// letters below 22 rows, hidden under 10 (grok welcome/logo.rs tiers).
func logoArt(height int) []string {
	switch {
	case height < 10:
		return nil
	case height < 22:
		return []string{
			"█   █   ████   ██  █   █",
			" █ █     █    █  █   █",
			"  █    █   █  ████   █ █ ",
			" █ █   █   █  █     █ █ ",
			"█   █   ████   ██    █  ",
		}
	default:
		return []string{
			"██╗  ██╗  ██████╗   ███████╗  ██╗   ██╗",
			"██║ ██╔╝  ██╔══██╗  ██╔════╝  ██║   ██║",
			"█████╔╝   ██║  ██║  █████╗    ██║   ██║",
			"██╔═██╗   ██║  ██║  ██╔══╝    ╚██╗ ██╔╝",
			"██║  ██╗  ██████╔╝  ███████╗   ╚████╔╝",
			"╚═╝  ╚═╝   ╚═════╝  ╚══════╝    ╚═══╝",
		}
	}
}

// --- matrix rain backdrop (welcome screen only) ---

// rainGlyphs mixes half-width katakana with digits and separators —
// the classic digital-rain alphabet (all narrow, so no wide-rune
// handling is needed).
var rainGlyphs = []rune("ｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄﾅﾆﾇﾈﾉﾊﾋﾌﾍﾎﾗﾘﾙﾚﾛﾜﾝ0123456789:･.=*+-<>|")

// rainCol is one falling stream: head row y, rows-per-step speed, a
// glyph per area row, and the fading trail length.
type rainCol struct {
	x, y, speed, trail int
	g                  []rune
}

// newRain seeds the welcome rain across w at a two-cell pitch (sparse:
// roughly two thirds of the columns active), heads spread over the
// rain area [top, bot].
func newRain(w, top, bot int) []rainCol {
	h := bot - top + 1
	if h < 4 || w < 8 {
		return nil
	}
	var cols []rainCol
	for x := 1; x < w-1; x += 2 {
		if rand.IntN(3) == 0 {
			continue
		}
		g := make([]rune, h)
		for i := range g {
			g[i] = rainGlyphs[rand.IntN(len(rainGlyphs))]
		}
		trail := 6 + rand.IntN(10)
		if trail > h {
			trail = h
		}
		cols = append(cols, rainCol{
			x: x, y: top + rand.IntN(h), speed: 1 + rand.IntN(2), trail: trail, g: g,
		})
	}
	return cols
}

// ensureRain (re)seeds the backdrop after size changes. Caller holds a.mu.
func (a *App) ensureRain(w, top, bot int) {
	if len(a.rain) == 0 || a.rainW != w || a.rainH != bot-top+1 {
		a.rain = newRain(w, top, bot)
		a.rainW, a.rainH = w, bot-top+1
	}
}

// stepRain advances every stream one step, flickers a glyph per column,
// and respawns columns whose tail has left the area. Caller holds a.mu.
func (a *App) stepRain(w, top, bot int) {
	a.ensureRain(w, top, bot)
	for i := range a.rain {
		c := &a.rain[i]
		c.y += c.speed
		c.g[rand.IntN(len(c.g))] = rainGlyphs[rand.IntN(len(rainGlyphs))]
		if c.y-c.trail > bot {
			c.y = top
			c.speed = 1 + rand.IntN(2)
			c.trail = 6 + rand.IntN(10)
		}
	}
}

// drawRain paints the streams behind the logo: bright head, fading
// tail. Caller holds a.mu.
func (a *App) drawRain(s tcell.Screen, top, bot int, head, mid, dim tcell.Style) {
	for _, c := range a.rain {
		for d := 0; d < c.trail && c.y-d >= top; d++ {
			row := c.y - d
			if row > bot {
				continue
			}
			st := mid
			if d == 0 {
				st = head
			} else if d > c.trail/2 {
				st = dim
			}
			s.SetContent(c.x, row, c.g[row-top], nil, st)
		}
	}
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

	// Matrix phosphor greens; deepened on the light theme so they stay
	headC, midC, dimC := theme.Hex("#00ff41"), theme.Hex("#00a828"), theme.Hex("#005c22")
	if !a.th.Dark {
		headC, midC, dimC = theme.Hex("#006e24"), theme.Hex("#00511b"), theme.Hex("#003812")
	}

	// Digital-rain backdrop between the top bar and the composer; the
	// logo and menu are drawn over it.
	top, bot := 1, h-5
	if bot > top+3 {
		a.ensureRain(w, top, bot)
		a.drawRain(s, top, bot, st(headC, false), st(midC, false), st(dimC, false))
	}

	// Logo + menu vertically centered in the content area (the composer
	// + shortcuts occupy the bottom 4 rows).
	contentTop, contentH := 2, h-8
	logo := logoArt(contentH)
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
