package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/FreePeak/xdev/internal/fscache"
)

// grep/glob (M7 #8, PRD §3.6): shell out to rg/fd when present (pi's own
// answer — the fast path), with a pure-Go fallback over the shared
// FS-scan cache so results feel instant and no extra dependency lands.
// The cache is process-local and shared by every caller.

var (
	sharedCacheOnce sync.Once
	sharedCache     *fscache.Cache
)

// SharedFSCache returns the process-wide scan cache (also used by the
// editor's fuzzy path completion — one cache, one truth).
func SharedFSCache() *fscache.Cache {
	sharedCacheOnce.Do(func() { sharedCache = fscache.New() })
	return sharedCache
}

// lookPath is swappable for tests.
var lookPath = exec.LookPath

// GrepTool searches file contents.
type GrepTool struct {
	CWD string
	// MaxMatches caps returned matches (0 → DefaultGrepMaxMatches).
	MaxMatches int
}

// DefaultGrepMaxMatches bounds a grep result so the context stays small.
const DefaultGrepMaxMatches = 200

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Description() string {
	return "search file contents with a regular expression; returns file:line: text matches"
}

func (t *GrepTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "regular expression to search for"},
    "path": {"type": "string", "description": "file or directory to search (default: cwd)"},
    "glob": {"type": "string", "description": "restrict to files matching this name pattern, e.g. *.go"},
    "ignore_case": {"type": "boolean", "description": "case-insensitive match"},
    "max_matches": {"type": "integer", "description": "cap on returned matches"}
  },
  "required": ["pattern"]
}`)
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	MaxMatches int    `json:"max_matches"`
}

func (t *GrepTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a grepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: "grep: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if a.Pattern == "" {
		return Result{Text: "grep: pattern is required", IsError: true}, nil
	}
	limit := a.MaxMatches
	if limit <= 0 {
		limit = t.MaxMatches
	}
	if limit <= 0 {
		limit = DefaultGrepMaxMatches
	}
	root := a.Path
	if root == "" {
		root = t.CWD
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(t.CWD, root)
	}

	// Fast path: ripgrep.
	if rg, err := lookPath("rg"); err == nil {
		if res, ok := t.runRG(ctx, rg, root, a, limit); ok {
			return res, nil
		}
	}
	return t.runGo(ctx, root, a, limit)
}

// runRG shells out to ripgrep; ok=false falls back to the Go path.
// Match counting is GLOBAL (same rule as the fallback): no --max-count,
// which would cap each file at one match and make the two paths disagree
// on how many lines a file contributes.
func (t *GrepTool) runRG(ctx context.Context, rg, root string, a grepArgs, limit int) (Result, bool) {
	argv := []string{"--line-number", "--no-heading", "--color=never"}
	if a.IgnoreCase {
		argv = append(argv, "--ignore-case")
	}
	if a.Glob != "" {
		argv = append(argv, "--glob", a.Glob)
	}
	argv = append(argv, "--", a.Pattern, root)
	// Killing on context cancellation stops the child; the sink bounds what
	// we accumulate, and stopRG ends the process once the limit is reached.
	kctx, stopRG := context.WithCancel(ctx)
	defer stopRG()
	cmd := exec.CommandContext(kctx, rg, argv...)
	cmd.Dir = t.CWD
	pipe, perr := cmd.StdoutPipe()
	if perr != nil {
		stopRG()
		return Result{}, false
	}
	if err := cmd.Start(); err != nil {
		stopRG()
		return Result{}, false
	}
	// Fixed-size window over the raw stream: a repo-wide grep for a common
	// token cannot grow memory, and the sink's own marker records what was
	// dropped (PRD §3.6 — bounded everything).
	sink := DefaultOutputSink()
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	matches := 0
	for sc.Scan() {
		matches++
		if _, werr := sink.Write(append(sc.Bytes(), '\n')); werr != nil {
			break
		}
		if matches >= limit {
			break // bounded: stop reading and let the deferred cancel kill rg
		}
	}
	readDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(readDone) }()
	<-readDone

	if matches == 0 {
		// Exit 1 is ripgrep's "no matches" answer; anything else (missing
		// binary, bad pattern) falls back to the Go path.
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() <= 1 {
			return Result{Text: "no matches"}, true
		}
		return Result{}, false
	}
	text, _ := sink.Result() // the sink marks its own truncation inline
	return renderGrep(text, root, t.CWD, limit, "rg"), true
}

// runGo is the pure-Go fallback: the FS-scan cache for the file list, then
// a line scan.
func (t *GrepTool) runGo(ctx context.Context, root string, a grepArgs, limit int) (Result, error) {
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Result{Text: "grep: bad pattern: " + err.Error(), IsError: true}, nil
	}

	roots := []string{root}
	files, _, _ := SharedFSCache().Scan(fscache.Options{Roots: roots, RespectGitignore: true})
	var b strings.Builder
	matches := 0
	scan := func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			if !re.Match(sc.Bytes()) {
				continue
			}
			matches++
			if matches > limit {
				return errStopScan
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", relDisplay(path, t.CWD), lineNo, truncateLine(sc.Text(), 400))
		}
		return sc.Err()
	}

	if len(files) == 0 {
		// root may be a single file, or empty because it vanished
		if fi, err := os.Stat(root); err == nil && !fi.IsDir() {
			files = []fscache.Entry{{Path: root}}
		}
	}
	for _, e := range files {
		select {
		case <-ctx.Done():
			return Result{Text: "grep: canceled", IsError: true}, nil
		default:
		}
		if e.IsDir {
			continue
		}
		if a.Glob != "" && !matchGlob(a.Glob, filepath.Base(e.Path)) {
			continue
		}
		if err := scan(e.Path); err == errStopScan {
			break
		}
	}
	if matches == 0 {
		return Result{Text: "no matches"}, nil
	}
	text := strings.TrimRight(b.String(), "\n")
	note := ""
	if matches > limit {
		note = fmt.Sprintf("\n[showing first %d matches]", limit)
	}
	return Result{Text: text + note, Details: map[string]any{"matches": matches}}, nil
}

var errStopScan = fmt.Errorf("grep: stop")

// renderGrep formats rg output, capping the line count. Paths are rewritten
// relative to cwd so the rg and pure-Go paths return byte-identical
// answers (rg echoes the search root back as an absolute prefix).
func renderGrep(out, root, cwd string, limit int, toolName string) Result {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return Result{Text: "no matches"}
	}
	truncated := false
	if len(lines) > limit {
		lines, truncated = lines[:limit], true
	}
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(truncateLine(rebaseGrepLine(ln, cwd), 400))
		b.WriteByte('\n')
	}
	text := strings.TrimRight(b.String(), "\n")
	if truncated {
		text += fmt.Sprintf("\n[showing first %d matches]", limit)
	}
	return Result{Text: text, Details: map[string]any{"tool": toolName}}
}

// grepLineRe matches ripgrep's `path:line:text` record. The path is
// non-greedy so a colon inside it cannot swallow the line number.
var grepLineRe = regexp.MustCompile(`^(.*?):(\d+):(.*)$`)

// rebaseGrepLine renders one rg record in the Go fallback's exact shape:
// `path:line: text`, with the path relative to cwd. Without the space
// normalization the model would see a different grep format depending on
// whether rg happens to be installed — the same divergence class as the
// per-file cap that --max-count used to introduce.
func rebaseGrepLine(line, cwd string) string {
	m := grepLineRe.FindStringSubmatch(line)
	if m == nil {
		return line // a continuation/aggregate line: pass through
	}
	return relDisplay(filepath.Clean(m[1]), cwd) + ":" + m[2] + ": " + m[3]
}

func truncateLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func relDisplay(path, cwd string) string {
	if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// GlobTool lists files by name pattern.
type GlobTool struct {
	CWD string
	// MaxResults caps returned paths (0 → DefaultGlobMaxResults).
	MaxResults int
}

// DefaultGlobMaxResults bounds a glob listing.
const DefaultGlobMaxResults = 300

func (t *GlobTool) Name() string { return "glob" }

func (t *GlobTool) Description() string {
	return "find files by name pattern (e.g. **/*.go); returns paths sorted by modification time"
}

func (t *GlobTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "name pattern; ** crosses directories, e.g. **/*_test.go"},
    "path": {"type": "string", "description": "directory to search (default: cwd)"},
    "max_results": {"type": "integer", "description": "cap on returned paths"}
  },
  "required": ["pattern"]
}`)
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

func (t *GlobTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a globArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{Text: "glob: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if a.Pattern == "" {
		return Result{Text: "glob: pattern is required", IsError: true}, nil
	}
	limit := a.MaxResults
	if limit <= 0 {
		limit = t.MaxResults
	}
	if limit <= 0 {
		limit = DefaultGlobMaxResults
	}
	root := a.Path
	if root == "" {
		root = t.CWD
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(t.CWD, root)
	}

	// Fast path: fd. Debian/Ubuntu ship the package as `fdfind`, so try
	// both names before falling back to the cached walk.
	if fd, err := lookPathFD(); err == nil {
		if res, ok := t.runFD(ctx, fd, root, a.Pattern, limit); ok {
			return res, nil
		}
	}

	entries, truncated, _ := SharedFSCache().Scan(fscache.Options{Roots: []string{root}, RespectGitignore: true})
	type hit struct {
		path string
		mod  int64
	}
	var hits []hit
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		rel, err := filepath.Rel(root, e.Path)
		if err != nil {
			continue
		}
		if !matchGlob(a.Pattern, filepath.ToSlash(rel)) {
			continue
		}
		hits = append(hits, hit{path: rel, mod: e.ModTime.UnixNano()})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })
	if len(hits) == 0 {
		return Result{Text: "no files matched"}, nil
	}
	capped := false
	if len(hits) > limit {
		hits, capped = hits[:limit], true
	}
	var b strings.Builder
	for _, h := range hits {
		b.WriteString(h.path)
		b.WriteByte('\n')
	}
	text := strings.TrimRight(b.String(), "\n")
	if capped || truncated {
		text += fmt.Sprintf("\n[showing %d files]", len(hits))
	}
	return Result{Text: text, Details: map[string]any{"files": len(hits)}}, nil
}

// runFD shells out to fd; ok=false falls back to the cache path.
func (t *GlobTool) runFD(ctx context.Context, fd, root, pattern string, limit int) (Result, bool) {
	argv := []string{"--color=never", "--type", "f", "--max-results", fmt.Sprint(limit)}
	// fd's pattern is a regex over the basename unless -g; use -g for
	// glob semantics so `**/*.go` behaves like the Go fallback.
	argv = append(argv, "-g", pattern, root)
	cmd := exec.CommandContext(ctx, fd, argv...)
	cmd.Dir = t.CWD
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			return Result{}, false
		}
		if out.Len() == 0 {
			return Result{Text: "no files matched"}, true
		}
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	var b strings.Builder
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		b.WriteString(relDisplay(ln, t.CWD))
		b.WriteByte('\n')
	}
	text := strings.TrimRight(b.String(), "\n")
	if text == "" {
		return Result{Text: "no files matched"}, true
	}
	return Result{Text: text, Details: map[string]any{"tool": "fd"}}, true
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// lookPathFD resolves fd under either name (fd on arch/homebrew, fdfind on
// Debian/Ubuntu packaging).
func lookPathFD() (string, error) {
	if p, err := lookPath("fd"); err == nil {
		return p, nil
	}
	return lookPath("fdfind")
}

// matchGlob matches a slash path against a glob supporting `**`. The
// stdlib alone cannot express `**` (path.Match has no recursive form), so
// this is the smallest correct expansion: split on `**`, match segments.
func matchGlob(pattern, path string) bool {
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)
	if !strings.Contains(pattern, "**") {
		// No recursion: match the basename or the full relative path.
		ok, err := filepath.Match(pattern, path)
		if err == nil && ok {
			return true
		}
		ok, err = filepath.Match(pattern, filepath.Base(path))
		return err == nil && ok
	}
	parts := strings.Split(pattern, "**")
	// Leading segment must match a prefix of the path.
	if head := strings.TrimSuffix(parts[0], "/"); head != "" {
		if !strings.HasPrefix(path, head+"/") {
			return false
		}
		path = strings.TrimPrefix(path, head+"/")
	}
	for i := 1; i < len(parts); i++ {
		seg := strings.Trim(parts[i], "/")
		if seg == "" {
			continue
		}
		// Find the earliest position where the segment matches the tail
		// (supporting `*.go` against any depth, or a literal dir).
		found := false
		for j := 0; j < len(path); j++ {
			if path[j] != '/' && j != 0 {
				continue
			}
			rest := path[j:]
			rest = strings.TrimPrefix(rest, "/")
			if segMatches(seg, rest) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// segMatches reports whether seg matches the rest of the path, where seg
// may itself be a multi-segment pattern ending in a filename glob.
func segMatches(seg, rest string) bool {
	if seg == rest {
		return true
	}
	if ok, err := filepath.Match(seg, filepath.Base(rest)); err == nil && ok {
		return true
	}
	// Multi-segment tail, e.g. "cmd/*.go": match the suffix.
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			if ok, err := filepath.Match(seg, rest[i+1:]); err == nil && ok {
				return true
			}
			if ok, err := filepath.Match(seg, filepath.Base(rest[i+1:])); err == nil && ok {
				return true
			}
		}
	}
	return false
}
