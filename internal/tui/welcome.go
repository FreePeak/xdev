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

// xdevLogo is the one welcome logo: the "xdev" pixel-grid wordmark — the
// terminal rendering of the same artwork in assets/brand/xdev-logo.svg.
// Each letter is a 5-wide matrix of pixels on a 20px grid — x and v are two
// arms meeting mid-height, d a bowl under a full-height stem, e a closed top
// eye, a centre crossbar and an open bottom aperture — and here every pixel
// becomes two cells wide (a cell is about twice as tall as it is wide, so the
// mark keeps its proportions) with one pixel of air between the letters, so
// every stroke is a whole number of cells.
//
// The d owns the ascender (art rows 0-1); x, e and v sit on the baseline band
// (rows 2-6). Every stem is on one absolute column across all seven rows and
// the ragged rows are trimmed at the right only — nothing is resampled or
// centred line by line, so no stroke wobbles. It renders identically at every
// terminal size; the only size-dependent choice is whether it fits at all
// (see logoArt).
var xdevLogo = []string{
	"                  ██",
	"                  ██",
	"██      ██    ██████    ██████    ██      ██",
	"  ██  ██    ██    ██  ██      ██  ██      ██",
	"    ██      ██    ██  ████████      ██  ██",
	"  ██  ██    ██    ██  ██            ██  ██",
	"██      ██    ██████    ██████        ██",
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
// when the terminal can't fit it: content shorter than the 7 art rows
// plus tagline, gap and menu, or narrower than the 44-cell art plus
// margins. One logo at every size — no variant swapping, so the
// artwork never changes shape between terminal sizes.
func logoArt(w, h int) []string {
	if h < 13 || w < logoWidth()+4 {
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
// 44-cell mark takes ~2.5s including its rest.
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

// promptHead collapses a block's text to the single line the top bar can
// measure: the first line only, its head taken (no terminal is a thousand
// cells wide, and the bar clips earlier still), tabs and control bytes
// sanitized so the width math matches what is painted, and whitespace runs
// squeezed. An all-blank line collapses to "", which the bar skips.
func promptHead(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	if len(line) > promptScanBytes {
		// The cut must not split a rune: ToValidUTF8 drops the incomplete
		// tail rather than leaving one the sanitizer would eat.
		line = strings.ToValidUTF8(line[:promptScanBytes], "")
	}
	return strings.Join(strings.Fields(sanitizeOutput(line)), " ")
}

// promptScanBytes bounds the text a prompt is collapsed from. It is a cost
// ceiling on a per-frame measurement, not a clip the user can see.
const promptScanBytes = 1 << 10

// topPrompts returns the two user prompts the top bar anchors on: the session's
// FIRST request and the NEWEST one. The pair reads as "what this session is
// about, and what it is answering now" once the transcript bands they belong to
// have scrolled away — a long session works on the tenth request while the
// first still names the task. They come back equal while only one prompt exists,
// and both empty when none has text. Both scans stop at the first prompt with
// something to say, so neither walks the transcript. Caller holds a.mu.
func (a *App) topPrompts() (first, last string) {
	for _, b := range a.blocks {
		if b.Kind != KindUser {
			continue
		}
		if first = promptHead(b.Text); first != "" {
			break
		}
	}
	for i := len(a.blocks) - 1; i >= 0; i-- {
		b := a.blocks[i]
		if b.Kind != KindUser {
			continue
		}
		if last = promptHead(b.Text); last != "" {
			break
		}
	}
	return first, last
}

// topPromptCells is the smallest share of the bar at which one prompt still
// says something — a word plus the ellipsis that admits it was cut. Below two
// shares there is nothing to gain from naming both prompts, so the bar keeps
// only the newest.
const topPromptCells = 8

// drawTopBar paints row 0 (grok top_bar.rs): the git branch left; with a
// transcript it also carries the session's FIRST user prompt and the NEWEST
// one, so the task the session is about and the request the screen is
// answering both stay visible while their transcript bands scroll away. The
// two read as one entry until a second prompt exists. Neither the working
// directory nor the model name is shown here — the status row and the
// composer's info divider carry them, and the prompts get the freed width.
// Caller holds a.mu.
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
	if !withPrompt {
		return
	}
	first, last := a.topPrompts()
	if first == "" {
		return
	}
	// Each entry pays for its own separator: the first rides the bar's lead-in
	// ("❯ " when no branch owns the left, the mid-dot otherwise), the second
	// always gets the mid-dot. The arithmetic keeps the last painted cell at
	// column w-2, so one cell of air stays at the right edge.
	lead, gap := " · ", " · "
	if left == "" {
		lead = "❯ "
	}
	prompts, seps := []string{first}, []string{lead}
	if last != first {
		prompts, seps = []string{first, last}, []string{lead, gap}
	}
	room := w - 2 - x
	for _, sep := range seps {
		room -= width(sep)
	}
	// Below one readable share the bar carries just the branch: a prompt cut
	// to a handful of cells names nothing and costs the transcript its width.
	if room < topPromptCells {
		return
	}
	if len(prompts) == 1 {
		prompts[0] = truncateCells(prompts[0], room, "…")
	} else if room < 2*topPromptCells {
		// Two prompts at half a share each say nothing, so the bar keeps the
		// NEWEST one alone at full width: the request on screen outranks the
		// session's opening line.
		prompts, seps = []string{last}, []string{lead}
		prompts[0] = truncateCells(prompts[0], room+width(gap), "…")
	} else {
		// Both fit the bar: the newest keeps at least half the room, so a long
		// opening prompt can never starve it; the first takes what is left.
		prompts[0] = truncateCells(prompts[0], max(room-width(prompts[1]), room/2), "…")
		prompts[1] = truncateCells(prompts[1], room-width(prompts[0]), "…")
	}
	for i, text := range prompts {
		drawText(s, x, 0, seps[i], dim)
		drawText(s, x+width(seps[i]), 0, text, promptSt)
		x += width(seps[i]) + width(text)
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
