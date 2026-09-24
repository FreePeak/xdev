package tui

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// linkHit is the screen rectangle occupied by a rendered link run. It is
// rebuilt by paint, so clicks use the same geometry as the visible text after
// scrolling, resizing, or changing the dock width.
type linkHit struct {
	x0, x1 int
	y      int
	target string
}

// smenuCovers reports whether the non-modal slash/@ suggestion panel paints
// over (x, y). It is UI-thread state; callers hold a.mu.
func (a *App) smenuCovers(x, y int) bool {
	if a.smenu == nil || !a.smenu.active() {
		return false
	}
	rows := a.smenu.rows()
	composerTop := a.height - 1 - a.composerRows()
	if len(rows) == 0 || composerTop < len(rows)+1 {
		return false
	}
	top := composerTop - len(rows) - 2
	return x >= 2 && x <= a.width-3 && y >= top && y <= composerTop
}

func (a *App) linkAt(x, y int) string {
	// The transcript is the bottom layer. A modal overlay owns the mouse and
	// paints over every link beneath it, so none of those hidden targets may
	// open. The slash menu is not modal: mask only its painted panel.
	if a.smenuCovers(x, y) || a.diffOv != nil || len(a.pickers) > 0 || a.HubRosterOpen() ||
		a.TrajectoryOpen() || (a.ask != nil && !a.ask.dead) || a.SettingsOverlayOpen() ||
		a.tpick != nil || a.spick.active() {
		return ""
	}
	for _, hit := range a.linkHits {
		if hit.y == y && x >= hit.x0 && x <= hit.x1 {
			return hit.target
		}
	}
	return ""
}

func (a *App) openLink(target string) error {
	if !isWebURL(target) {
		return fmt.Errorf("unsupported URL")
	}
	if a.linkOpen != nil {
		return a.linkOpen(target)
	}
	return openExternalURL(target)
}

func isWebURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" &&
		(strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https"))
}

// webURLStart returns the first HTTP(S) scheme at a token boundary. Matching is
// case-insensitive, but a scheme embedded in a word (abchttps://...) is not a
// URL. It scans the original string without copying, so long streamed lines
// keep the renderer's linear cost.
func webURLStart(s string) int {
	for i := 0; i+7 <= len(s); i++ {
		if !urlBoundary(s, i) {
			continue
		}
		if i+8 <= len(s) && asciiEqualFold(s[i:i+8], "https://") {
			return i
		}
		if asciiEqualFold(s[i:i+7], "http://") {
			return i
		}
	}
	return -1
}

func urlBoundary(s string, i int) bool {
	if i == 0 {
		return true
	}
	// Walk back over combining marks as part of the preceding grapheme, so
	// decomposed "éhttps://…" is not mistaken for a punctuation boundary.
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		if !unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Me, r) {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !isURLWordByte(s[i-1])
		}
		i -= size
	}
	return true
}

func asciiEqualFold(s, lower string) bool {
	for i := range lower {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

func isURLWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		strings.ContainsRune("._~:/?#@!$&*+,;=%", rune(c))
}

// webURLEnd finds the end of a bare URL. All Unicode opening punctuation is
// tracked as a stack so paired IRI delimiters stay in the URL; an unmatched
// closing bracket or sentence punctuation ends it. This also handles ASCII and
// fullwidth pairs without a Unicode-property table.
func webURLEnd(s string) int {
	var closers []rune
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case unicode.IsSpace(r) || strings.ContainsRune("<>\"'", r):
			return trimURLPunctuation(s[:i], len(closers))
		case isURLOpeningBracket(r):
			closers = append(closers, urlClosingBracket(r))
		case isURLClosingBracket(r):
			if len(closers) == 0 {
				return trimURLPunctuation(s[:i], 0)
			}
			closers = closers[:len(closers)-1]
		case isUnicodeURLPunctuation(r):
			return trimURLPunctuation(s[:i], len(closers))
		}
		i += size
	}
	return trimURLPunctuation(s, len(closers))
}

func isURLOpeningBracket(r rune) bool {
	return r == '(' || r == '[' || unicode.Is(unicode.Ps, r)
}

func isURLClosingBracket(r rune) bool {
	return r == ')' || r == ']' || unicode.Is(unicode.Pe, r)
}

func urlClosingBracket(r rune) rune {
	switch r {
	case '(':
		return ')'
	case '[':
		return ']'
	case '（':
		return '）'
	case '［':
		return '］'
	case '｛':
		return '｝'
	case '〈':
		return '〉'
	case '《':
		return '》'
	case '「':
		return '」'
	case '『':
		return '』'
	default:
		return r + 1
	}
}

func isUnicodeURLPunctuation(r rune) bool {
	return r >= utf8.RuneSelf && unicode.Is(unicode.Po, r)
}

func trimURLPunctuation(s string, openDepth int) int {
	end := len(s)
	for end > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:end])
		if isURLClosingBracket(r) {
			if openDepth == 0 {
				return end
			}
		} else if r == '.' || r == ',' || r == ';' || r == ':' || r == '!' || r == '?' ||
			r == '*' || r == '_' || r == '`' ||
			(r >= utf8.RuneSelf && unicode.Is(unicode.Po, r)) {
			end -= size
			continue
		}
		return end
	}
	return end
}

// startAndReap starts cmd and waits for it on a background goroutine. The
// buffered channel lets the reaper finish even if the caller abandons the
// result, so a click never blocks the UI and the child never becomes a zombie.
func startAndReap(cmd *exec.Cmd) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, nil
}

// openExternalURL returns launch errors only. A platform opener can report a
// later failure after it has successfully started, but the click has already
// been delivered; startAndReap still reaps that child.
func openExternalURL(target string) error {
	if !isWebURL(target) {
		return fmt.Errorf("unsupported URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "linux":
		if !has("xdg-open") {
			return fmt.Errorf("xdg-open is not available")
		}
		cmd = exec.Command("xdg-open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		return fmt.Errorf("opening links is unsupported on %s", runtime.GOOS)
	}
	_, err := startAndReap(cmd)
	return err
}
