package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// AST tooling (M7 #8, PRD §3.6): shell out to the `ast-grep` binary when
// present — never embed tree-sitter (CGO + memory). Both tools degrade to
// a clear "not installed" error rather than pretending to work.
//
// Output is decoded from NDJSON (`--json=stream`) and capped, so a pattern
// matching ten thousand nodes cannot balloon memory (PRD §3.6: bounded
// everything).

// astGrepBinary resolves the CLI, swappable for tests.
//
// Only `ast-grep` is probed. The `sg` alias is NOT used: on Debian/Ubuntu
// /usr/bin/sg belongs to a different package entirely, so falling back to
// it ran the wrong binary (CI caught it rejecting -p). ast-grep's own
// releases install both names, so probing `ast-grep` loses nothing real.
var astGrepBinary = func() (string, error) { return lookPath("ast-grep") }

// DefaultASTMaxMatches caps one AST search/rewrite report.
const DefaultASTMaxMatches = 200

// astArgs is the shared argument shape of both AST tools.
type astArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Lang       string `json:"lang"`
	Skip       int    `json:"skip"`
	MaxMatches int    `json:"max_matches"`
	Rewrite    string `json:"rewrite"`
	Apply      bool   `json:"apply"`
}

// astMatch is one streamed ast-grep result (the subset we surface).
type astMatch struct {
	Text        string `json:"text"`
	Lines       string `json:"lines"`
	File        string `json:"file"`
	Replacement string `json:"replacement"`
	Range       struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
	} `json:"range"`
}

func astRoot(path, cwd string) string {
	if path == "" {
		path = cwd
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return path
}

// ASTGrepTool searches code by AST pattern.
type ASTGrepTool struct {
	CWD        string
	MaxMatches int
}

func (t *ASTGrepTool) Name() string { return "ast_grep" }

func (t *ASTGrepTool) Description() string {
	return "structural code search via ast-grep: matches syntax, not text. $VAR captures one node, $$$VAR captures zero or more; separate calls for unrelated patterns"
}

func (t *ASTGrepTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "AST pattern, e.g. \"func $NAME($$$ARGS) { $$$BODY }\""},
    "path": {"type": "string", "description": "file or directory to search (default: cwd)"},
    "lang": {"type": "string", "description": "language override when extension inference is ambiguous (e.g. cpp for .h)"},
    "skip": {"type": "integer", "description": "matches to skip before reporting"}
  },
  "required": ["pattern"]
}`)
}

func (t *ASTGrepTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a astArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: "ast_grep: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return Result{Text: "ast_grep: pattern is required", IsError: true}, nil
	}
	bin, err := astGrepBinary()
	if err != nil {
		return Result{Text: "ast_grep: ast-grep binary not found (xdev shells out; it never embeds tree-sitter)", IsError: true}, nil
	}
	argv := []string{"run", "-p", a.Pattern, "--json=stream", "--color=never"}
	if a.Lang != "" {
		argv = append(argv, "-l", a.Lang)
	}
	argv = append(argv, astRoot(a.Path, t.CWD))
	limit := a.MaxMatches
	if limit <= 0 {
		limit = t.MaxMatches
	}
	if limit <= 0 {
		limit = DefaultASTMaxMatches
	}
	matches, total, capped, err := runASTStream(ctx, bin, t.CWD, a.Pattern, argv, a.Skip, limit)
	if err != nil {
		return Result{Text: "ast_grep: " + err.Error(), IsError: true}, nil
	}
	if total == 0 {
		return Result{Text: "no matches"}, nil
	}
	text := renderASTMatches(matches, t.CWD)
	if capped {
		text += "\n[more matches suppressed]"
	}
	return Result{Text: text, Details: map[string]any{"matches": total, "capped": capped}}, nil
}

// ASTEditTool rewrites code by AST pattern, staged by default: it reports
// what would change unless apply is true.
type ASTEditTool struct {
	CWD        string
	MaxMatches int
}

func (t *ASTEditTool) Name() string { return "ast_edit" }

func (t *ASTEditTool) Description() string {
	return "structural AST-aware rewrite via ast-grep: pattern → replacement template. Matches are staged (reported, not written) unless apply=true; prefer the edit tool for plain text edits"
}

func (t *ASTEditTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "AST pattern to match"},
    "rewrite": {"type": "string", "description": "replacement template; metavariables in the pattern substitute in"},
    "paths": {"type": "array", "items": {"type": "string"}, "description": "files or directories to rewrite (default: cwd)"},
    "lang": {"type": "string", "description": "language override"},
    "apply": {"type": "boolean", "description": "write the rewrites in place (default false = report only)"}
  },
  "required": ["pattern", "rewrite"]
}`)
}

func (t *ASTEditTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a astArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: "ast_edit: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return Result{Text: "ast_edit: pattern is required", IsError: true}, nil
	}
	bin, err := astGrepBinary()
	if err != nil {
		return Result{Text: "ast_edit: ast-grep binary not found (xdev shells out; it never embeds tree-sitter)", IsError: true}, nil
	}
	targets := astPaths(args, t.CWD)
	argv := []string{"run", "-p", a.Pattern, "-r", a.Rewrite, "--color=never"}
	if a.Lang != "" {
		argv = append(argv, "-l", a.Lang)
	}
	limit := a.MaxMatches
	if limit <= 0 {
		limit = t.MaxMatches
	}
	if limit <= 0 {
		limit = DefaultASTMaxMatches
	}

	// `--json` silently overrides `-U` (ast-grep prints the preview and
	// writes nothing), so the two modes must build separate argv.
	if !a.Apply {
		preview := append(append([]string(nil), argv...), "--json=stream")
		preview = append(preview, targets...)
		matches, total, capped, err := runASTStream(ctx, bin, t.CWD, a.Pattern, preview, 0, limit)
		if err != nil {
			return Result{Text: "ast_edit: " + err.Error(), IsError: true}, nil
		}
		if total == 0 {
			return Result{Text: "no matches"}, nil
		}
		text := renderASTRewrites(matches, t.CWD)
		if capped {
			text += "\n[more matches suppressed]"
		}
		return Result{Text: text + "\n\nstaged — re-run with apply=true to write", Details: map[string]any{"matches": total}}, nil
	}

	// Apply in place (-U, no --json), then invalidate every touched root.
	applyArgv := append(append([]string(nil), argv...), "-U")
	applyArgv = append(applyArgv, targets...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, applyArgv...)
	cmd.Dir = t.CWD
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return Result{Text: "ast_edit: " + msg, IsError: true}, nil
	}
	for _, p := range targets {
		SharedFSCache().Invalidate(p)
	}
	summary := strings.TrimSpace(stdout.String()) // e.g. "Applied 1 changes"
	if summary == "" {
		summary = "applied rewrite to " + strings.Join(relList(targets, t.CWD), ", ")
	}
	return Result{Text: summary, Details: map[string]any{"applied": true}}, nil
}

// astPaths resolves the paths argument (default: cwd).
func astPaths(args json.RawMessage, cwd string) []string {
	var v struct {
		Paths []string `json:"paths"`
	}
	_ = json.Unmarshal(args, &v)
	if len(v.Paths) == 0 {
		return []string{cwd}
	}
	out := make([]string, 0, len(v.Paths))
	for _, p := range v.Paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		out = append(out, p)
	}
	return out
}

func relList(paths []string, cwd string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, relDisplay(p, cwd))
	}
	return out
}

// patternBalanced is a cheap syntactic plausibility check for an ast-grep
// pattern: brackets and quotes balance outside string literals. ast-grep
// exits 1 with EMPTY stdout and stderr for a pattern that does not parse
// (verified against 0.45.3), which is byte-for-byte its "no matches" shape —
// without this check a typo'd pattern reports absence and the model
// refactor-deletes live code against a query that never ran.
// ponytail: syntactic balance, not a tree-sitter parse — a valid pattern
// with an odd quote count would be mis-flagged as a parse issue; upgrade
// path is a tree-sitter validation pass in ast-grep itself.
func patternBalanced(p string) bool {
	var stack []rune
	inStr := false
	escaped := false
	for _, r := range p {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inStr = !inStr
		case inStr:
			// inside a literal: brackets do not count
		case r == '(' || r == '[' || r == '{':
			stack = append(stack, r)
		case r == ')' || r == ']' || r == '}':
			if len(stack) == 0 {
				return false
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if (r == ')' && open != '(') || (r == ']' && open != '[') || (r == '}' && open != '{') {
				return false
			}
		}
	}
	return !inStr && len(stack) == 0
}

// runASTStream decodes at most `limit` match lines after skipping `skip`,
// stopping early (and killing the child) once the cap is reached.
func runASTStream(ctx context.Context, bin, cwd, pattern string, argv []string, skip, limit int) (matches []astMatch, total int, capped bool, err error) {
	kctx, kill := context.WithCancel(ctx)
	defer kill()
	cmd := exec.CommandContext(kctx, bin, argv...)
	cmd.Dir = cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, perr := cmd.StdoutPipe()
	if perr != nil {
		return nil, 0, false, perr
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, false, err
	}
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 256<<10), 8<<20) // one match line can be large
	for sc.Scan() {
		var m astMatch
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		total++
		if total <= skip {
			continue
		}
		if len(matches) >= limit {
			capped = true
			break
		}
		matches = append(matches, m)
	}
	waitErr := cmd.Wait() // kill() makes Wait return promptly after the break
	if waitErr != nil && total == 0 {
		// ast-grep also exits non-zero on "no matches"; only surface a real
		// failure (bad pattern, unreadable path) as an error.
		msg := strings.TrimSpace(stderr.String())
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) && ee.ExitCode() == 1 && msg == "" {
			// Exit 1 with no output is both "no matches" and "pattern does
			// not parse" — ast-grep cannot tell them apart. When the pattern
			// is syntactically implausible, say so instead of reporting
			// absence: a model that reads "no matches" for a broken query
			// acts on evidence that was never collected.
			if !patternBalanced(pattern) {
				return nil, 0, false, errors.New("ast-grep: pattern does not parse (unbalanced brackets or quotes) — fix the pattern or tighten path before concluding anything")
			}
			return nil, 0, false, nil
		}
		if msg == "" {
			msg = waitErr.Error()
		}
		return nil, 0, false, errors.New(msg)
	}
	return matches, total, capped, nil
}

// renderASTMatches formats search hits as file:line: text (ast-grep's
// lines are 0-based; the tool reports 1-based like grep).
func renderASTMatches(ms []astMatch, cwd string) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "%s:%d: %s\n", relDisplay(filepath.Clean(m.File), cwd), m.Range.Start.Line+1,
			truncateLine(strings.TrimSpace(m.Lines), 300))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderASTRewrites formats preview hits as before → after.
func renderASTRewrites(ms []astMatch, cwd string) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "%s:%d\n  - %s\n  + %s\n", relDisplay(filepath.Clean(m.File), cwd), m.Range.Start.Line+1,
			truncateLine(strings.TrimSpace(m.Text), 200), truncateLine(strings.TrimSpace(m.Replacement), 200))
	}
	return strings.TrimRight(b.String(), "\n")
}
