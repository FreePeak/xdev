package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The verifier's whole job is to turn "the file no longer parses" into
// something the model reads in the SAME tool result. These tests pin both
// halves: the parse verdict itself, and that the verdict reaches a real
// edit/write Result.

const verifyBroken = "package p\n\nfunc f() {\n\treturn\n"
const verifyClean = "package p\n\nfunc f() {\n\treturn\n}\n"

func TestVerifyGoSyntaxReportsMalformedFile(t *testing.T) {
	note := verifyGoSyntax("a.go", []byte(verifyBroken))
	if note == "" {
		t.Fatal("a file missing its closing brace must produce a note")
	}
	if !strings.HasPrefix(note, "syntax: ") {
		t.Fatalf("note must say what it is, got %q", note)
	}
	if !strings.Contains(note, "a.go:") {
		t.Fatalf("note must name the position, got %q", note)
	}
	if !strings.Contains(note, "no longer parses") {
		t.Fatalf("note must say what to do, got %q", note)
	}
}

func TestVerifyGoSyntaxSilentOnCleanFile(t *testing.T) {
	if note := verifyGoSyntax("a.go", []byte(verifyClean)); note != "" {
		t.Fatalf("clean file must produce no note, got %q", note)
	}
}

func TestVerifyGoSyntaxIgnoresNonGoAndOversize(t *testing.T) {
	// A non-Go file is not the verifier's business — parse it and every
	// JSON/YAML/markdown edit grows a spurious warning.
	if note := verifyGoSyntax("a.json", []byte("{not json")); note != "" {
		t.Fatalf("non-Go file must produce no note, got %q", note)
	}
	big := make([]byte, verifyMaxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if note := verifyGoSyntax("a.go", big); note != "" {
		t.Fatalf("oversize file must be skipped, got %q", note)
	}
}

func TestVerifyGoSyntaxCountsLaterErrors(t *testing.T) {
	// Two independent breaks: the note leads with the first and says how
	// many more there are, so the model knows whether it is one fix or five.
	src := "package p\n\nfunc f( {\n}\n\nfunc g( {\n}\n"
	note := verifyGoSyntax("a.go", []byte(src))
	if note == "" || !strings.Contains(note, "more") {
		t.Fatalf("multiple parse errors must be counted, got %q", note)
	}
	if len(note) > verifyMaxMessage+120 {
		t.Fatalf("note grew past its cap: %d bytes", len(note))
	}
}

// writeResult runs the write tool against a temp file, returning the text.
func writeResult(t *testing.T, path, content string) Result {
	t.Helper()
	wt := NewWriteTool()
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	res, err := wt.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.IsError {
		t.Fatalf("write refused: %s", res.Text)
	}
	return res
}

func TestWriteReportsGoSyntaxError(t *testing.T) {
	dir := t.TempDir()
	res := writeResult(t, filepath.Join(dir, "broken.go"), verifyBroken)
	if !strings.Contains(res.Text, "syntax: ") {
		t.Fatalf("write of an unparseable Go file must say so, got:\n%s", res.Text)
	}
	details, _ := res.Details.(map[string]any)
	if details["syntaxWarning"] == nil {
		t.Fatalf("the note must also land in Details, got %v", res.Details)
	}
}

func TestWriteGoFileStaysQuietWhenItParses(t *testing.T) {
	dir := t.TempDir()
	res := writeResult(t, filepath.Join(dir, "ok.go"), verifyClean)
	if strings.Contains(res.Text, "syntax") {
		t.Fatalf("a clean write must not mention syntax, got:\n%s", res.Text)
	}
}

func TestWriteNonGoFileStaysQuiet(t *testing.T) {
	dir := t.TempDir()
	res := writeResult(t, filepath.Join(dir, "notes.md"), "# not go (\n")
	if strings.Contains(res.Text, "syntax") {
		t.Fatalf("a non-Go write must not be parsed, got:\n%s", res.Text)
	}
}

// editResult runs one hashline patch through the edit tool.
func editResult(t *testing.T, path, patch string) Result {
	t.Helper()
	et := NewEditTool()
	args, _ := json.Marshal(map[string]string{"input": patch})
	res, err := et.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if res.IsError {
		t.Fatalf("edit refused: %s", res.Text)
	}
	return res
}

// The case the enhancement is actually about: a line-range patch removes a
// brace and the file is broken. Before this, the model learned that one
// turn later when its build failed.
func TestEditReportsGoSyntaxErrorItIntroduced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(verifyClean), 0o644); err != nil {
		t.Fatal(err)
	}
	// Drop the closing brace (line 5) by replacing it with nothing.
	res := editResult(t, path, "["+path+"]\nREM 5")
	if !strings.Contains(res.Text, "syntax: ") {
		t.Fatalf("an edit that breaks the parse must say so, got:\n%s", res.Text)
	}
	details, _ := res.Details.(map[string]any)
	if details["syntaxWarning"] == nil {
		t.Fatalf("the note must also land in Details, got %v", res.Details)
	}
	// The write still happened — the verifier reports, it never vetoes.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the edit must not be rolled back: %v", err)
	}
}

func TestEditStaysQuietWhenItKeepsTheFileParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(verifyClean), 0o644); err != nil {
		t.Fatal(err)
	}
	res := editResult(t, path, "["+path+"]\nPUT >4:\n+\t_ = 1")
	if strings.Contains(res.Text, "syntax") {
		t.Fatalf("a valid edit must not mention syntax, got:\n%s", res.Text)
	}
}

// --- tier 1: the language-server half of the ladder (#263) -----------------
//
// A parser cannot see a type error, so without this tier a Go file that
// compiles wrong is indistinguishable from one that is fine until the model
// spends a turn on `go build`.

// stubProvider is a DiagnosticsProvider with a fixed answer.
type stubProvider struct {
	problems []Problem
	ok       bool
	calls    int
	delay    time.Duration
}

func (s *stubProvider) DiagnosticsFor(ctx context.Context, path string, wait time.Duration) ([]Problem, bool) {
	s.calls++
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
		}
	}
	return s.problems, s.ok
}

func TestVerifyReportsLanguageServerDiagnostics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "typed.go")
	if err := os.WriteFile(path, []byte(verifyClean), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.SetDiagnosticsProvider(&stubProvider{
		ok: true,
		problems: []Problem{
			{Severity: "error", Location: "typed.go:5:2", Message: "undefined: fmtx"},
		},
	})
	if note := reg.VerifyAfterWrite(context.Background(), path); note == "" {
		t.Fatal("a type error the parser cannot see must still reach the model")
	} else if !strings.Contains(note, "undefined: fmtx") || !strings.Contains(note, "typed.go:5:2") {
		t.Fatalf("the note must carry the message and the position, got %q", note)
	}
}

func TestVerifyPrefersTheParseErrorOverTheServer(t *testing.T) {
	// A file that does not parse cannot produce meaningful type diagnostics.
	// Reporting both would bury the actionable one.
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(path, []byte(verifyBroken), 0o644); err != nil {
		t.Fatal(err)
	}
	sp := &stubProvider{ok: true, problems: []Problem{
		{Severity: "error", Location: "broken.go:1:1", Message: "type error nobody can trust"},
	}}
	reg := NewRegistry()
	reg.SetDiagnosticsProvider(sp)
	note := reg.VerifyAfterWrite(context.Background(), path)
	if !strings.HasPrefix(note, "syntax: ") {
		t.Fatalf("the parse error must lead, got %q", note)
	}
	if sp.calls != 0 {
		t.Fatalf("the language server must not be consulted for an unparseable file (%d calls)", sp.calls)
	}
}

func TestVerifyOrdersErrorsAheadOfWarnings(t *testing.T) {
	// The truncation cap keeps the first N, so a file with five warnings and
	// one error must not report five warnings and hide the error.
	problems := make([]Problem, 0, 6)
	for i := 0; i < 5; i++ {
		problems = append(problems, Problem{Severity: "warning", Message: "chatter" + string(rune('a'+i))})
	}
	problems = append(problems, Problem{Severity: "error", Message: "the real problem"})
	note := renderProblems(problems)
	if !strings.Contains(note, "the real problem") {
		t.Fatalf("the error must survive the cap, got %q", note)
	}
	if !strings.Contains(note, "+1 more") {
		t.Fatalf("the dropped ones must be counted, got %q", note)
	}
}

func TestVerifyStaysQuietWithoutAProvider(t *testing.T) {
	// The default wiring has no provider (--no-lsp, or a repo with no server
	// for this language). That must be silent, not an error.
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.go")
	if err := os.WriteFile(path, []byte(verifyClean), 0o644); err != nil {
		t.Fatal(err)
	}
	if note := NewRegistry().VerifyAfterWrite(context.Background(), path); note != "" {
		t.Fatalf("no provider means no note, got %q", note)
	}
}

func TestVerifyProviderThatAnswersNothingIsSilent(t *testing.T) {
	// ok=false is "nothing warm for this file", which is the common case. It
	// must not surface as a problem on a successful write.
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.go")
	if err := os.WriteFile(path, []byte(verifyClean), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.SetDiagnosticsProvider(&stubProvider{ok: false})
	if note := reg.VerifyAfterWrite(context.Background(), path); note != "" {
		t.Fatalf("a provider with nothing to say must be silent, got %q", note)
	}
}

func TestVerifyDoesNotBlockPastCancellation(t *testing.T) {
	// The write path must not be held open by a slow language server. A
	// canceled context has to end the wait, and the note is dropped rather
	// than blocking the turn the model is holding.
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.go")
	if err := os.WriteFile(path, []byte(verifyClean), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.SetDiagnosticsProvider(&stubProvider{ok: false, delay: 30 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan string, 1)
	go func() { done <- reg.VerifyAfterWrite(ctx, path) }()
	select {
	case note := <-done:
		if note != "" {
			t.Fatalf("a canceled verify must not report, got %q", note)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("VerifyAfterWrite ignored context cancellation and held the write open")
	}
}
