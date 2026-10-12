package testimpact

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// graph is the module's package graph, as `go list -e -json ./...` reports
// it. Edges are imports (production, internal test, external test), and the
// reverse closure over those edges is the affected set.
//
// `go list` is the right tool and not a re-implementation: it is the
// compiler's own answer about which packages depend on which, it honours
// build tags and file suffixes, and it costs ~0.5s for this repo's 39
// packages. A hand-rolled AST import scan would get all three wrong.
type graph struct {
	pkgs map[string]*pkgInfo
	// rev maps a package to the packages that import it (production or
	// test). The closure walks this backwards from the changed set.
	rev map[string]map[string]bool
	// dirToImport maps an absolute directory to its import path, so a
	// changed file path resolves to a package without string surgery on
	// module paths.
	dirToImport map[string]string
	// root is the repository root the diff paths are relative to
	// (`git rev-parse --show-toplevel`). It turns a relative diff path into
	// a package directory, and a package back into `./internal/tool` for
	// the suggested command.
	root string
}

// pkgInfo is the slice of `go list` output this package needs.
type pkgInfo struct {
	ImportPath   string
	Dir          string
	Name         string
	Imports      []string
	TestImports  []string
	XTestImports []string
	TestGoFiles  []string
	XTestGoFiles []string
	Standard     bool
	Goroot       bool
}

// loadGraph runs `go list -e -json ./...` once and indexes the result.
//
// -e matters: one package with a compile error must not empty the whole
// graph. A broken package is still a node with a Dir, which is exactly the
// state a session mid-edit is in.
func loadGraph(ctx context.Context, run runner, cwd string, timeout time.Duration) (*graph, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := run(cctx, cwd, "go", "list", "-e", "-json", "./...")
	if err != nil {
		return nil, fmt.Errorf("testimpact: go list: %w", err)
	}
	g := &graph{
		pkgs:        map[string]*pkgInfo{},
		rev:         map[string]map[string]bool{},
		dirToImport: map[string]string{},
	}
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var p pkgInfo
		err := dec.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("testimpact: decode go list output: %w", err)
		}
		if p.ImportPath == "" {
			continue
		}
		cp := p
		g.pkgs[p.ImportPath] = &cp
		if p.Dir != "" {
			if abs, aerr := filepath.Abs(p.Dir); aerr == nil {
				g.dirToImport[filepath.Clean(abs)] = p.ImportPath
			}
		}
	}
	for path, p := range g.pkgs {
		for _, dep := range allImports(p) {
			if _, ok := g.pkgs[dep]; !ok {
				continue // stdlib, or a module dep outside ./...
			}
			if g.rev[dep] == nil {
				g.rev[dep] = map[string]bool{}
			}
			g.rev[dep][path] = true
		}
	}
	g.root = repoRoot(ctx, run, cwd, timeout)
	return g, nil
}

// allImports is every import edge out of a package: production, internal
// test, and external test. The test edges are half the reason this works —
// a package's own tests are part of what a change to it affects.
func allImports(p *pkgInfo) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
		for _, dep := range list {
			if !seen[dep] {
				seen[dep] = true
				out = append(out, dep)
			}
		}
	}
	return out
}

// repoRoot asks git for the worktree root, so a diff path from any
// subdirectory resolves. A failure is not fatal: cwd is the fallback, and a
// session that runs the tool from the repo root (the common case) is
// unaffected.
func repoRoot(ctx context.Context, run runner, cwd string, timeout time.Duration) string {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if out, err := run(cctx, cwd, "git", "-C", cwd, "rev-parse", "--show-toplevel"); err == nil {
		if root := strings.TrimSpace(out); root != "" {
			if abs, aerr := filepath.Abs(root); aerr == nil {
				return filepath.Clean(abs)
			}
		}
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		return filepath.Clean(abs)
	}
	return cwd
}

// packagesForFiles maps changed file paths to import paths. A file outside
// every known package (markdown, a script, a file in a directory `./...`
// skips) contributes nothing: it has no Go test that depends on it, and
// inventing one would be a guess dressed as a fact.
func (g *graph) packagesForFiles(files []string) []string {
	set := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		dir := filepath.Dir(filepath.FromSlash(f))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(g.root, dir)
		}
		if imp, ok := g.dirToImport[filepath.Clean(dir)]; ok {
			set[imp] = true
		}
	}
	return sortedKeys(set)
}

// reverseClosure returns the changed packages plus every package that
// transitively imports one of them, sorted. It is the deterministic half of
// the answer: a test outside this set cannot be affected by a Go change in
// the only sense the compiler can prove.
func (g *graph) reverseClosure(changed []string) []string {
	seen := map[string]bool{}
	var stack []string
	for _, c := range changed {
		if !seen[c] {
			seen[c] = true
			stack = append(stack, c)
		}
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for up := range g.rev[n] {
			if !seen[up] {
				seen[up] = true
				stack = append(stack, up)
			}
		}
	}
	return sortedKeys(seen)
}

// candidates inventories the test functions in the closure, capped at max.
//
// The order before the cap is deliberate and stable: direct packages first,
// then by package path, then by test name. The cap is the only place this
// tool can silently lose an answer, so what it keeps must be the part most
// likely to matter — the tests in the package that was actually edited.
func (g *graph) candidates(closure, changed []string, max int) ([]Candidate, int) {
	direct := map[string]bool{}
	for _, c := range changed {
		direct[c] = true
	}
	order := append([]string(nil), closure...)
	sort.SliceStable(order, func(i, j int) bool {
		if direct[order[i]] != direct[order[j]] {
			return direct[order[i]]
		}
		return order[i] < order[j]
	})

	total := 0
	var out []Candidate
	for _, imp := range order {
		p := g.pkgs[imp]
		if p == nil {
			continue
		}
		got := testsIn(p)
		total += len(got)
		for _, c := range got {
			c.Direct = direct[imp]
			c.PkgDir = g.relDir(imp)
			if len(out) < max {
				out = append(out, c)
			}
		}
	}
	return out, total
}

// relDir renders a package's directory relative to the repository root, as
// the `./internal/tool` a `go test` invocation wants. A package outside the
// root (a module cache dep) keeps its import path, which is the honest
// answer for a path that has no relative form.
func (g *graph) relDir(imp string) string {
	p := g.pkgs[imp]
	if p == nil || p.Dir == "" {
		return imp
	}
	abs, err := filepath.Abs(p.Dir)
	if err != nil {
		return imp
	}
	rel, err := filepath.Rel(g.root, filepath.Clean(abs))
	if err != nil || strings.HasPrefix(rel, "..") {
		return imp
	}
	return filepath.ToSlash(rel)
}

// testFuncRe matches a test or fuzz function declaration. It gates the
// parser: a test file with no `func Test` line at all cannot contribute, and
// most files in a package are not test files with tests.
var testFuncRe = regexp.MustCompile(`(?m)^func (Test|Fuzz)[A-Za-z0-9_]*\(`)

// testsIn extracts every Test/Fuzz function from a package's test files.
//
// go/parser, not a regex over the file: the declaration list is exact (a
// `func TestFoo(` inside a string literal is not a test), and it yields the
// doc comment in the same walk — the one piece of semantic information the
// ranker has about a test beyond its name.
func testsIn(p *pkgInfo) []Candidate {
	var out []Candidate
	fset := token.NewFileSet()
	for _, list := range [][]string{p.TestGoFiles, p.XTestGoFiles} {
		for _, name := range list {
			path := filepath.Join(p.Dir, name)
			src, err := os.ReadFile(path)
			if err != nil || !testFuncRe.Match(src) {
				continue
			}
			file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution|parser.ParseComments)
			if err != nil {
				continue // a file mid-edit is not this tool's error
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Name == nil {
					continue
				}
				if !isTestName(fn.Name.Name) {
					continue
				}
				out = append(out, Candidate{
					Package: p.ImportPath,
					Name:    fn.Name.Name,
					File:    name,
					Doc:     docLine(fn.Doc),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// isTestName reports whether a name is one the `go test` runner would run:
// Test/Fuzz followed by a non-lowercase rune. `TestHelper` IS run by go test
// (the runner does not know it is a helper), so it is included; `testFoo`
// and a bare `Test` are not.
func isTestName(name string) bool {
	for _, prefix := range []string{"Test", "Fuzz"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[len(prefix):]
		if rest == "" {
			return false
		}
		c := rest[0]
		return !(c >= 'a' && c <= 'z')
	}
	return false
}

// docLine collapses a test's leading comment block to one line, capped. It is
// the criterion text the ranker reads, so a whole paragraph would be prompt
// budget spent on one candidate.
func docLine(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	var parts []string
	for _, c := range doc.List {
		t := strings.TrimPrefix(c.Text, "//")
		t = strings.TrimPrefix(t, "/*")
		t = strings.TrimSuffix(t, "*/")
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		parts = append(parts, t)
	}
	line := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if r := []rune(line); len(r) > maxCriterionRunes {
		line = string(r[:maxCriterionRunes-1]) + "…"
	}
	return line
}

// sortedKeys returns the map's keys, sorted.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
