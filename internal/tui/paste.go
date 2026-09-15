package tui

// Bracketed paste and the clipboard image chord (omp parity: #235 image-paste
// chips, #233 paste collapse).
//
// Two jobs, one rule behind both: a paste is not a stream of keystrokes.
//
//  1. A terminal pasting raw bytes sends them as ordinary keys — with CR
//     between the lines, and CR is Enter — so a multi-line paste used to submit
//     one message per line. The screen asks for bracketed paste (EnablePaste)
//     and the state machine below routes the bytes into the composer as ONE
//     insertion, so one paste stays one message.
//  2. Pasted code must reach the model as code. A blob with the shape of source
//     goes into the buffer inside a fence, which is what keeps an indented
//     snippet from reading as prose — and it keeps the user looking at exactly
//     the text that will be sent, which a lossy "collapsed paste" would not.
//
// Only images need a side channel: a composer is a rune buffer, so bytes cannot
// live in it. An image is therefore held beside the buffer and shown by a chip
// carrying its own number ("[Image 256x128 #3]"). The number makes the chip
// unique, and the rule that keeps a message honest is that the buffer is the
// source of truth: a payload rides with the message only while its chip is
// still in the buffer. Delete the chip and the image is gone — nothing can be
// sent that the composer does not display.
//
// A terminal that ignores the paste mode keeps the old behaviour — its CRs
// send — and there is nothing better to do: guessing which keys came from a
// mouse would misfire on every ordinary fast typist.

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"  // a pasted GIF reports its size
	_ "image/jpeg" // a pasted JPEG reports its size
	_ "image/png"  // a pasted PNG reports its size
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gdamore/tcell/v2"
)

// terminal that ends a paste on a timer (xterm holds a trailing ESC back in
// case it opens a sequence) sends its end marker this late; a terminal that
// never sends one — some drop it when its ESC is read as Alt+<something> — is
// flushed on this timer instead of freezing the input.
const pasteGrace = 150 * time.Millisecond

// maxPasteImageBytes caps one pasted image. It is a memory ceiling, not a
// feature: a screen region is far under it, and a bigger file's base64 body
// would be rejected by the API on every later turn of the session anyway.
const maxPasteImageBytes = 12 << 20

// maxPendingImages is how many payloads a composer may hold. The buffer, not
// this number, decides what gets sent, so dropping the oldest only costs a
// chip its bytes — and 64 screenshots is hundreds of megabytes of RAM.
const maxPendingImages = 64

// imageChipPrefix is what a chip's text starts with. The chip is the composer's
// only visible stand for an image, so it must read as a label; it is stripped
// from the text when the payload goes out (App.expandPastes).
const imageChipPrefix = "[Image "

// PasteImage is one image waiting in the composer: the chip the user sees and
// the bytes the message carries.
type PasteImage struct {
	ID        int64 // printed in the chip; unique for the session's lifetime
	MediaType string
	Data      []byte // raw bytes; base64 happens at the provider
	Width     int    // 0 when the bytes decode to nothing we recognise
	Height    int
	Path      string // set when the paste was a file path, "" for clipboard bytes
}

// chip is the composer text standing for this image. Both the insert and the
// send-time scan call it, so a chip that can be found is exactly a chip whose
// payload goes out.
func (p PasteImage) chip() string {
	if p.Width > 0 && p.Height > 0 {
		return fmt.Sprintf("%s%dx%d #%d]", imageChipPrefix, p.Width, p.Height, p.ID)
	}
	return fmt.Sprintf("%s#%d]", imageChipPrefix, p.ID)
}

// pendingImages are the composer's image payloads. The buffer is the source of
// truth about which of them exist; this is only the store for what their bytes
// are. UI-thread-owned, exactly like the editor.
type pendingImages struct {
	list []PasteImage
	last int64
}

// add stores one image, returning it so the caller can insert its chip.
func (q *pendingImages) add(p PasteImage) PasteImage {
	q.last++
	p.ID = q.last
	q.list = append(q.list, p)
	if extra := len(q.list) - maxPendingImages; extra > 0 {
		q.list = append(q.list[:0], q.list[extra:]...)
	}
	return p
}

// inBuf returns the payloads whose chip still shows in buf, ordered by where the
// chip sits in it. Position, not insertion order, is the prompt's order: an
// image pasted between two sentences reaches the model between those sentences,
// and a chip the user moved to the front belongs at the front.
func (q *pendingImages) inBuf(buf string) []PasteImage {
	type hit struct {
		p PasteImage
		i int
	}
	var hits []hit
	for _, p := range q.list {
		if i := strings.Index(buf, p.chip()); i >= 0 {
			hits = append(hits, hit{p, i})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].i < hits[j].i })
	out := make([]PasteImage, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.p)
	}
	return out
}

// keep re-primes payloads a send reset dropped from the store, so a draft
// handed back to the composer still names live images (returnDraft).
func (q *pendingImages) keep(imgs []PasteImage) {
	for _, p := range imgs {
		found := false
		for _, have := range q.list {
			if have.ID == p.ID {
				found = true
				break
			}
		}
		if !found {
			q.list = append(q.list, p)
		}
	}
}

// drop forgets images whose chip is gone from buf, so a payload cannot outlive
func (q *pendingImages) drop(buf string) {
	if len(q.list) == 0 {
		return
	}
	kept := q.list[:0]
	for _, p := range q.list {
		if strings.Contains(buf, p.chip()) {
			kept = append(kept, p)
		}
	}
	q.list = kept
}

// ImagesFor returns the images a composer draft carries, in the order their
// chips sit in it. Caller: the submit path, on the UI thread — the list is
// editor-owned, so it needs no lock, and the draft handed in is the buffer as
// the user saw it.
func (a *App) ImagesFor(buf string) []PasteImage { return a.images.inBuf(buf) }

// expandPastes rewrites a draft so the model receives what each chip stands
// for. An attached image's chip leaves the text — the image part is its
// rendering, and a request body must not repeat it. A chip whose payload is
// gone (refused for size, or a transcript line replayed into the composer)
// stays as typed, so a message always says what the composer showed rather than
// silently dropping a reference.
func (a *App) expandPastes(buf string) string {
	for _, p := range a.images.list {
		if i := strings.Index(buf, p.chip()); i >= 0 {
			buf = strings.ReplaceAll(buf, p.chip(), "")
		}
	}
	return strings.TrimSpace(buf)
}

// dropStalePastes releases the payloads the buffer no longer shows. Caller: the
// UI thread after every edit that can remove text (App.handleKey's editor
// path); the composer is small and this is a substring scan over ≤64 chips.
func (a *App) dropStalePastes() { a.images.drop(a.ed.Text()) }

// --------------------------------------------------------------------------
// The paste state machine (UI thread: the editor is UI-thread-owned)
// --------------------------------------------------------------------------

// pasteState accumulates one bracketed paste. It is a value on the App so the
// event loop reads as a switch — the whole terminal input path depends on it
// being right, and above all a paste that never closes must not eat the next
// keystroke.
type pasteState struct {
	on    bool // inside a 200…201 window: keys are payload, not commands
	buf   strings.Builder
	since time.Time // last byte seen, for the stuck-window watchdog
}

// feedPaste handles one event while a bracketed-paste window may be open. It
// reports whether the App is done with the event, and hands back the finished
// payload when the window closes — including when it closes because a control
// key arrived, so a terminal that lost its end marker can never swallow Ctrl-C.
//
// CR and LF inside a window are both kept: terminals paste CRLF, LF or a lone
// CR depending on what the application asked for, and normalizePasted reduces
// whichever it sent to one shape.
func (p *pasteState) feedPaste(ev tcell.Event) (consumed bool, text string) {
	switch v := ev.(type) {
	case *tcell.EventPaste:
		if v.Start() {
			p.on, p.buf, p.since = true, strings.Builder{}, time.Now()
			return true, ""
		}
		text = p.buf.String()
		p.on = false
		p.buf.Reset()
		return true, text
	case *tcell.EventKey:
		if !p.on {
			return false, ""
		}
		p.since = time.Now()
		switch v.Key() {
		case tcell.KeyRune:
			p.buf.WriteRune(v.Rune())
		case tcell.KeyEnter: // a pasted CR
			p.buf.WriteRune('\n')
		case tcell.KeyCtrlJ: // a pasted LF (tcell reports 0x0A as Ctrl+J)
			p.buf.WriteRune('\n')
		case tcell.KeyTab: // a pasted TAB
			p.buf.WriteRune('\t')
		default:
			// ESC (which can be the lost end marker itself) or any other
			// control key: the user meant the key, and the paste is whatever
			// arrived until now. Hand back the text AND leave the event
			// unconsumed, so Ctrl-C still quits and Esc still cancels.
			text := p.buf.String()
			p.on = false
			p.buf.Reset()
			return false, text
		}
		return true, ""
	}
	return false, ""
}

// handlePaste puts one completed paste into the composer. Caller: UI thread.
func (a *App) handlePaste(s string) {
	s = normalizePasted(s)
	if s == "" {
		a.poke()
		return
	}
	// A one-line paste naming an image file is attached for real: macOS hands a
	// dragged file to the terminal as its path, and a path is worthless to a
	// model that cannot open it.
	if p, ok := imageFromPath(s); ok {
		a.insertImage(p)
		return
	}
	if strings.Contains(s, "\n") && looksLikeCode(s) {
		s = "```\n" + strings.Trim(s, "\n") + "\n```\n"
	}
	a.ed.InsertText(s)
	a.poke()
}

// insertImage stores a payload and puts its chip at the cursor, followed by a
// space so the next word typed does not touch the chip's bracket.
func (a *App) insertImage(p PasteImage) {
	p = a.images.add(p)
	a.ed.InsertText(p.chip() + " ")
	a.poke()
}

// pasteClipboard attaches the clipboard image (the paste-image chord). The
// bytes live in the system clipboard, which no terminal forwards to the app, so
// this is the only way a screenshot reaches the model.
func (a *App) pasteClipboard() {
	if a.clipImage == nil {
		a.clipImage = clipboardImage
	}
	if a.vision != nil && !a.vision() {
		a.setNotice("the current model takes no image input — switch with /model, or paste the file path instead")
		a.poke()
		return
	}
	data, mime, err := a.clipImage()
	if err != nil {
		// A chord that fails must say so, or it looks dead.
		a.setNotice(err.Error())
		a.poke()
		return
	}
	if len(data) > maxPasteImageBytes {
		a.setNotice(fmt.Sprintf("clipboard image is %.1f MB — over the %d MB paste limit",
			float64(len(data))/(1<<20), maxPasteImageBytes>>20))
		a.poke()
		return
	}
	p := PasteImage{MediaType: mime, Data: data}
	p.Width, p.Height = imageBounds(data)
	a.insertImage(p)
}

// setNotice shows one line on the composer divider: the channel the selection
// copy confirmation already uses, so a failed chord rides the same row instead
// of adding chrome. Caller: UI thread.
func (a *App) setNotice(s string) {
	a.selNotice = s
	a.selNoticeUntil = time.Now().Add(selGrace)
}

// flushStuckPaste closes a paste window whose end marker never arrived, so the
// terminal keeps working. Caller: the App's tick loop, on the UI thread and
// outside a.mu (the editor is UI-thread-owned).
func (a *App) flushStuckPaste() bool {
	if !a.paste.on || time.Since(a.paste.since) < pasteGrace {
		return false
	}
	s := a.paste.buf.String()
	a.paste.on = false
	a.paste.buf.Reset()
	a.handlePaste(s)
	return true
}

// --------------------------------------------------------------------------
// Reading the pasted bytes
// --------------------------------------------------------------------------

// normalizePasted maps whatever line-ending convention the terminal or the
// clipboard used onto the buffer's LF, and makes the text safe to put in a
// buffer at all:
//
//   - CRLF and a lone CR become LF;
//   - tabs expand to three spaces, the width omp's paste controller uses: the
//     screen has no tab stops, so one pasted tab shifts the rest of its line;
//   - control characters are dropped. A buffer is measured and drawn one rune
//     at a time and a raw control byte paints as garbage; the terminal's own
//     erase characters go too, because inside a paste they are data, and data
//     that deletes what the user typed is worse than noise.
//
// Text that needs none of this keeps its bytes exactly: a code block is mostly
// indentation, and mangling it here is the one mistake a user would notice.
func normalizePasted(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if !strings.ContainsFunc(s, pasteScars) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("   ")
		case unicode.IsControl(r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pasteScars reports a rune the sanitizer would change, so ordinary printable
// text skips the rebuild entirely.
func pasteScars(r rune) bool { return r == '\t' || unicode.IsControl(r) }

// looksLikeCode reports whether a pasted blob carries the shape of source code
// and should therefore be fenced. The signals are the ones a code block has —
// indentation, fences, terminators, diff headers, keyword openers — and they
// are deliberately blunt: prose must never be mistaken for code (that would
// wrap a paragraph in backticks), while a stack trace that misses the fence is
// only a missed nicety.
func looksLikeCode(s string) bool {
	seen := 0
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if seen++; seen > 40 {
			break // enough evidence; a megabyte of text is not worth scanning
		}
		if strings.HasPrefix(ln, "\t") || strings.HasPrefix(ln, "  ") {
			return true
		}
		for _, p := range codePrefixes {
			if strings.HasPrefix(t, p) {
				return true
			}
		}
		for _, suf := range codeSuffixes {
			if strings.HasSuffix(t, suf) {
				return true
			}
		}
	}
	return false
}

// codePrefixes and codeSuffixes are the line shapes of a program: language
// keywords, shell/diff/comment openers, and the terminators that close a
// statement. They carry their spacing, so a sentence that merely contains the
// word does not match.
var (
	codePrefixes = []string{
		"```", "~~~", "//", "/*", "<", "@@", "diff ", "--- ", "+++ ", "|",
		"func ", "def ", "class ", "import ", "package ", "return ", "const ",
		"var ", "type ", "let ", "fn ", "pub ", "if ", "else ", "for ",
		"while ", "switch ", "case ", "try ", "catch ", "except ", "export ",
		"SELECT ", "INSERT ", "UPDATE ", "DELETE ", "$ ", "> ", "#include",
	}
	codeSuffixes = []string{";", "{", "}", ")", ",", " => ", "::", "\\"}
)

// imageFileExts are the image types a pasted path may name. Anything else stays
// text: attaching a source file to a prompt is not what a paste is for.
var imageFileExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
}

// imageFromPath loads a one-line paste that names an existing image file.
// Unreadable paths (another machine's file, a deleted one, a format we cannot
// name) are not an error: the text is pasted as typed, which is what the user
// can still act on.
func imageFromPath(s string) (PasteImage, bool) {
	if strings.Contains(s, "\n") || len(s) > 1024 {
		return PasteImage{}, false
	}
	s = strings.TrimSpace(s)
	for _, cand := range pathCandidates(s) {
		if !imageFileExts[strings.ToLower(filepath.Ext(cand))] {
			continue
		}
		fi, err := os.Stat(cand)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		p, err := readImageFile(cand)
		if err != nil {
			continue
		}
		return p, true
	}
	return PasteImage{}, false
}

// pathCandidates unwraps the forms a terminal may deliver a path in: bare,
// quoted, and shell-escaped.
func pathCandidates(s string) []string {
	out := make([]string, 0, 3)
	add := func(c string) {
		if c != "" {
			out = append(out, filepath.Clean(c))
		}
	}
	add(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		add(s[1 : len(s)-1])
	}
	if strings.Contains(s, "\\ ") {
		add(strings.ReplaceAll(s, "\\ ", " "))
	}
	return out
}

// readImageFile loads a pasted path as an image payload. The media type comes
// from the bytes, not the extension: a renamed file must not claim to be a PNG.
func readImageFile(path string) (PasteImage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PasteImage{}, err
	}
	mime, ok := imageMediaType(b)
	if !ok {
		return PasteImage{}, fmt.Errorf("%s: not an image format I can name", filepath.Base(path))
	}
	if len(b) > maxPasteImageBytes {
		return PasteImage{}, fmt.Errorf("%s: %.1f MB, over the %d MB paste limit",
			filepath.Base(path), float64(len(b))/(1<<20), maxPasteImageBytes>>20)
	}
	p := PasteImage{MediaType: mime, Data: b, Path: path}
	p.Width, p.Height = imageBounds(b)
	return p, nil
}

// imageMediaType names an image from its magic bytes. The stdlib's sniffer
// already answers with exactly the string the wire wants for these formats, and
// it does not know webp/bmp — those two are checked by hand.
func imageMediaType(b []byte) (string, bool) {
	switch mime := http.DetectContentType(b); mime {
	case "image/png", "image/jpeg", "image/gif":
		return mime, true
	}
	switch {
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp", true
	case len(b) > 54 && string(b[:2]) == "BM":
		return "image/bmp", true
	}
	return "", false
}

// imageBounds is a pasted image's WxH for its chip; 0x0 when the bytes are not
// a format the stdlib can read.
func imageBounds(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}
