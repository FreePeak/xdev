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
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"signatureHelp","file":"a.go"}`))
	if err != nil || !out.IsError || !strings.Contains(out.Text, "unknown op signatureHelp") {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	// rename is a real op now (#96) and must still refuse without a name.
	if out, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"rename","file":"a.go"}`)); !out.IsError || !strings.Contains(out.Text, "needs new_name") {
		t.Fatalf("rename without new_name = %+v", out)
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

// The edit-plan renderers are pure decoding, so they are tested directly:
// a rename's `changes` map, the documentChanges form, and the Command-shaped
// codeAction array servers really send.
func TestWorkspaceEditAndCodeActionDecoding(t *testing.T) {
	ed := json.RawMessage(`{"changes":{"file:///w/a.go":[{"range":{"start":{"line":3,"character":0},"end":{"line":3,"character":4}},"newText":"newName"}]}}`)
	text := renderWorkspaceEdit(ed, "/w")
	if !strings.Contains(text, "a.go: 1 edit(s)") || !strings.Contains(text, "line 4") {
		t.Fatalf("changes form rendered as: %q", text)
	}
	// A non-file URI must come back unchanged rather than be mangled.
	if got := pathFromURI("untitled:a.go"); got != "untitled:a.go" {
		t.Fatalf("non-file URI mangled: %q", got)
	}
	if got := pathFromURI("file:///w/a.go"); got != "/w/a.go" {
		t.Fatalf("file URI decoded to %q", got)
	}

	// Object form with kinds and edits.
	objs, err := parseCodeActions(json.RawMessage(`[{"title":"Extract function","kind":"refactor.extract","edit":{"changes":{"file:///w/b.go":[]}}}]`))
	if err != nil || len(objs) != 1 {
		t.Fatalf("object form: %v (%+v)", err, objs)
	}
	if objs[0].Title != "Extract function" || len(objs[0].editFiles()) != 1 {
		t.Fatalf("action decoded as %+v", objs[0])
	}
	// Command form (no edit) is legal per the spec and must not error.
	cmds, err := parseCodeActions(json.RawMessage(`[{"title":"Add import","command":"go.add.import"}]`))
	if err != nil || len(cmds) != 1 || !strings.Contains(cmds[0].Kind, "go.add.import") {
		t.Fatalf("command form: %v (%+v)", err, cmds)
	}
	// Empty and null responses decode to no actions, not an error.
	if got, err := parseCodeActions(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty response: %v %v", got, err)
	}
}

// CapabilitiesSummary lists what the server offered, dot-pathed and sorted,
// and hides the providers it explicitly declined.
func TestCapabilitiesSummary(t *testing.T) {
	c := &Client{}
	c.mu.Lock()
	c.caps = json.RawMessage(`{"renameProvider":true,"codeActionProvider":{"resolveProvider":false},"hoverProvider":false}`)
	c.mu.Unlock()
	got := c.CapabilitiesSummary()
	// An options object counts as the capability being offered, even when its
	// only leaf (resolveProvider) is false; a declined scalar does not.
	if !strings.Contains(got, "renameProvider") || !strings.Contains(got, "codeActionProvider") {
		t.Fatalf("offered providers missing from %q", got)
	}
	if strings.Contains(got, "hoverProvider") {
		t.Fatalf("a declined provider must not be listed: %q", got)
	}
	if strings.Contains(got, "codeActionProvider.resolveProvider") {
		t.Fatalf("a false leaf must not be listed as offered: %q", got)
	}
}

// codeAction needs a RANGE, not a point. Building it used to type-assert the
// JSON map form of a position the helper returns as a Position struct, so the
// first real request panicked in the tool goroutine (#96, caught only by
// driving a live gopls). The fake records the params it was sent.
func TestCodeActionsSendsARangeNotAPoint(t *testing.T) {
	tool, f, _ := newTestTool(t, defaultTestConfig())
	f.setRespond(func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method == "textDocument/codeAction" {
			return json.RawMessage(`[{"title":"Add import","kind":"quickfix"}]`), nil
		}
		return defaultResponder(method, params)
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"code_actions","file":"a.go","line":3,"col":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError {
		t.Fatalf("code_actions errored: %s", out.Text)
	}
	var sent struct {
		Range    *codeRange `json:"range"`
		Position *Position  `json:"position"`
	}
	if err := json.Unmarshal(f.lastParams("textDocument/codeAction"), &sent); err != nil {
		t.Fatalf("params decode: %v", err)
	}
	if sent.Range == nil {
		t.Fatalf("no range in the codeAction request: %s", f.lastParams("textDocument/codeAction"))
	}
	// 1-based input becomes 0-based LSP, widened by one character.
	if sent.Range.Start.Line != 2 || sent.Range.End.Line != 2 || sent.Range.End.Character != sent.Range.Start.Character+1 {
		t.Fatalf("range = %+v, want line 2 widened by one character", sent.Range)
	}
	if sent.Position != nil {
		t.Fatal("a codeAction request must not also send a bare position")
	}
	if !strings.Contains(out.Text, "Add import") || !strings.Contains(out.Text, "quickfix") {
		t.Fatalf("listing dropped the action: %s", out.Text)
	}
}

// defaultResponder answers the handshake and document requests the fake is
// asked about, leaving the test's own responder to override any method.
func defaultResponder(method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "textDocument/codeAction":
		return json.RawMessage(`[]`), nil
	}
	return nil, nil
}

// codeRange mirrors the wire shape for assertions (the package type is
// exported as Range; this decodes the JSON that was SENT).
type codeRange struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}
