// Package tts adds local speech synthesis: it shells out to the OS speech
// facility (macOS `say`, Linux `spd-say`/`espeak-ng`, Windows PowerShell
// SAPI), splitting long text into sentence-sized chunks so playback starts
// without waiting for the whole text and never overlaps.
//
// Nothing here is cgo: the backend is an external binary, so the single
// static binary stays CGO-free. A missing backend binary is an actionable
// error (or a clean skip in dry-run), never a crash.
package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// MaxTextBytes caps one request. A runaway paste would otherwise pin the
	// session for minutes behind a blocking audio device; the text is cut on
	// a rune boundary and the caller is told so.
	MaxTextBytes = 8 << 10

	// MaxChunkRunes bounds a single spoken chunk. Chunking is per-rune (not
	// per-byte) because backends count characters.
	MaxChunkRunes = 300

	// MinRate and MaxRate bound tts.rate (words per minute). Below MinRate a
	// long text takes hours; above MaxRate macOS `say` and espeak-ng clip
	// into noise.
	MinRate = 80
	MaxRate = 600

	// defaultWPM is the rate backends default to, used as the zero point when
	// mapping words-per-minute onto a backend's relative scale.
	defaultWPM = 175
)

// Settings is the `tts` configuration group: the voice and rate a session
// speaks with. A zero field means "whatever the backend defaults to".
type Settings struct {
	Voice string `yaml:"voice"`
	Rate  int    `yaml:"rate"`
}

// Request is one synthesis request: the configured defaults with per-call
// overrides already applied, plus the text.
type Request struct {
	Text   string
	Voice  string
	Rate   int
	DryRun bool
}

// Report is the outcome of one Speak call.
type Report struct {
	Backend   string   `json:"backend"`
	Chunks    []string `json:"chunks"`
	Bytes     int      `json:"bytes"` // bytes actually spoken, after truncation
	Total     int      `json:"total"` // bytes offered
	Truncated bool     `json:"truncated"`
	DryRun    bool     `json:"dryRun"`
}

// Summary is the one-line outcome, shared by the tool result and the CLI.
func (r Report) Summary() string {
	verb := "spoke"
	if r.DryRun {
		verb = "dry-run: planned"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d chunk(s) via %s (%d bytes)", verb, len(r.Chunks), r.Backend, r.Bytes)
	if r.Truncated {
		fmt.Fprintf(&b, "; text truncated to %d of %d bytes", r.Bytes, r.Total)
	}
	return b.String()
}

// Backend renders one chunk into a command line for a local synthesizer.
type Backend interface {
	// Name is the binary looked up on PATH.
	Name() string
	// Args is argv[1:] for one chunk.
	Args(text string, s Request) []string
	// Hint is appended to the missing-binary error to make it actionable.
	Hint() string
}

// ForGOOS picks the backend for goos ("" = the host). lookPath reports what
// is installed; nil means exec.LookPath. Windows and Linux have more than one
// candidate, so the choice is made here rather than at spawn time; when none
// is installed the preferred one is still returned so the failure names what
// to install.
func ForGOOS(goos string, lookPath func(string) (string, error)) (Backend, error) {
	if goos == "" {
		goos = runtime.GOOS
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	switch goos {
	case "darwin":
		return sayBackend{}, nil
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		if _, err := lookPath("spd-say"); err == nil {
			return spdSayBackend{}, nil
		}
		if _, err := lookPath("espeak-ng"); err == nil {
			return espeakBackend{}, nil
		}
		return spdSayBackend{}, nil
	case "windows":
		if _, err := lookPath("powershell"); err != nil {
			if _, err := lookPath("pwsh"); err == nil {
				return sapiBackend{bin: "pwsh"}, nil
			}
		}
		return sapiBackend{bin: "powershell"}, nil
	default:
		return nil, fmt.Errorf("tts: no speech backend for %s (supported: darwin, linux, windows)", goos)
	}
}

// Speaker plays requests through one backend.
type Speaker struct {
	backend Backend
	// Lookup resolves the backend binary on PATH (nil = exec.LookPath).
	Lookup func(string) (string, error)
}

// New returns a speaker for the host OS.
func New() (Speaker, error) {
	be, err := ForGOOS(runtime.GOOS, exec.LookPath)
	if err != nil {
		return Speaker{}, err
	}
	return Speaker{backend: be}, nil
}

// Plan renders one argv preview line per chunk. It spawns nothing, so it is
// both the `--dry-run` output and the no-audio test path.
func (sp Speaker) Plan(req Request) []string {
	text, _ := truncateBytes(req.Text, MaxTextBytes)
	chunks := Split(text, MaxChunkRunes)
	lines := make([]string, 0, len(chunks))
	for i, c := range chunks {
		argv := append([]string{sp.backend.Name()}, sp.backend.Args(c, req)...)
		lines = append(lines, fmt.Sprintf("[%d/%d] %s", i+1, len(chunks), strings.Join(argv, " ")))
	}
	return lines
}

// Speak chunks the text and plays the chunks one at a time: chunk n+1 starts
// only after chunk n's process exits, so two voices never overlap and ctx
// cancellation stops playback between chunks.
func (sp Speaker) Speak(ctx context.Context, req Request) (Report, error) {
	text, truncated := truncateBytes(req.Text, MaxTextBytes)
	chunks := Split(text, MaxChunkRunes)
	rep := Report{
		Backend:   sp.backend.Name(),
		Chunks:    chunks,
		Bytes:     len(text),
		Total:     len(req.Text),
		Truncated: truncated,
		DryRun:    req.DryRun,
	}
	if len(chunks) == 0 {
		return rep, errors.New("tts: nothing to say (empty text)")
	}
	if req.DryRun {
		return rep, nil
	}
	lookup := sp.Lookup
	if lookup == nil {
		lookup = exec.LookPath
	}
	bin, err := lookup(sp.backend.Name())
	if err != nil {
		return rep, fmt.Errorf("tts: %s not found on PATH — %s", sp.backend.Name(), sp.backend.Hint())
	}
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		cmd := exec.CommandContext(ctx, bin, sp.backend.Args(chunk, req)...)
		// stdout is the host terminal in every mode (TUI included): never let
		// a backend write into it.
		cmd.Stdout = io.Discard
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return rep, fmt.Errorf("tts: %s: %w%s", sp.backend.Name(), err, tail(errBuf.String()))
		}
	}
	return rep, nil
}

// tail renders bounded backend stderr for an error message.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return ": " + s
}

// RateOK reports whether a configured or requested rate is usable. Zero means
// "backend default".
func RateOK(rate int) bool {
	return rate == 0 || (rate >= MinRate && rate <= MaxRate)
}

// ValidateRate reports a usable rate with the message a user needs to fix it.
// The settings merge and the tool share this so the wording cannot drift.
func ValidateRate(rate int) error {
	if RateOK(rate) {
		return nil
	}
	return fmt.Errorf("tts: rate %d out of range (%d-%d words per minute, omit or use 0 for the backend default)", rate, MinRate, MaxRate)
}

// merge applies per-call overrides on top of the configured defaults.
func merge(s Settings, text, voice string, rate int, dryRun bool) Request {
	req := Request{Text: text, Voice: s.Voice, Rate: s.Rate, DryRun: dryRun}
	if voice != "" {
		req.Voice = voice
	}
	if rate != 0 {
		req.Rate = rate
	}
	return req
}

// relativeRate maps words-per-minute onto a backend's relative scale
// (-span..+span, 0 = defaultWPM).
//
// ponytail: coarse linear map, one formula for every relative-scale backend;
// upgrade path is a per-backend calibration table if users report that
// `tts.rate` does not sound the same on Windows/Linux as on macOS.
func relativeRate(wpm, span int) int {
	if wpm <= 0 {
		return 0
	}
	rel := (wpm - defaultWPM) * span / defaultWPM
	if rel > span {
		return span
	}
	if rel < -span {
		return -span
	}
	return rel
}

// ---- backends ----

// sayBackend is macOS: `say -v VOICE -r WPM -- TEXT`. `--` keeps text that
// starts with a dash from being read as a flag.
type sayBackend struct{}

func (sayBackend) Name() string { return "say" }
func (sayBackend) Hint() string {
	return "`say` ships with macOS; a missing one means the system install is damaged"
}
func (sayBackend) Args(text string, s Request) []string {
	args := make([]string, 0, 5)
	if s.Voice != "" {
		args = append(args, "-v", s.Voice)
	}
	if s.Rate > 0 {
		args = append(args, "-r", strconv.Itoa(s.Rate))
	}
	return append(args, "--", text)
}

// spdSayBackend is speech-dispatcher (Linux, also on BSD): `spd-say` takes a
// relative rate (-100..100) and `-w` waits for the utterance to finish, which
// is what keeps chunks sequential.
type spdSayBackend struct{}

func (spdSayBackend) Name() string { return "spd-say" }
func (spdSayBackend) Hint() string {
	return "install speech-dispatcher for spd-say, or espeak-ng and set tts.voice accordingly"
}
func (spdSayBackend) Args(text string, s Request) []string {
	args := []string{"-w"}
	if s.Voice != "" {
		args = append(args, "-y", s.Voice)
	}
	if s.Rate > 0 {
		args = append(args, "-r", strconv.Itoa(relativeRate(s.Rate, 100)))
	}
	return append(args, "--", text)
}

// espeakBackend is espeak-ng (Linux/BSD fallback): `-s` is words per minute,
// the same unit as tts.rate.
type espeakBackend struct{}

func (espeakBackend) Name() string { return "espeak-ng" }
func (espeakBackend) Hint() string {
	return "install espeak-ng, or speech-dispatcher to use spd-say instead"
}
func (espeakBackend) Args(text string, s Request) []string {
	args := make([]string, 0, 5)
	if s.Voice != "" {
		args = append(args, "-v", s.Voice)
	}
	if s.Rate > 0 {
		args = append(args, "-s", strconv.Itoa(s.Rate))
	}
	return append(args, "--", text)
}

// sapiBackend is Windows: PowerShell + System.Speech.Synthesis. The script is
// passed with -EncodedCommand (base64 UTF-16LE), which sidesteps cmd.exe and
// PowerShell quoting entirely, so arbitrary text — quotes, newlines, dashes —
// survives intact.
type sapiBackend struct{ bin string }

func (b sapiBackend) Name() string {
	if b.bin != "" {
		return b.bin
	}
	return "powershell"
}
func (b sapiBackend) Hint() string {
	return "install PowerShell (built in on Windows, `pwsh` on newer releases) to use the SAPI backend"
}
func (b sapiBackend) Args(text string, s Request) []string {
	var script strings.Builder
	script.WriteString("Add-Type -AssemblyName System.Speech; $s = New-Object System.Speech.Synthesis.SpeechSynthesizer")
	if s.Rate > 0 {
		fmt.Fprintf(&script, "; $s.Rate = %d", relativeRate(s.Rate, 10))
	}
	if s.Voice != "" {
		fmt.Fprintf(&script, "; $s.SelectVoice('%s')", psQuote(s.Voice))
	}
	fmt.Fprintf(&script, "; $s.Speak('%s')", psQuote(text))
	return []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", encodeCommand(script.String())}
}

// psQuote escapes for a single-quoted PowerShell literal.
func psQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }

// encodeCommand renders a PowerShell script as the base64 UTF-16LE payload
// -EncodedCommand expects.
func encodeCommand(script string) string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// ---- chunking ----

// truncateBytes cuts text to at most max bytes on a rune boundary.
func truncateBytes(text string, max int) (string, bool) {
	if len(text) <= max {
		return text, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

// Split cuts text into chunks of at most max runes, preferring sentence
// boundaries, so playback starts after the first sentence rather than after
// the whole text. Sentences longer than max are broken at the last clause or
// word boundary that fits (a single unbroken token is cut hard).
func Split(text string, max int) []string {
	if max <= 0 {
		max = MaxChunkRunes
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var chunks []string
	cur := 0 // runes in the chunk currently being packed
	for _, sentence := range splitSentences(text) {
		for _, piece := range hardSplit(sentence, max) {
			n := utf8.RuneCountInString(piece)
			switch {
			case cur == 0:
				chunks = append(chunks, piece)
				cur = n
			case cur+1+n > max:
				// Packing the next sentence would overflow: start a new chunk.
				chunks = append(chunks, piece)
				cur = n
			default:
				chunks[len(chunks)-1] += " " + piece
				cur += 1 + n
			}
		}
	}
	return chunks
}

// splitSentences cuts text after ., !, ?, or the CJK full stops. A `.!?` run
// (ellipsis, "?!") is one boundary, a `.` needs whitespace or end-of-text
// after it, and a leading abbreviation token ("e.g.", "Dr.", "3.") never
// ends a sentence.
func splitSentences(text string) []string {
	rs := []rune(text)
	var out []string
	start := 0
	for i := 0; i < len(rs); i++ {
		if !isTerminator(rs[i]) {
			continue
		}
		j := i
		for j+1 < len(rs) && isTerminator(rs[j+1]) {
			j++
		}
		boundary := true
		if rs[i] == '.' || rs[j] == '.' {
			boundary = !abbreviation(rs, i)
		}
		if j+1 < len(rs) && !unicode.IsSpace(rs[j+1]) {
			boundary = false
		}
		if boundary {
			if s := strings.TrimSpace(string(rs[start : j+1])); s != "" {
				out = append(out, s)
			}
			start = j + 1
		}
		i = j
	}
	if s := strings.TrimSpace(string(rs[start:])); s != "" {
		out = append(out, s)
	}
	return out
}

func isTerminator(c rune) bool {
	switch c {
	case '.', '!', '?', '…', '。', '！', '？':
		return true
	}
	return false
}

// abbreviations are tokens whose trailing dot is not a sentence end. Kept
// short on purpose: every entry is a word that cannot end a sentence.
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "sr": true,
	"jr": true, "st": true, "vs": true, "etc": true, "inc": true, "ltd": true,
	"e.g": true, "i.e": true, "cf": true, "al": true, "approx": true, "dept": true,
	"fig": true, "no": true, "vol": true,
}

// abbreviation reports whether the dot at rs[dot] closes a token that is not a
// sentence end: a single letter/digit (initials, "e.g", list numbers) or a
// known abbreviation.
func abbreviation(rs []rune, dot int) bool {
	end := dot
	start := end
	for start > 0 {
		c := rs[start-1]
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '\'' || c == '’' {
			start--
			continue
		}
		break
	}
	if start == end {
		return false
	}
	token := strings.ToLower(string(rs[start:end]))
	if utf8.RuneCountInString(token) == 1 {
		return true
	}
	return abbreviations[token]
}

// hardSplit bounds one sentence to max runes, preferring the last clause or
// word boundary inside the window.
func hardSplit(s string, max int) []string {
	rs := []rune(s)
	if len(rs) <= max {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var out []string
	for len(rs) > max {
		cut := max
		for k := cut; k > cut/2; k-- {
			if isBreakRune(rs[k-1]) {
				cut = k
				break
			}
		}
		if piece := strings.TrimSpace(string(rs[:cut])); piece != "" {
			out = append(out, piece)
		}
		rs = rs[cut:]
		for len(rs) > 0 && unicode.IsSpace(rs[0]) {
			rs = rs[1:]
		}
	}
	if piece := strings.TrimSpace(string(rs)); piece != "" {
		out = append(out, piece)
	}
	return out
}

func isBreakRune(c rune) bool {
	return unicode.IsSpace(c) || c == ',' || c == ';' || c == ':'
}
