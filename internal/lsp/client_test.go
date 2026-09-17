package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFramingRoundTrip drives a real request/response exchange through the
// Content-Length framing against the in-process server.
func TestFramingRoundTrip(t *testing.T) {
	f := newFakeLSP(t, echoResponder(func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "textDocument/hover" {
			t.Errorf("unexpected method %q", method)
		}
		return json.RawMessage(`{"contents":{"value":"func main()"}}`), nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := f.client.Call(ctx, "textDocument/hover", map[string]any{"position": Position{Line: 3}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := hoverText(raw); got != "func main()" {
		t.Fatalf("hover text = %q", got)
	}

	// A notification carries no id and reaches the server as an event.
	if err := f.client.Notify("initialized", map[string]any{}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	waitFor(t, "initialized notification", func() bool { return f.notificationCount("initialized") == 1 })
	if p := string(f.lastParams("initialized")); p != "{}" {
		t.Fatalf("initialized params = %q", p)
	}
}

func TestReadFrameHeaders(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "plain", in: "Content-Length: 2\r\n\r\n{}", want: "{}"},
		{name: "lowercase header", in: "content-length: 2\r\n\r\n{}", want: "{}"},
		{name: "extra headers skipped", in: "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: 2\r\n\r\n{}", want: "{}"},
		{name: "lf only", in: "Content-Length: 2\n\n{}", want: "{}"},
		{name: "missing length", in: "Content-Type: x\r\n\r\n{}", wantErr: "missing Content-Length"},
		{name: "bad length", in: "Content-Length: abc\r\n\r\n{}", wantErr: "bad Content-Length"},
		{name: "over cap", in: "Content-Length: 99999999\r\n\r\n{}", wantErr: "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readFrame(bufio.NewReader(strings.NewReader(tc.in)))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readFrame: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("frame = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCallSurfacesServerError(t *testing.T) {
	f := newFakeLSP(t, echoResponder(func(method string, params json.RawMessage) (json.RawMessage, error) {
		return nil, errString("no package metadata")
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.client.Call(ctx, "textDocument/definition", nil)
	if err == nil || !strings.Contains(err.Error(), "no package metadata") {
		t.Fatalf("err = %v, want the server's message", err)
	}
}

// TestDiagnosticsCacheAndWait covers the async publish path: a file the
// server has not reported yet waits for the first publish, and a later
// publish replaces the cached set.
func TestDiagnosticsCacheAndWait(t *testing.T) {
	f := newFakeLSP(t, echoResponder(nil))
	path := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(path, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := uriFromPath(path)

	go func() {
		time.Sleep(60 * time.Millisecond)
		f.publish(uri, []Diagnostic{{Severity: 1, Source: "gopls", Message: "undefined: x"}})
	}()
	ds, seen := f.client.DiagnosticsFor(context.Background(), path, 2*time.Second)
	if !seen || len(ds) != 1 || ds[0].Message != "undefined: x" {
		t.Fatalf("diagnostics = %+v seen=%v", ds, seen)
	}

	f.publish(uri, nil)
	waitFor(t, "clean publish", func() bool {
		ds, seen := f.client.DiagnosticsFor(context.Background(), path, 0)
		return seen && len(ds) == 0
	})
}

// TestEnsureOpenTracksEdits checks didOpen once and didChange only when the
// on-disk content actually moved on.
func TestEnsureOpenTracksEdits(t *testing.T) {
	f := newFakeLSP(t, echoResponder(nil))
	path := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(path, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.client.EnsureOpen(path, "go"); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}
	if err := f.client.EnsureOpen(path, "go"); err != nil {
		t.Fatalf("EnsureOpen (unchanged): %v", err)
	}
	waitFor(t, "didOpen", func() bool { return f.notificationCount("textDocument/didOpen") == 1 })
	if n := f.notificationCount("textDocument/didChange"); n != 0 {
		t.Fatalf("didChange count = %d, want 0 while the file is unchanged", n)
	}

	if err := os.WriteFile(path, []byte("package a\n\nfunc f() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.client.EnsureOpen(path, "go"); err != nil {
		t.Fatalf("EnsureOpen (edited): %v", err)
	}
	waitFor(t, "didChange", func() bool { return f.notificationCount("textDocument/didChange") == 1 })
	if p := string(f.lastParams("textDocument/didChange")); !strings.Contains(p, `"version":2`) {
		t.Fatalf("didChange params = %s, want version 2", p)
	}
}

// TestClientDeadAfterClose makes sure a closed client fails fast instead of
// hanging a tool call.
func TestClientDeadAfterClose(t *testing.T) {
	f := newFakeLSP(t, echoResponder(nil))
	_ = f.client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.client.Call(ctx, "textDocument/hover", nil); err == nil {
		t.Fatal("call on a closed client should fail")
	}
}
