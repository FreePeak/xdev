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
    "op": {"type": "string", "enum": ["diagnostics", "definition", "references", "hover", "symbols"], "description": "query to run"},
    "file": {"type": "string", "description": "file path (relative to cwd); \"*\" for diagnostics = every file reported so far"},
    "line": {"type": "integer", "description": "1-based line of the position (omit when passing symbol)"},
    "col": {"type": "integer", "description": "1-based column (defaults to the first non-space character of the line)"},
    "symbol": {"type": "string", "description": "identifier to locate, e.g. \"resolveModel\" or \"resolveModel#2\" for the second occurrence; for op=symbols it becomes the workspace symbol query"},
    "timeout": {"type": "integer", "description": "per-action timeout in seconds (5..300, default 30)"}
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
	case "":
		return errResult("lsp: op is required (diagnostics|definition|references|hover|symbols)"), nil
	default:
		return errResult("lsp: unknown op " + a.Op + " (want diagnostics|definition|references|hover|symbols)"), nil
	}
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
