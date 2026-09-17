package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// Output bounds (M13 #52): a query must never dump a whole dependency graph
// or a 200KB hover into the model's context.
const (
	MaxSites     = 50
	MaxTextBytes = 8 << 10
	// diagWait bounds how long a diagnostics call waits for the first
	// publish of a file the server has just been told about.
	diagWait       = 2 * time.Second
	defaultTimeout = 30 * time.Second
	minTimeout     = 5 * time.Second
	maxTimeout     = 300 * time.Second
)

// Tool is the `lsp` tool: read-only language-server queries over the servers
// configured in lsp.servers, launched lazily on first use.
type Tool struct {
	CWD string

	mgr *Manager
}

// NewTool builds the tool from the layered settings. A nil settings value
// yields the built-in defaults (lazy, 5m idle timeout).
func NewTool(cwd string, settings *config.Settings) *Tool {
	return NewToolWithConfig(ConfigFromSettings(settings), cwd)
}

// NewToolWithConfig builds the tool from an explicit configuration.
func NewToolWithConfig(cfg Config, cwd string) *Tool {
	return &Tool{CWD: cwd, mgr: NewManager(cfg, cwd)}
}

func (t *Tool) Name() string { return "lsp" }

func (t *Tool) Description() string {
	return "language-server queries (diagnostics, definition, references, hover, symbols); servers such as gopls/rust-analyzer/pyright start lazily on first use"
}

func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {"type": "string", "enum": ["diagnostics", "definition", "references", "hover", "symbols", "rename", "code_actions", "capabilities"], "description": "query to run; rename and code_actions REPORT the workspace edit they would make and never write files — carry the change out with edit/write"},
    "file": {"type": "string", "description": "file path (relative to cwd); \"*\" for diagnostics = every file reported so far"},
    "line": {"type": "integer", "description": "1-based line of the position (omit when passing symbol)"},
    "col": {"type": "integer", "description": "1-based column (defaults to the first non-space character of the line)"},
    "symbol": {"type": "string", "description": "identifier to locate, e.g. \"resolveModel\" or \"resolveModel#2\" for the second occurrence; for op=symbols it becomes the workspace symbol query"},
    "timeout": {"type": "integer", "description": "per-action timeout in seconds (5..300, default 30)"},
    "new_name": {"type": "string", "description": "op=rename: the new identifier"},
    "query": {"type": "string", "description": "op=code_actions: filter the list by title substring (case-insensitive)"},
    "index": {"type": "integer", "description": "op=code_actions: which filtered action to describe (0-based, default 0)"},
    "apply": {"type": "boolean", "description": "accepted for forward compatibility and ignored: these ops never mutate files"}
  },
  "required": ["op"]
}`)
}

// Prewarm starts configured servers up front when lsp.lazy is false. A no-op
// under the default lazy configuration.
func (t *Tool) Prewarm() { t.mgr.Prewarm() }

// Close stops every server this tool launched.
func (t *Tool) Close() { t.mgr.Close() }

type lspArgs struct {
	Op      string `json:"op"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Symbol  string `json:"symbol"`
	Timeout int    `json:"timeout"`
	// NewName is the target of op "rename".
	NewName string `json:"new_name"`
	// Apply is accepted but always false in effect: rename and code_actions
	// REPORT their workspace edit rather than writing it, so the only file
	// mutations in a session keep going through the edit/write tools (where
	// the approval gate, the freshness check and secret redaction live).
	Apply bool `json:"apply"`
	// Query filters op "code_actions" by title substring.
	Query string `json:"query"`
	// Index picks one code action to apply (0-based, after the filter).
	Index int `json:"index"`
}

func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a lspArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return errResult("lsp: malformed arguments: " + err.Error()), nil
	}
	timeout := defaultTimeout
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
		if timeout < minTimeout {
			timeout = minTimeout
		}
		if timeout > maxTimeout {
			timeout = maxTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch strings.ToLower(strings.TrimSpace(a.Op)) {
	case "diagnostics":
		return t.diagnostics(ctx, a)
	case "definition", "references", "hover":
		return t.positionQuery(ctx, strings.ToLower(strings.TrimSpace(a.Op)), a)
	case "symbols":
		return t.symbols(ctx, a)
	case "rename":
		return t.rename(ctx, a)
	case "code_actions":
		return t.codeActions(ctx, a)
	case "capabilities":
		return t.capabilities(ctx, a)
	case "":
		return errResult("lsp: op is required (diagnostics|definition|references|hover|symbols|rename|code_actions|capabilities)"), nil
	default:
		return errResult("lsp: unknown op " + a.Op + " (want diagnostics|definition|references|hover|symbols|rename|code_actions|capabilities)"), nil
	}
}

// openAt resolves a file argument into a live client plus the LSP position
// and document URI the positional ops send. Shared by rename and code_actions
// so the three positional paths cannot drift apart.
func (t *Tool) openAt(ctx context.Context, op string, a lspArgs) (*Client, string, Position, map[string]any, error) {
	if strings.TrimSpace(a.File) == "" {
		return nil, "", Position{}, nil, fmt.Errorf("lsp: %s needs a file", op)
	}
	abs, err := t.absPath(a.File)
	if err != nil {
		return nil, "", Position{}, nil, err
	}
	cl, langID, err := t.mgr.ClientFor(ctx, abs)
	if err != nil {
		return nil, "", Position{}, nil, err
	}
	if err := cl.EnsureOpen(abs, langID); err != nil {
		return nil, "", Position{}, nil, err
	}
	pos, err := resolvePosition(abs, a.Line, a.Col, a.Symbol)
	if err != nil {
		return nil, "", Position{}, nil, err
	}
	uri := uriFromPath(abs)
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	}
	return cl, uri, pos, params, nil
}

// rename computes textDocument/rename and reports the resulting workspace
// edit: which files change and how many edits each carries.
//
// It deliberately does NOT write. Applying edits here would open a second
// file-mutating path that bypasses the edit tool's approval gate, freshness
// check and secret redaction — so the answer is the plan, and the model
// carries it out with edit/write where the user can see it.
func (t *Tool) rename(ctx context.Context, a lspArgs) (tool.Result, error) {
	newName := strings.TrimSpace(a.NewName)
	if newName == "" {
		return errResult("lsp: rename needs new_name"), nil
	}
	cl, _, _, params, err := t.openAt(ctx, "rename", a)
	if err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	params["newName"] = newName
	raw, err := cl.Call(ctx, "textDocument/rename", params)
	if err != nil {
		return errResult(err.Error()), nil
	}
	text := renderWorkspaceEdit(raw, t.CWD)
	if strings.TrimSpace(text) == "" {
		return tool.Result{Text: "rename to " + newName + " produces no edits"}, nil
	}
	return tool.Result{
		Text: "rename " + newName + " would change:\n" + text +
			"\n\napply it with the edit/write tools (this op reports the plan; it never edits files itself)",
	}, nil
}

// codeActions lists (and optionally applies) the code actions offered at a
// position — quick fixes and refactorings the server advertises.
func (t *Tool) codeActions(ctx context.Context, a lspArgs) (tool.Result, error) {
	cl, uri, pos, params, err := t.openAt(ctx, "code_actions", a)
	if err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	// codeAction needs a RANGE, not a point: widen the position by one
	// character (the server clamps an out-of-range end).
	params["range"] = map[string]any{
		"start": pos,
		"end":   Position{Line: pos.Line, Character: pos.Character + 1},
	}
	delete(params, "position")
	params["context"] = map[string]any{"diagnostics": []any{}}
	raw, err := cl.Call(ctx, "textDocument/codeAction", params)
	if err != nil {
		return errResult(err.Error()), nil
	}
	actions, err := parseCodeActions(raw)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if q := strings.ToLower(strings.TrimSpace(a.Query)); q != "" {
		var kept []codeAction
		for _, act := range actions {
			if strings.Contains(strings.ToLower(act.Title), q) {
				kept = append(kept, act)
			}
		}
		actions = kept
	}
	if len(actions) == 0 {
		return tool.Result{Text: "no code actions at that position"}, nil
	}
	if a.Index > 0 || (a.Apply && a.Index >= 0 && len(actions) > 1) {
		if a.Index >= len(actions) {
			return errResult(fmt.Sprintf("lsp: index %d out of range (%d actions)", a.Index, len(actions))), nil
		}
		actions = []codeAction{actions[a.Index]}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d code action(s):", len(actions))
	for i, act := range actions {
		fmt.Fprintf(&b, "\n  %d. %s", i, act.Title)
		if act.Kind != "" {
			fmt.Fprintf(&b, " [%s]", act.Kind)
		}
		if files := act.editFiles(); len(files) > 0 {
			fmt.Fprintf(&b, " — touches %s", strings.Join(files, ", "))
		}
	}
	_ = uri
	b.WriteString("\n\nlike rename, this op reports; apply the change with edit/write")
	return tool.Result{Text: capText(b.String(), MaxTextBytes)}, nil
}

// capabilities reports what the server for one file actually advertised —
// the answer to "why did that op do nothing", and the raw escape hatch that
// keeps an unusual server from being a dead end.
func (t *Tool) capabilities(ctx context.Context, a lspArgs) (tool.Result, error) {
	if strings.TrimSpace(a.File) == "" {
		return errResult("lsp: capabilities needs a file"), nil
	}
	abs, err := t.absPath(a.File)
	if err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	cl, _, err := t.mgr.ClientFor(ctx, abs)
	if err != nil {
		return errResult(err.Error()), nil
	}
	text := cl.CapabilitiesSummary()
	if text == "" {
		return tool.Result{Text: "server reported no capabilities"}, nil
	}
	return tool.Result{Text: capText(text, MaxTextBytes)}, nil
}

func errResult(msg string) tool.Result {
	return tool.Result{Text: msg, IsError: true}
}

// positionQuery handles the ops that need a position in a file.
func (t *Tool) positionQuery(ctx context.Context, op string, a lspArgs) (tool.Result, error) {
	if strings.TrimSpace(a.File) == "" {
		return errResult("lsp: " + op + " needs a file"), nil
	}
	abs, err := t.absPath(a.File)
	if err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	cl, langID, err := t.mgr.ClientFor(ctx, abs)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := cl.EnsureOpen(abs, langID); err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	pos, err := resolvePosition(abs, a.Line, a.Col, a.Symbol)
	if err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": uriFromPath(abs)},
		"position":     pos,
	}

	switch op {
	case "definition":
		raw, err := cl.Call(ctx, "textDocument/definition", params)
		if err != nil {
			return errResult(err.Error()), nil
		}
		sites := parseLocations(raw)
		if len(sites) == 0 {
			return tool.Result{Text: "no definition found"}, nil
		}
		return tool.Result{Text: capSites(sites, t.CWD), Details: map[string]any{"sites": len(sites)}}, nil
	case "references":
		params["context"] = map[string]any{"includeDeclaration": true}
		raw, err := cl.Call(ctx, "textDocument/references", params)
		if err != nil {
			return errResult(err.Error()), nil
		}
		sites := parseLocations(raw)
		if len(sites) == 0 {
			return tool.Result{Text: "no references found"}, nil
		}
		return tool.Result{Text: capSites(sites, t.CWD), Details: map[string]any{"sites": len(sites)}}, nil
	case "hover":
		raw, err := cl.Call(ctx, "textDocument/hover", params)
		if err != nil {
			return errResult(err.Error()), nil
		}
		text := hoverText(raw)
		if text == "" {
			return tool.Result{Text: "no hover information"}, nil
		}
		return tool.Result{Text: capText(text, MaxTextBytes)}, nil
	}
	return errResult("lsp: unknown op " + op), nil
}

// symbols serves op=symbols: documentSymbol for a file, or a workspace/symbol
// query when a symbol is given (routed through that file's server).
func (t *Tool) symbols(ctx context.Context, a lspArgs) (tool.Result, error) {
	if strings.TrimSpace(a.File) == "" {
		return errResult("lsp: symbols needs a file (the server is routed by file type)"), nil
	}
	abs, err := t.absPath(a.File)
	if err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	cl, langID, err := t.mgr.ClientFor(ctx, abs)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := cl.EnsureOpen(abs, langID); err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	if q := strings.TrimSpace(a.Symbol); q != "" {
		raw, err := cl.Call(ctx, "workspace/symbol", map[string]any{"query": q})
		if err != nil {
			return errResult(err.Error()), nil
		}
		text := workspaceSymbols(raw, t.CWD)
		if text == "" {
			return tool.Result{Text: "no workspace symbols match " + q}, nil
		}
		return tool.Result{Text: capText(text, MaxTextBytes)}, nil
	}
	raw, err := cl.Call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": uriFromPath(abs)},
	})
	if err != nil {
		return errResult(err.Error()), nil
	}
	text := documentSymbols(raw, t.CWD)
	if text == "" {
		return tool.Result{Text: "no symbols in " + displayPath(abs, t.CWD)}, nil
	}
	return tool.Result{Text: capText(text, MaxTextBytes)}, nil
}

// diagnostics reports the cached publishDiagnostics for one file, or for
// every file reported so far when file is "*".
func (t *Tool) diagnostics(ctx context.Context, a lspArgs) (tool.Result, error) {
	if strings.TrimSpace(a.File) == "*" {
		all := t.mgr.AllDiagnostics()
		if len(all) == 0 {
			return tool.Result{Text: "no diagnostics cached yet — servers report only files they have been asked about"}, nil
		}
		paths := make([]string, 0, len(all))
		for p := range all {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		var b strings.Builder
		for _, p := range paths {
			renderDiagnostics(&b, displayPath(p, t.CWD), all[p])
		}
		if strings.TrimSpace(b.String()) == "" {
			return tool.Result{Text: "no diagnostics: every reported file is clean"}, nil
		}
		return tool.Result{Text: capText(strings.TrimRight(b.String(), "\n"), MaxTextBytes)}, nil
	}
	if strings.TrimSpace(a.File) == "" {
		return errResult("lsp: diagnostics needs a file (or \"*\" for every reported file)"), nil
	}
	abs, err := t.absPath(a.File)
	if err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	cl, langID, err := t.mgr.ClientFor(ctx, abs)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := cl.EnsureOpen(abs, langID); err != nil {
		return errResult("lsp: " + err.Error()), nil
	}
	ds, seen := cl.DiagnosticsFor(ctx, abs, diagWait)
	if !seen {
		return tool.Result{Text: fmt.Sprintf(
			"lsp: %s reported no diagnostics within %s (the server may still be indexing)", displayPath(abs, t.CWD), diagWait)}, nil
	}
	if len(ds) == 0 {
		return tool.Result{Text: displayPath(abs, t.CWD) + ": no diagnostics"}, nil
	}
	var b strings.Builder
	renderDiagnostics(&b, displayPath(abs, t.CWD), ds)
	return tool.Result{Text: capText(strings.TrimRight(b.String(), "\n"), MaxTextBytes),
		Details: map[string]any{"diagnostics": len(ds)}}, nil
}

func (t *Tool) absPath(file string) (string, error) {
	p := file
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.CWD, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a directory", file)
	}
	return p, nil
}

type site struct {
	Path string
	Line int // 1-based
	Col  int // 1-based
}

// location covers both Location and LocationLink shapes.
type location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
	// LocationLink
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

func parseLocations(raw json.RawMessage) []site {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var locs []location
	if err := json.Unmarshal(raw, &locs); err != nil {
		var one location
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil
		}
		locs = []location{one}
	}
	out := make([]site, 0, len(locs))
	for _, l := range locs {
		uri, rng := l.URI, l.Range
		if uri == "" {
			uri, rng = l.TargetURI, l.TargetSelectionRange
			if rng == (Range{}) {
				rng = l.TargetRange
			}
		}
		if uri == "" {
			continue
		}
		out = append(out, site{Path: uriToPath(uri), Line: rng.Start.Line + 1, Col: rng.Start.Character + 1})
	}
	return out
}

// hoverText flattens the three hover content shapes (MarkupContent,
// MarkedString, MarkedString[]).
func hoverText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil || len(h.Contents) == 0 {
		return ""
	}
	var mc struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(h.Contents, &mc); err == nil && mc.Value != "" {
		return mc.Value
	}
	var s string
	if err := json.Unmarshal(h.Contents, &s); err == nil {
		return s
	}
	var marked []json.RawMessage
	if err := json.Unmarshal(h.Contents, &marked); err == nil {
		parts := make([]string, 0, len(marked))
		for _, m := range marked {
			var ms struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(m, &ms); err == nil && ms.Value != "" {
				parts = append(parts, ms.Value)
				continue
			}
			var lit string
			if err := json.Unmarshal(m, &lit); err == nil && lit != "" {
				parts = append(parts, lit)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// documentSymbols renders DocumentSymbol[] (nested) or SymbolInformation[].
func documentSymbols(raw json.RawMessage, cwd string) string {
	type docSym struct {
		Name     string    `json:"name"`
		Detail   string    `json:"detail"`
		Kind     int       `json:"kind"`
		Children []docSym  `json:"children"`
		Location *location `json:"location"`
	}
	var syms []docSym
	if err := json.Unmarshal(raw, &syms); err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(list []docSym, depth int)
	walk = func(list []docSym, depth int) {
		for _, s := range list {
			fmt.Fprintf(&b, "%s%s %s", strings.Repeat("  ", depth), symbolKind(s.Kind), s.Name)
			if s.Detail != "" {
				fmt.Fprintf(&b, " — %s", s.Detail)
			}
			if s.Location != nil {
				fmt.Fprintf(&b, "  (%s:%d)", displayPath(uriToPath(s.Location.URI), cwd), s.Location.Range.Start.Line+1)
			}
			b.WriteByte('\n')
			walk(s.Children, depth+1)
		}
	}
	walk(syms, 0)
	return strings.TrimRight(b.String(), "\n")
}

// workspaceSymbols renders SymbolInformation[] from workspace/symbol.
func workspaceSymbols(raw json.RawMessage, cwd string) string {
	var syms []struct {
		Name     string    `json:"name"`
		Kind     int       `json:"kind"`
		Location *location `json:"location"`
	}
	if err := json.Unmarshal(raw, &syms); err != nil {
		return ""
	}
	var b strings.Builder
	for _, s := range syms {
		loc := ""
		if s.Location != nil {
			loc = fmt.Sprintf("  %s:%d:%d", displayPath(uriToPath(s.Location.URI), cwd),
				s.Location.Range.Start.Line+1, s.Location.Range.Start.Character+1)
		}
		fmt.Fprintf(&b, "%s %s%s\n", symbolKind(s.Kind), s.Name, loc)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderDiagnostics(b *strings.Builder, path string, ds []Diagnostic) {
	for _, d := range ds {
		src := ""
		if d.Source != "" {
			src = " (" + d.Source + ")"
		}
		code := ""
		if c, ok := d.Code.(string); ok && c != "" {
			code = " [" + c + "]"
		} else if c, ok := d.Code.(float64); ok {
			code = fmt.Sprintf(" [%g]", c)
		}
		fmt.Fprintf(b, "%s:%d:%d: %s: %s%s%s\n", path, d.Range.Start.Line+1, d.Range.Start.Character+1,
			severityName(d.Severity), strings.TrimSpace(d.Message), code, src)
	}
}

func severityName(s int) string {
	switch s {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "information"
	case 4:
		return "hint"
	default:
		return "diagnostic"
	}
}

// symbolKind names the LSP SymbolKind enum.
func symbolKind(k int) string {
	kinds := []string{"", "File", "Module", "Namespace", "Package", "Class", "Method", "Property",
		"Field", "Constructor", "Enum", "Interface", "Function", "Variable", "Constant", "String",
		"Number", "Boolean", "Array", "Object", "Key", "Null", "EnumMember", "Struct", "Event",
		"Operator", "TypeParameter"}
	if k > 0 && k < len(kinds) {
		return kinds[k]
	}
	return "Symbol"
}

// capSites renders sites with the cap and an explicit truncation marker.
func capSites(sites []site, cwd string) string {
	total := len(sites)
	if total > MaxSites {
		sites = sites[:MaxSites]
	}
	var b strings.Builder
	for _, s := range sites {
		fmt.Fprintf(&b, "%s:%d:%d\n", displayPath(s.Path, cwd), s.Line, s.Col)
	}
	if total > MaxSites {
		fmt.Fprintf(&b, "… +%d more sites (capped at %d)\n", total-MaxSites, MaxSites)
	}
	return strings.TrimRight(b.String(), "\n")
}

// capText bounds one text payload; the marker says the report is incomplete
// rather than silently short.
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n… truncated at %d bytes", limit)
}

// displayPath renders path relative to cwd when it is below it.
func displayPath(path, cwd string) string {
	if cwd == "" {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// resolvePosition turns the tool's {line, col, symbol} into a 0-based LSP
// position. A symbol ("name", or "name#2" for the second occurrence) is
// resolved against the file text so the caller need not count columns; a bare
// line snaps to its first non-space character.
func resolvePosition(path string, line, col int, symbol string) (Position, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return Position{}, err
	}
	lines := strings.Split(string(src), "\n")
	if symbol != "" {
		l, c, ok := symbolOffset(lines, symbol, line)
		if !ok {
			return Position{}, fmt.Errorf("symbol %q not found in %s", symbol, filepath.Base(path))
		}
		return Position{Line: l, Character: utf16Len(lines[l][:c])}, nil
	}
	if line < 1 {
		return Position{}, errors.New("line (1-based) is required when no symbol is given")
	}
	if line > len(lines) {
		return Position{}, fmt.Errorf("line %d is past the end of %s (%d lines)", line, filepath.Base(path), len(lines))
	}
	text := lines[line-1]
	c := col
	if c < 1 {
		c = 1
		for c <= len(text) && (text[c-1] == ' ' || text[c-1] == '\t') {
			c++
		}
	}
	if c > len(text)+1 {
		return Position{}, fmt.Errorf("col %d is past the end of line %d (%d columns)", col, line, len(text))
	}
	return Position{Line: line - 1, Character: utf16Len(text[:c-1])}, nil
}

// symbolOffset finds the nth whole-word occurrence of a symbol, preferring
// occurrences on the given (1-based) line when one is supplied. Occurrences
// inside comments are skipped: a doc comment naming the symbol would
// otherwise win and send the server a position with no identifier under it
// (gopls answers "no identifier found" there).
func symbolOffset(lines []string, symbol string, line int) (int, int, bool) {
	name, nth := symbol, 1
	if i := strings.LastIndex(symbol, "#"); i > 0 {
		if n, err := strconv.Atoi(symbol[i+1:]); err == nil && n > 0 {
			name, nth = symbol[:i], n
		}
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(name) + `\b`)
	if err != nil {
		return 0, 0, false
	}
	type hit struct{ line, col int }
	var onLine, code []hit
	for i, text := range lines {
		for _, m := range re.FindAllStringIndex(text, -1) {
			h := hit{i, m[0]}
			if line > 0 && i == line-1 {
				// An explicit line is the user pointing at it: no filtering.
				onLine = append(onLine, h)
				continue
			}
			if commentMasked(text, m[0]) {
				continue
			}
			code = append(code, h)
		}
	}
	pick := code
	if len(onLine) > 0 {
		pick = onLine
	}
	if len(pick) < nth {
		return 0, 0, false
	}
	return pick[nth-1].line, pick[nth-1].col, true
}

// commentMasked reports whether a byte offset on a line looks like it is
// inside a comment: the line itself is a comment (// # * /* --) or a //
// or # marker precedes the offset.
//
// ponytail: naive sniffing, no string-literal awareness (a rendered "//" in
// a string hides the rest of the line). Ceiling: rare false negatives;
// upgrade path is a per-language lexer if the lsp tool grows edit actions.
func commentMasked(text string, off int) bool {
	trimmed := strings.TrimLeft(text, " \t")
	for _, lead := range []string{"//", "/*", "*", "#", "--"} {
		if strings.HasPrefix(trimmed, lead) {
			return true
		}
	}
	for _, marker := range []string{"//", "#"} {
		if i := strings.Index(text, marker); i >= 0 && i < off {
			return true
		}
	}
	return false
}

// utf16Len counts UTF-16 code units (the LSP character unit).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// codeAction is one entry of a textDocument/codeAction response. A response
// item is either a Command (no edit) or a CodeAction with an edit / edit
// reference; both spellings are decoded.
type codeAction struct {
	Title string        `json:"title"`
	Kind  string        `json:"kind"`
	Edit  workspaceEdit `json:"edit"`
	// Command is overloaded by the spec: a Command object's `command` is a
	// string, a CodeAction's `command` is an object. RawMessage absorbs both,
	// and commandName() reads whichever arrived.
	Command json.RawMessage `json:"command"`
}

// commandName renders the command field whatever shape the server used.
func (c codeAction) commandName() string {
	if len(c.Command) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(c.Command, &s); err == nil && s != "" {
		return s
	}
	var obj struct {
		Command string `json:"command"`
		Title   string `json:"title"`
	}
	if json.Unmarshal(c.Command, &obj) == nil {
		if obj.Command != "" {
			return obj.Command
		}
		return obj.Title
	}
	return ""
}

// editFiles lists the document URIs an action touches, absolute-path form.
func (c codeAction) editFiles() []string {
	if len(c.Edit.Changes) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.Edit.Changes))
	for uri := range c.Edit.Changes {
		out = append(out, pathFromURI(uri))
	}
	sort.Strings(out)
	return out
}

// workspaceEdit is the LSP shape both rename and code actions return: either
// `changes` keyed by document URI, or the document-diagram `documentChanges`
// form. Only `changes` is decoded here; the diagram form is reported as a
// count rather than pretending to understand annotations.
type workspaceEdit struct {
	Changes         map[string][]textEdit `json:"changes"`
	DocumentChanges []struct {
		Edits        []textEdit `json:"edits"`
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	} `json:"documentChanges"`
}

type textEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// parseCodeActions decodes a textDocument/codeAction response. The spec
// allows an array of CodeAction objects OR an array of Commands, and a Command
// decodes cleanly into the CodeAction shape (its `command` is a string), so
// the two are told apart by whether a kind/edit is present — a bare Command is
// labelled as such instead of showing up as a nameless, edit-less action.
func parseCodeActions(raw json.RawMessage) ([]codeAction, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var actions []codeAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, fmt.Errorf("unrecognized codeAction response: %s", capText(string(raw), 200))
	}
	for i := range actions {
		if actions[i].Kind == "" && len(actions[i].Edit.Changes) == 0 {
			if name := actions[i].commandName(); name != "" {
				actions[i].Kind = "command:" + name
			}
		}
	}
	return actions, nil
}

// renderWorkspaceEdit describes an edit plan: one line per document with its
// edit count and the first affected line, so a model can carry the change out
// with the edit tool without guessing.
func renderWorkspaceEdit(raw json.RawMessage, cwd string) string {
	if len(raw) == 0 {
		return ""
	}
	var ed workspaceEdit
	if err := json.Unmarshal(raw, &ed); err != nil {
		return capText(string(raw), 400)
	}
	var b strings.Builder
	for _, uri := range sortedChangeKeys(ed) {
		edits := ed.Changes[uri]
		file := pathFromURI(uri)
		if rel, err := filepath.Rel(cwd, file); err == nil && !strings.HasPrefix(rel, "..") {
			file = rel
		}
		fmt.Fprintf(&b, "  %s: %d edit(s)", file, len(edits))
		if len(edits) > 0 {
			fmt.Fprintf(&b, " (first at line %d)", edits[0].Range.Start.Line+1)
		}
		b.WriteString("\n")
	}
	for _, dc := range ed.DocumentChanges {
		fmt.Fprintf(&b, "  %s: %d edit(s) (documentChanges)\n",
			pathFromURI(dc.TextDocument.URI), len(dc.Edits))
	}
	return b.String()
}

// sortedChangeKeys keeps the rendered plan stable across servers (map order
// is random, and a diff-like report must not reorder between runs).
func sortedChangeKeys(ed workspaceEdit) []string {
	out := make([]string, 0, len(ed.Changes))
	for uri := range ed.Changes {
		out = append(out, uri)
	}
	sort.Strings(out)
	return out
}
