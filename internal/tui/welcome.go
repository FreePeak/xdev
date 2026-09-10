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

// logoArt returns the braille xdev logo for the given terminal height
// (grok welcome/logo.rs tiers: hidden below 10 usable rows, compact
// below 22, full above).
func logoArt(height int) []string {
	// "Xdev" in 5-row block letters, two-space gaps.
	full := []string{
		"█   █   ████   ██  █   █",
		" █ █     █    █  █  █   █",
		"  █    █   █  ████   █ █ ",
		" █ █   █   █  █     █ █ ",
		"█   █   ████   ██    █  ",
	}
	if height < 10 {
		return nil
	}
	if height < 22 {
		return full[2:]
	}
	return full
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

	// Logo + menu vertically centered in the content area (the composer
	// + shortcuts occupy the bottom 4 rows).
	contentTop, contentH := 2, h-8
	logo := logoArt(contentH)
	menu := welcomeMenuItems(len(a.blocks) > 0)
	total := len(logo) + 1 + len(menu)
	y := contentTop + max(0, (contentH-total)/2)

	for _, ln := range logo {
		drawText(s, max(2, (w-width(ln))/2), y, ln, st(a.th.Get(theme.AccentUser), false))
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
