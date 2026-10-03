package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The write-verification ladder asks this tool about every edit (#263), so
// the contract here is mostly about restraint: what it must NOT do, and what
// "nothing to report" has to look like.

// TestDiagnosticsForNeverLaunchesAServer is the load-bearing guarantee. The
// write path is on the critical path of the tool the model trusts most; a
// cold language-server handshake there would add seconds to every edit in a
// repo the model has not queried yet. So this reports ok=false when nothing
// is warm, and the caller falls back to the cheap parser tier.
func TestDiagnosticsForNeverLaunchesAServer(t *testing.T) {
	tool, f, st := newTestTool(t, defaultTestConfig())

	problems, ok := tool.DiagnosticsFor(context.Background(), filepath.Join(tool.CWD, "a.go"), 50*time.Millisecond)
	if ok || len(problems) != 0 {
		t.Fatalf("cold manager = (%v, %v), want no answer", problems, ok)
	}
	if st.count() != 0 {
		t.Fatalf("DiagnosticsFor launched %d servers; it must never launch one", st.count())
	}
	if f.notificationCount("textDocument/didOpen") != 0 {
		t.Fatal("a cold manager must not even open the document")
	}
}

// TestDiagnosticsForReportsWhatAWarmServerPublished is the feature: the rich
// tier that catches a type error the parser cannot see.
func TestDiagnosticsForReportsWhatAWarmServerPublished(t *testing.T) {
	tool, f, _ := newTestTool(t, defaultTestConfig())
	abs := filepath.Join(tool.CWD, "a.go")

	// Warm it the way a real session would: an lsp query.
	if _, err := tool.Execute(context.Background(), []byte(`{"op":"diagnostics","file":"a.go"}`)); err != nil {
		t.Fatalf("warm: %v", err)
	}
	f.publish(uriFromPath(abs), []Diagnostic{{
		Severity: 1, Message: "undefined: alpha",
		Range: Range{Start: Position{Line: 2, Character: 5}},
	}})

	problems, ok := tool.DiagnosticsFor(context.Background(), abs, time.Second)
	if !ok {
		t.Fatal("a warm server with a published error must answer")
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", problems)
	}
	if problems[0].Severity != "error" {
		t.Errorf("severity = %q, want error", problems[0].Severity)
	}
	// The position is 0-based on the wire and 1-based for a human.
	if !strings.HasSuffix(problems[0].Location, "a.go:3:6") {
		t.Errorf("location = %q, want a human 1-based position", problems[0].Location)
	}
	if problems[0].Message != "undefined: alpha" {
		t.Errorf("message = %q", problems[0].Message)
	}
}

// TestDiagnosticsForSilentWhenTheServerIsClean keeps a successful edit
// silent. A note on every clean write would train the model to ignore notes.
func TestDiagnosticsForSilentWhenTheServerIsClean(t *testing.T) {
	tool, f, _ := newTestTool(t, defaultTestConfig())
	abs := filepath.Join(tool.CWD, "a.go")
	if _, err := tool.Execute(context.Background(), []byte(`{"op":"diagnostics","file":"a.go"}`)); err != nil {
		t.Fatalf("warm: %v", err)
	}
	f.publish(uriFromPath(abs), []Diagnostic{})

	if problems, ok := tool.DiagnosticsFor(context.Background(), abs, time.Second); ok || len(problems) != 0 {
		t.Fatalf("a clean file = (%+v, %v), want no answer", problems, ok)
	}
}

// TestWarmClientForIsAPeek pins the non-launching lookup on its own, since the
// write path's safety rests entirely on it.
func TestWarmClientForIsAPeek(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n")
	mgr := NewManager(Config{Lazy: true, IdleTimeout: time.Minute, Servers: DefaultServers()}, dir)
	t.Cleanup(mgr.Close)
	st := &starter{client: newFakeLSP(t, nil).client}
	mgr.start = st.start

	if _, _, warm := mgr.WarmClientFor(filepath.Join(dir, "a.go")); warm {
		t.Fatal("nothing is running yet")
	}
	if st.count() != 0 {
		t.Fatalf("WarmClientFor launched %d servers", st.count())
	}
	// A language with no configured server is not an error either.
	if _, _, warm := mgr.WarmClientFor(filepath.Join(dir, "q.unknownext")); warm {
		t.Fatal("an unroutable file must not report a warm client")
	}
	if st.count() != 0 {
		t.Fatalf("an unroutable file launched %d servers", st.count())
	}
}

// TestWarmClientForSurvivesASecondSpellingOfTheSameFile is a regression
// guard for a bug found by running the real thing, not by reading it.
//
// A file can be named two ways: macOS answers /tmp/x when asked about
// /private/tmp/x, and a symlinked working directory does the same in reverse.
// When a client was started under one spelling and looked up under the other,
// the peek missed and the post-write diagnostics silently dropped to the
// parser tier for the whole session -- the write looked "fine" because the
// report never ran, which is the worst way for this feature to fail.
//
// Both spellings must find the SAME running server.
func TestWarmClientForSurvivesASecondSpellingOfTheSameFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	realFile := filepath.Join(dir, "a.go")
	writeFile(t, realFile, "package x\n")

	// A symlink is the portable way to produce a second spelling; macOS /tmp
	// is the same hazard without one.
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	spelled := filepath.Join(link, "a.go")

	mgr := NewManager(Config{Lazy: true, IdleTimeout: time.Minute, Servers: DefaultServers()}, dir)
	defer mgr.Close()
	st := &starter{client: newFakeLSP(t, nil).client}
	mgr.start = st.start

	// Start the server the way a real session does: with the real spelling.
	if _, _, err := mgr.ClientFor(context.Background(), realFile); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, _, ok := mgr.WarmClientFor(realFile); !ok {
		t.Fatal("the same spelling that started the server must find it")
	}
	// The one that used to fail.
	if _, _, ok := mgr.WarmClientFor(spelled); !ok {
		t.Fatalf("WarmClientFor(%s) missed a running server started for %s; "+
			"one file must have one identity", spelled, realFile)
	}
	// And it must be the very same client, not a second launch.
	if st.count() != 1 {
		t.Fatalf("the second spelling launched %d servers; it must reuse the running one", st.count())
	}
}
