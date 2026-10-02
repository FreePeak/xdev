package tool

import (
	"go/parser"
	"go/scanner"
	"go/token"
	"path/filepath"
	"strings"
)

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
