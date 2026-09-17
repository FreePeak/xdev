package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestPathTokenDetectsMentions(t *testing.T) {
	tests := []struct {
		text          string
		prefix, query string
		ok            bool
	}{
		{"fix @", "fix ", "", true},
		{"fix @cache", "fix ", "cache", true},
		{"@a.go", "", "a.go", true},
		{"email me at x@y.com", "", "", false}, // mid-word @ is prose, not a path
		{"read @a.go then @b", "read @a.go then ", "b", true},
		{"@done and more", "", "", false}, // token already closed by a space
		{"no mention here", "", "", false},
		// Quoted mentions carry a path with spaces (omp's `@"…"` form).
		{`@"my file.go`, "", "my file.go", true},
		{`fix @"a b/c`, "fix ", "a b/c", true},
		{`@"my file.go" and more`, "", "", false}, // the closing quote ended it
		{"@internal/tu", "", "internal/tu", true},
	}
	for _, tc := range tests {
		prefix, query, ok := pathToken(tc.text)
		if ok != tc.ok || (ok && (prefix != tc.prefix || query != tc.query)) {
			t.Errorf("pathToken(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.text, prefix, query, ok, tc.prefix, tc.query, tc.ok)
		}
	}
}

// TestSplitPathQueryScopesToDirectory pins what the readdir is asked for: the
// segment before the last slash, with the trailing prefix to match inside it.
func TestSplitPathQueryScopesToDirectory(t *testing.T) {
	tests := []struct {
		in, dir, seg string
	}{
		{"cache", "", "cache"},
		{"internal/tu", "internal", "tu"},
		{"internal/", "internal", ""},
		{"a/b/c", "a/b", "c"},
		{"/internal", "", "internal"},
		{"/internal/", "internal", ""},
		{"", "", ""},
		{"../x", "..", "x"},
	}
	for _, tc := range tests {
		dir, seg := splitPathQuery(tc.in)
		if dir != tc.dir || seg != tc.seg {
			t.Errorf("splitPathQuery(%q) = (%q,%q), want (%q,%q)", tc.in, dir, seg, tc.dir, tc.seg)
		}
	}
}

// appWithTree wires a fake directory tree: each key is a directory, each value
// its raw entries in os.ReadDir's lexical order.
func appWithTree(tree map[string][]PathEntry) *App {
	a := &App{}
	a.SetPathCompletion("/proj", func(dir string) []PathEntry { return tree[dir] })
	return a
}

func suggNames(items []suggestion) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

// TestPathCandidatesScopeToOneDirectory is the point of the rewrite: a typed
// token costs ONE readdir of the named directory, and the root dir's entries
// never leak into a subdirectory's listing.
func TestPathCandidatesScopeToOneDirectory(t *testing.T) {
	tree := map[string][]PathEntry{
		"":         {{Name: "cache.go"}, {Name: "docs", IsDir: true}},
		"internal": {{Name: "cache.go"}, {Name: "readme.md"}, {Name: "tui", IsDir: true}},
	}
	a := appWithTree(tree)
	got := suggNames(a.pathCandidates("internal/cac"))
	if len(got) != 1 || got[0] != "cache.go" {
		t.Fatalf("internal/cac = %v, want [cache.go]", got)
	}
}

// TestPathCandidatesDirsFirstWithSlash pins the omp ordering that makes the
// menu drillable: directories come first and carry a trailing slash.
func TestPathCandidatesDirsFirstWithSlash(t *testing.T) {
	a := appWithTree(map[string][]PathEntry{
		"": {{Name: "deep", IsDir: true}, {Name: "dockerfile"}, {Name: "docs", IsDir: true}},
	})
	got := suggNames(a.pathCandidates("d"))
	want := []string{"deep/", "docs/", "dockerfile"}
	if len(got) != len(want) {
		t.Fatalf("d = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("d = %v, want %v", got, want)
		}
	}
	for _, it := range a.pathCandidates("d") {
		if it.kind != kindPath || it.Tag != "path" {
			t.Fatalf("item not tagged as a path: %+v", it)
		}
	}
}

// TestPathCandidatesPrefixOnly: matching is a case-insensitive prefix, not a
// subsequence — `@ca` must not offer README.md.
func TestPathCandidatesPrefixOnly(t *testing.T) {
	a := appWithTree(map[string][]PathEntry{
		"": {{Name: "CATALOG.md"}, {Name: "README.md"}, {Name: "cache.go"}},
	})
	got := suggNames(a.pathCandidates("ca"))
	if len(got) != 2 || got[0] != "CATALOG.md" || got[1] != "cache.go" {
		t.Fatalf("ca = %v, want [CATALOG.md cache.go]", got)
	}
}

// TestPathCandidatesHidesOnlyVCS: a hidden, gitignored or vendored directory
// IS offered (that is the ask) — only .git and friends never are.
func TestPathCandidatesHidesOnlyVCS(t *testing.T) {
	a := appWithTree(map[string][]PathEntry{
		"": {{Name: ".env"}, {Name: ".git", IsDir: true}, {Name: ".gitignore"},
			{Name: ".hidden", IsDir: true}, {Name: "node_modules", IsDir: true}},
	})
	got := suggNames(a.pathCandidates(""))
	want := []string{".hidden/", "node_modules/", ".env", ".gitignore"}
	if len(got) != len(want) {
		t.Fatalf("= %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("= %v, want %v", got, want)
		}
	}
}

// TestPathCandidatesBareAtListsRoot: a bare `@` names no directory, so it lists
// the completion root — one readdir, omp's answer to the same token. Hidden and
// vendored entries at the root are offered; only VCS metadata is dropped.
func TestPathCandidatesBareAtListsRoot(t *testing.T) {
	a := appWithTree(map[string][]PathEntry{
		"": {{Name: ".git", IsDir: true}, {Name: "cmd", IsDir: true},
			{Name: "main.go"}, {Name: "node_modules", IsDir: true}},
	})
	got := suggNames(a.pathCandidates(""))
	want := []string{"cmd/", "node_modules/", "main.go"}
	if len(got) != len(want) {
		t.Fatalf("bare @ = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bare @ = %v, want %v", got, want)
		}
	}
}

// TestPathCandidatesNeverWalksOnBareAt is the regression guard for the stall
// this file exists to prevent: the only directory a keystroke may read is the
// one the token names (the root when it names none). Nothing here may ever ask
// for a listing that is not a single readdir, however many keystrokes follow.
func TestPathCandidatesNeverWalksOnBareAt(t *testing.T) {
	var asked []string
	a := &App{}
	a.SetPathCompletion("/proj", func(dir string) []PathEntry {
		asked = append(asked, dir)
		return nil
	})
	for _, q := range []string{"", "", "a", "/", "internal/"} {
		a.pathCandidates(q)
	}
	want := []string{"", "", "", "", "internal"}
	if len(asked) != len(want) {
		t.Fatalf("readdir calls = %q, want %q", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("readdir calls = %q, want %q", asked, want)
		}
	}
}

// TestPathCandidatesTrailingSlashListsDir: accepting a directory adds "/" and
// reopens the menu inside it, so the next keystroke must list that directory.
func TestPathCandidatesTrailingSlashListsDir(t *testing.T) {
	a := appWithTree(map[string][]PathEntry{
		"internal": {{Name: "tool", IsDir: true}, {Name: "tui", IsDir: true}},
	})
	got := suggNames(a.pathCandidates("internal/"))
	if len(got) != 2 || got[0] != "tool/" || got[1] != "tui/" {
		t.Fatalf("internal/ = %v, want [tool/ tui/]", got)
	}
}

// TestPathCandidatesSlashListsRoot: `@/` names the root explicitly, so it must
// read that directory — the same listing a bare `@` shows.
func TestPathCandidatesSlashListsRoot(t *testing.T) {
	a := appWithTree(map[string][]PathEntry{
		"": {{Name: "cmd", IsDir: true}, {Name: "main.go"}},
	})
	got := suggNames(a.pathCandidates("/"))
	if len(got) != 2 || got[0] != "cmd/" || got[1] != "main.go" {
		t.Fatalf("@/ = %v, want [cmd/ main.go]", got)
	}
}

func TestPathCandidatesDisabledWithoutSource(t *testing.T) {
	a := &App{}
	if got := a.pathCandidates("x"); got != nil {
		t.Fatalf("completion enabled without a source: %v", got)
	}
}

func TestPathCandidatesCapsPool(t *testing.T) {
	var files []PathEntry
	for range 500 {
		files = append(files, PathEntry{Name: "file.go"})
	}
	a := appWithTree(map[string][]PathEntry{"": files})
	if got := len(a.pathCandidates("")); got > maxPathCandidates {
		t.Fatalf("unbounded candidate list: %d", got)
	}
}

// TestPathMentionQuotesSpaces: a completion that inserted a bare path with a
// space would end the mention there, so the file must be quoted.
func TestPathMentionQuotesSpaces(t *testing.T) {
	if got := pathMention("fix ", "docs", "my file.go"); got != `fix @"docs/my file.go"` {
		t.Fatalf("pathMention = %q", got)
	}
	if got := pathMention("", "", "plain.go"); got != "@plain.go" {
		t.Fatalf("pathMention = %q", got)
	}
	// A directory keeps its slash so the menu can reopen inside it — and a
	// quoted one stays OPEN, or the next keystroke would fall outside the token.
	if got := pathMention("", "a dir", "my dir/"); got != `@"a dir/my dir/` {
		t.Fatalf("pathMention = %q", got)
	}
	// A plain directory name stays unquoted, with its slash, and stays open.
	if got := pathMention("", "cmd", "xdev/"); got != "@cmd/xdev/" {
		t.Fatalf("pathMention = %q", got)
	}
}

// TestAppAtCompletionDrivesKeyPath is the end-to-end check through the real
// key handler: `@` opens the menu on the completion root, typing a directory
// prefix narrows it to that directory, Tab on a directory keeps the slash and
// reopens the menu inside it (so the next keystroke lists the CHILD directory),
// and Tab on a file closes the mention with a space.
func TestAppAtCompletionDrivesKeyPath(t *testing.T) {
	app, _ := newTestApp(t, 100, 30)
	app.SetPathCompletion("/proj", func(dir string) []PathEntry {
		return map[string][]PathEntry{
			"":         {{Name: "internal", IsDir: true}, {Name: "main.go"}},
			"internal": {{Name: "tool", IsDir: true}, {Name: "tui", IsDir: true}},
		}[dir]
	})
	// The bare `@` opens the menu on the root listing, and typing the directory
	// name rescopes it.
	typeRunes(app, "read @internal/")
	app.mu.Lock()
	got := suggNames(app.smenu.rows())
	app.mu.Unlock()
	if len(got) != 2 || got[0] != "tool/" || got[1] != "tui/" {
		t.Fatalf("menu after @internal/ = %v, want [tool/ tui/]", got)
	}
	// Tab on the first row (tool/) must keep the slash and list tool/, not
	// finish the mention with a space.
	app.handleKey(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone))
	if want := "read @internal/tool/"; app.ed.Text() != want {
		t.Fatalf("after accepting a dir = %q, want %q", app.ed.Text(), want)
	}
	// With the child directory now named, the menu is scoped to it: no
	// entries, so it closes rather than showing the parent's contents again.
	app.mu.Lock()
	open := app.smenu != nil && app.smenu.active()
	app.mu.Unlock()
	if open {
		t.Fatal("menu must close when the named directory has no matches")
	}
}
