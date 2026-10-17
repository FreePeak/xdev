package tool

import (
	"context"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The post-write verification ladder.
//
// Tier 0 (verifyTier0 below): a standard-library parse, so the cheapest
// possible answer to "did the bytes you just wrote stop parsing" costs no
// subprocess, no language server, and microseconds.
//
// Tier 1 (DiagnosticsProvider, installed by the lsp package): a language
// server that is ALREADY running. This is #263's rich end -- LSP
// publishDiagnostics carries type errors a parser structurally cannot see --
// and it is deliberately warm-only: the write path must never be the thing
// that pays a 30-second language-server handshake.
//
// The interface lives here rather than in internal/lsp because internal/lsp
// imports this package, and the reverse edge would be an import cycle. That
// constraint is also why the provider is a one-method seam over a flattened
// Problem type: any backend can fill it without the tool package growing a
// dependency.

// Problem is one diagnostic flattened for the model-facing note. LSP's
// severity 1..4 is the provider's convention, not the harness's, so only the
// error/warning distinction crosses the seam.
type Problem struct {
	Severity string
	// Location is an already-formatted position ("a.go:12:3"); the provider
	// owns the path shape because only it knows the display rules.
	Location string
	Message  string
}

// DiagnosticsProvider reports what a language server says about a file the
// harness has just written. ok=false means "nothing to report, or nothing
// running" -- never an error: a provider that cannot answer must not make a
// successful write look broken.
type DiagnosticsProvider interface {
	DiagnosticsFor(ctx context.Context, path string, wait time.Duration) ([]Problem, bool)
}

// SetDiagnosticsProvider installs the tier-1 provider for this registry's
// edit/write tools. Passing nil restores the pre-#263 behavior.
func (r *Registry) SetDiagnosticsProvider(p DiagnosticsProvider) { r.diagProvider = p }

// verifyWait bounds tier 1: long enough for a warm server to re-analyze one
// file, short enough that the model is not left holding its turn open while a
// language server thinks.
// verifyWait bounds tier 1.
//
// Measured against a real gopls on 2026-10-03: a warm server answers in
// ~30-40ms for a brand-new file, and a clean file publishes an empty set just
// as fast, so this ceiling is a backstop rather than a budget that is normally
// spent. It matches the wait the pre-existing `lsp diagnostics` call uses, so
// the write path is never stricter than the tool the model could have called
// itself.
const verifyWait = 2 * time.Second

// verifyMaxProblems caps how many diagnostics reach the model. A broken file
// can produce hundreds; the model needs to know it is broken and where to look
// first, not a compiler log it pays for on every following read.
const verifyMaxProblems = 5

// VerifyAfterWrite returns one advisory line describing what is wrong with the
// file a write or edit just produced, or "" when nothing is wrong.
//
// It never fails or blocks the write -- the bytes are on disk before this
// runs, and the point is telling the model, not refusing -- and it never
// invents a problem.
func (r *Registry) VerifyAfterWrite(ctx context.Context, path string) string {
	if note := verifyTier0(path); note != "" {
		// A file that does not parse cannot produce meaningful type
		// diagnostics, and the parse error is the more actionable line.
		return note
	}
	if note := verifyExternal(ctx, path); note != "" {
		// A compiler-level failure (format, type, syntax the stdlib parser
		// cannot see) is more actionable than a warm language server's note.
		return note
	}
	if r == nil || r.diagProvider == nil {
		return ""
	}
	problems, ok := r.diagProvider.DiagnosticsFor(ctx, path, verifyWait)
	if !ok || len(problems) == 0 {
		return ""
	}
	return renderProblems(problems)
}

// renderProblems renders the leading problems as one actionable line, errors
// first so a fatal one is never truncated away behind five warnings.
func renderProblems(problems []Problem) string {
	ordered := make([]Problem, 0, len(problems))
	for _, p := range problems {
		if strings.EqualFold(strings.TrimSpace(p.Severity), "error") {
			ordered = append(ordered, p)
		}
	}
	for _, p := range problems {
		if !strings.EqualFold(strings.TrimSpace(p.Severity), "error") {
			ordered = append(ordered, p)
		}
	}
	more := len(ordered) - verifyMaxProblems
	if more > 0 {
		ordered = ordered[:verifyMaxProblems]
	}
	var b strings.Builder
	b.WriteString("language server reports ")
	for i, p := range ordered {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(p.Message)
		if p.Location != "" {
			b.WriteString(" (")
			b.WriteString(p.Location)
			b.WriteString(")")
		}
	}
	if more > 0 {
		b.WriteString(" (+")
		b.WriteString(itoa(more))
		b.WriteString(" more)")
	}
	b.WriteString(" -- fix these before the next build")
	return capVerifyMessage(b.String())
}

// verifyExternal runs a language-specific external check when the tool is
// installed. It is best-effort: a missing tool, a timeout, or an unreadable
// file all yield "", never an error. The point is catching what the stdlib
// parser cannot (formatting, type errors) without making the write path depend
// on a toolchain being present.
func verifyExternal(ctx context.Context, path string) string {
	if ctx.Err() != nil {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 || st.Size() > verifyMaxBytes {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return runExternal(ctx, "gofmt", []string{"-l", path}, "gofmt")
	case ".ts", ".tsx":
		return runExternal(ctx, "tsc", []string{"--noEmit", "--skipLibCheck", path}, "tsc")
	case ".py":
		for _, py := range []string{"python3", "python"} {
			if _, err := exec.LookPath(py); err == nil {
				return runExternal(ctx, py, []string{"-m", "py_compile", path}, "py_compile")
			}
		}
		return ""
	}
	return ""
}

// verifyExternalTimeout bounds the subprocess so a slow toolchain cannot hold
// the model's turn open.
const verifyExternalTimeout = 10 * time.Second

// runExternal executes one external check, returning an advisory note on
// failure and "" otherwise. A missing binary is silent.
func runExternal(ctx context.Context, tool string, args []string, label string) string {
	if ctx.Err() != nil {
		return ""
	}
	bin, err := exec.LookPath(tool)
	if err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, verifyExternalTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if cctx.Err() != nil {
		// Cancellation or timeout: never report; the write already succeeded.
		return ""
	}
	trimmed := strings.TrimSpace(string(out))
	if err == nil && trimmed == "" {
		return ""
	}
	msg := trimmed
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if msg == "" {
		return ""
	}
	return label + ": " + capVerifyMessage(msg)
}

// verifyTier0 parses the file on disk with the standard library. It reports ""
// for a non-Go file, an unreadable file, or one too large to be the product of
// an edit.
func verifyTier0(path string) string {
	if !strings.EqualFold(filepath.Ext(path), ".go") {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 || st.Size() > verifyMaxBytes {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return verifyGoSyntax(path, data)
}

// Post-write syntax verification for Go files.
//
// The edit/write path already guards *staleness* (internal/tool/edit.go
// checkFreshness re-anchors ops by exact text when the file moved) and
// *shifting* (the hashline grammar's line ranges). Neither guards the case
// the model gets wrong most often with a line-range patch: it removes a
// brace, or leaves an `else` behind, and the file is now syntactically
// broken. Today the model learns that one turn later — when its next `bash
// go build` fails, or never.
//
// go/parser is the standard library: no dependency, no CGO, no subprocess,
// no language server to start, and microseconds on a normal file. That is
// deliberately the *cheap* end of the diagnostics ladder — #263 owns the
// rich end (LSP `publishDiagnostics` with type errors). This catches what a
// parser can catch, which for edit-tool damage is most of it.
//
// What it is NOT: a type checker. `go/parser` sees syntax, not types, so
// `x := "s"; x.Foo()` parses clean. Saying otherwise would be a lie in the
// tool result; the model-facing wording therefore says "syntax".
//
// The verifier never fails or blocks a write: the bytes are already on disk
// by the time it runs, and the point is telling the model, not refusing.

// verifyMaxBytes bounds the file handed to the parser. A file this large is
// not the shape an edit-tool patch produces, and parsing it would spend the
// tool's latency budget on a case that cannot be its own error.
const verifyMaxBytes = 2 << 20 // 2 MiB

// verifyMaxMessage caps the rendered first error: one line, no stack, no
// column-by-column dump.
const verifyMaxMessage = 300

// verifyGoSyntax parses data as a Go file, returning a one-line
// model-facing note when it does not parse and "" when it does (or when the
// path is not Go, or is too large to be worth parsing). It is advisory
// text appended to a successful write/edit result.
func verifyGoSyntax(path string, data []byte) string {
	if !strings.EqualFold(filepath.Ext(path), ".go") {
		return ""
	}
	if len(data) == 0 || len(data) > verifyMaxBytes {
		return ""
	}
	fset := token.NewFileSet()
	var el scanner.ErrorList
	_, err := parser.ParseFile(fset, path, data, parser.SkipObjectResolution)
	if err == nil {
		return ""
	}
	if list, ok := err.(scanner.ErrorList); ok {
		el = list
	} else {
		return "syntax check: " + capVerifyMessage(err.Error())
	}
	if len(el) == 0 {
		return ""
	}
	first := el[0]
	msg := first.Msg
	if pos := first.Pos; pos.IsValid() {
		msg = pos.String() + ": " + msg
	}
	more := ""
	if len(el) > 1 {
		more = " (+" + itoa(len(el)-1) + " more)"
	}
	return "syntax: " + capVerifyMessage(msg) + more + " — the file no longer parses; fix it before the next build"
}

// capVerifyMessage clips to verifyMaxMessage on a rune boundary.
func capVerifyMessage(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= verifyMaxMessage {
		return s
	}
	cut := verifyMaxMessage
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// isRuneStart reports whether b begins a UTF-8 sequence (no utf8 import
// needed for one byte test, and this keeps the file's imports honest).
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// itoa renders a small non-negative count without importing strconv for one
// call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
