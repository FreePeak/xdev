package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestTool builds the tool over a manager whose launch seam hands back the
// in-process fake server.
func newTestTool(t *testing.T, cfg Config) (*Tool, *fakeLSP, *starter) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n\nfunc alpha() {}\n")
	tool := NewToolWithConfig(cfg, dir)
	f := newFakeLSP(t, nil)
	st := &starter{client: f.client}
	tool.mgr.start = st.start
	t.Cleanup(tool.Close)
	return tool, f, st
}

func defaultTestConfig() Config {
	return Config{Lazy: true, IdleTimeout: time.Minute, Servers: DefaultServers()}
}

// TestDefinitionCapsSites is the output-bound contract: 60 sites come back as
// MaxSites sites plus an explicit truncation marker.
func TestDefinitionCapsSites(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n\nfunc alpha() {}\n")

	var locs []string
	for i := 0; i < 60; i++ {
		locs = append(locs, fmt.Sprintf(`{"uri":"file://%s/a.go","range":{"start":{"line":%d,"character":2},"end":{"line":%d,"character":7}}}`, dir, i, i))
	}
	res := fmt.Sprintf("[%s]", strings.Join(locs, ","))
	f := newFakeLSP(t, echoResponder(func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "textDocument/definition" {
			return nil, errString("unexpected method " + method)
		}
		return json.RawMessage(res), nil
	}))
	tool := NewToolWithConfig(defaultTestConfig(), dir)
	defer tool.Close()
	tool.mgr.start = (&starter{client: f.client}).start

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"definition","file":"a.go","symbol":"alpha"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected error result: %s", out.Text)
	}
	if got := strings.Count(out.Text, "\n") + 1; got != MaxSites+1 { // 50 sites + marker line
		t.Fatalf("line count = %d, want %d\n%s", got, MaxSites+1, out.Text)
	}
	if !strings.Contains(out.Text, "+10 more sites (capped at 50)") {
		t.Fatalf("missing truncation marker:\n%s", out.Text)
	}
	if last := lastLine(out.Text); last != "… +10 more sites (capped at 50)" {
		t.Fatalf("last line = %q", last)
	}
	if !strings.Contains(out.Text, "a.go:50:3") {
		t.Fatalf("50th site missing:\n%s", out.Text)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestHoverCapTruncates checks the 8KB text bound.
func TestHoverCapTruncates(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n\nfunc alpha() {}\n")

	big := strings.Repeat("x", 10<<10)
	f := newFakeLSP(t, echoResponder(func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "textDocument/hover" {
			return nil, errString("unexpected method " + method)
		}
		raw, _ := json.Marshal(map[string]any{"contents": map[string]any{"kind": "markdown", "value": big}})
		return raw, nil
	}))
	tool := NewToolWithConfig(defaultTestConfig(), dir)
	defer tool.Close()
	tool.mgr.start = (&starter{client: f.client}).start

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"hover","file":"a.go","line":3,"col":6}`))
	if err != nil || out.IsError {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	if !strings.Contains(out.Text, "truncated at 8192 bytes") {
		t.Fatalf("missing truncation marker (len=%d)", len(out.Text))
	}
	if len(out.Text) > MaxTextBytes+64 {
		t.Fatalf("hover text = %d bytes, want <= %d plus marker", len(out.Text), MaxTextBytes)
	}
}

// TestSymbolsRendering covers the documentSymbol shape (nested children).
func TestSymbolsRendering(t *testing.T) {
	tool, f, _ := newTestTool(t, defaultTestConfig())
	f.setRespond(echoResponder(func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "textDocument/documentSymbol" {
			return nil, errString("unexpected method " + method)
		}
		return json.RawMessage(fmt.Sprintf(`[
			{"name":"Config","kind":23,"range":{"start":{"line":0,"character":0}},
			 "children":[{"name":"Name","kind":8,"range":{"start":{"line":1,"character":1}}}]},
			{"name":"alpha","kind":12,"detail":"func()"},
			{"name":"beta","kind":12,"location":{"uri":"file://%s/a.go","range":{"start":{"line":9,"character":0}}}}
		]`, tool.CWD)), nil
	}))
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"symbols","file":"a.go"}`))
	if err != nil || out.IsError {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	want := "Struct Config\n  Field Name\nFunction alpha — func()\nFunction beta  (a.go:10)"
	if strings.TrimRight(out.Text, "\n") != want {
		t.Fatalf("symbols =\n%q\nwant\n%q", out.Text, want)
	}
}

func TestSymbolsWorkspaceQueryRoutesThroughFileServer(t *testing.T) {
	tool, f, _ := newTestTool(t, defaultTestConfig())
	f.setRespond(echoResponder(func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "workspace/symbol" {
			return nil, errString("unexpected method " + method)
		}
		return json.RawMessage(`[{"name":"alpha","kind":12,"location":{"uri":"file:///tmp/a.go","range":{"start":{"line":4,"character":5}}}}]`), nil
	}))
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"symbols","file":"a.go","symbol":"alph"}`))
	if err != nil || out.IsError {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	if !strings.Contains(out.Text, "Function alpha") || !strings.Contains(out.Text, "/tmp/a.go:5:6") {
		t.Fatalf("workspace symbols = %q", out.Text)
	}
	if p := string(f.lastParams("workspace/symbol")); p != `{"query":"alph"}` {
		t.Fatalf("query params = %s", p)
	}
}

func TestDiagnosticsReporting(t *testing.T) {
	tool, f, _ := newTestTool(t, defaultTestConfig())
	uri := uriFromPath(filepath.Join(tool.CWD, "a.go"))
	f.publish(uri, []Diagnostic{{
		Severity: 1, Source: "gopls", Message: "undefined: alpha",
		Range: Range{Start: Position{Line: 2, Character: 5}},
	}})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"diagnostics","file":"a.go"}`))
	if err != nil || out.IsError {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	if !strings.Contains(out.Text, "a.go:3:6: error: undefined: alpha (gopls)") {
		t.Fatalf("diagnostics = %q", out.Text)
	}

	// file:"*" reports everything the servers have published so far.
	all, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"diagnostics","file":"*"}`))
	if err != nil || all.IsError || !strings.Contains(all.Text, "undefined: alpha") {
		t.Fatalf("all diagnostics = %+v err = %v", all, err)
	}
}

// TestPositionFromSymbol checks the request the server actually receives:
// {symbol} is resolved to a line/column in the file.
func TestPositionFromSymbol(t *testing.T) {
	tool, f, _ := newTestTool(t, defaultTestConfig())
	f.setRespond(echoResponder(func(method string, params json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`[]`), nil
	}))
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"references","file":"a.go","symbol":"alpha"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	waitFor(t, "references request", func() bool { return len(f.lastParams("textDocument/references")) > 0 })
	p := string(f.lastParams("textDocument/references"))
	// "func alpha() {}" is line 3, 1-based col 6 -> 0-based {2,5}.
	if !strings.Contains(p, `"position":{"line":2,"character":5}`) {
		t.Fatalf("references params = %s", p)
	}
	if !strings.Contains(p, `"includeDeclaration":true`) {
		t.Fatalf("references params = %s, want includeDeclaration", p)
	}
}

// TestMissingServerIsToolError: an absent binary is a tool error the model can
// act on, never a panic or a Go error.
func TestMissingServerIsToolError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package x\n")
	tool := NewToolWithConfig(Config{
		Lazy:        true,
		IdleTimeout: time.Minute,
		Servers:     map[string]ServerSpec{"go": {Command: "xdev-lsp-binary-that-does-not-exist", FileTypes: []string{"go"}}},
	}, dir)
	defer tool.Close()

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"definition","file":"a.go","line":1,"col":1}`))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if !out.IsError || !strings.Contains(out.Text, "not found on PATH") {
		t.Fatalf("out = %+v", out)
	}
}

func TestUnknownOpAndBadArgs(t *testing.T) {
	tool, _, st := newTestTool(t, defaultTestConfig())
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"rename","file":"a.go"}`))
	if err != nil || !out.IsError || !strings.Contains(out.Text, "unknown op rename") {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	out, err = tool.Execute(context.Background(), json.RawMessage(`{"op":"definition"}`))
	if err != nil || !out.IsError || !strings.Contains(out.Text, "needs a file") {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	out, err = tool.Execute(context.Background(), json.RawMessage(`not json`))
	if err != nil || !out.IsError {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	if st.count() != 0 {
		t.Fatalf("bad arguments launched %d server(s)", st.count())
	}
}

func TestToolSchemaIsValidJSON(t *testing.T) {
	tool := NewTool("/tmp", nil)
	if !json.Valid(tool.Parameters()) {
		t.Fatal("Parameters() is not valid JSON")
	}
	if tool.Name() != "lsp" || tool.Description() == "" {
		t.Fatalf("name/description = %q/%q", tool.Name(), tool.Description())
	}
}

func TestResolvePosition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeFile(t, path, "package x\n\nfunc alpha() { alpha() }\n")

	cases := []struct {
		name    string
		line    int
		col     int
		symbol  string
		want    Position
		wantErr string
	}{
		{name: "explicit col", line: 3, col: 6, want: Position{Line: 2, Character: 5}},
		{name: "snaps to first non-space", line: 2, want: Position{Line: 1, Character: 0}},
		{name: "symbol", symbol: "alpha", want: Position{Line: 2, Character: 5}},
		{name: "second occurrence", symbol: "alpha#2", want: Position{Line: 2, Character: 15}},
		{name: "missing symbol", symbol: "beta", wantErr: "not found"},
		{name: "line past end", line: 99, wantErr: "past the end"},
		{name: "no position", wantErr: "line (1-based) is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePosition(path, tc.line, tc.col, tc.symbol)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePosition: %v", err)
			}
			if got != tc.want {
				t.Fatalf("position = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCapTextKeepsRuneBoundaries(t *testing.T) {
	s := strings.Repeat("é", 100) // 2 bytes each
	got := capText(s, 51)
	if !strings.Contains(got, "truncated at 51 bytes") {
		t.Fatalf("missing marker: %q", got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("split a rune: %q", got)
	}
}

// TestSymbolSkipsComments pins the comment-skip in symbolOffset: a doc
// comment naming the symbol must not shadow its declaration (gopls answers
// "no identifier found" on a comment position).
func TestSymbolSkipsComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeFile(t, path, "package x\n\n// alpha is documented here.\nfunc alpha() { alpha() }\n")

	got, err := resolvePosition(path, 0, 0, "alpha")
	if err != nil {
		t.Fatalf("resolvePosition: %v", err)
	}
	// Line 4 (the func line), not line 3 (the doc comment).
	if got != (Position{Line: 3, Character: 5}) {
		t.Fatalf("position = %+v, want the declaration not the doc comment", got)
	}
}
