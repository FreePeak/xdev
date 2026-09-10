package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapUserCommandsDir points the user-root seam at dir for the test's
// lifetime, so discovery never touches the developer's real ~/.xdev.
func swapUserCommandsDir(t *testing.T, dir string) {
	t.Helper()
	prev := userCommandsDir
	userCommandsDir = func() string { return dir }
	t.Cleanup(func() { userCommandsDir = prev })
}

// writeFile creates a markdown command file with directories as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverCommandsProjectBeatsUser(t *testing.T) {
	dir := t.TempDir()
	swapUserCommandsDir(t, filepath.Join(dir, "agent")) // user root = <dir>/agent
	writeFile(t, filepath.Join(dir, "agent", "commands", "deploy.md"),
		"user version\n")
	writeFile(t, filepath.Join(dir, "work", ".xdev", "commands", "deploy.md"),
		"project version\n")
	// Separate name only present in the user root.
	writeFile(t, filepath.Join(dir, "agent", "commands", "useronly.md"),
		"only in user root\n")

	got := DiscoverCommands(filepath.Join(dir, "work"))
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	// Sorted by name: deploy, useronly.
	if got[0].Name != "deploy" || got[0].Template != "project version\n" {
		t.Errorf("deploy = %+v, want project version (no merge)", got[0])
	}
	if got[1].Name != "useronly" {
		t.Errorf("useronly missing: %+v", got[1])
	}
}

func TestDiscoverCommandsFilenameVsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".xdev", "commands", "commit.md"),
		"---\nname: Ship\n---\nbody\n")
	writeFile(t, filepath.Join(dir, ".xdev", "commands", "OTHER.md"),
		"plain\n")

	got := DiscoverCommands(dir)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	// Frontmatter name wins and is lowercased; sorted order shifts to other, ship.
	if got[0].Name != "other" || got[0].Description != "plain" {
		t.Errorf("other = %+v", got[0])
	}
	if got[1].Name != "ship" {
		t.Errorf("ship = %+v, want frontmatter name lowercased", got[1])
	}
	if got[1].Template != "body\n" {
		t.Errorf("template = %q, want frontmatter stripped", got[1].Template)
	}
}

func TestDiscoverCommandsDescriptionRules(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 61)
	writeFile(t, filepath.Join(dir, ".xdev", "commands", "fallback.md"),
		"\n\n  first line here  \nbody\n")
	writeFile(t, filepath.Join(dir, ".xdev", "commands", "front.md"),
		"---\ndescription: from frontmatter\n---\nbody\n")
	writeFile(t, filepath.Join(dir, ".xdev", "commands", "long.md"),
		long+"\n")
	writeFile(t, filepath.Join(dir, ".xdev", "commands", "exact.md"),
		strings.Repeat("y", 60)+"\n")

	got := DiscoverCommands(dir)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(got), got)
	}
	byName := map[string]string{}
	for _, c := range got {
		byName[c.Name] = c.Description
	}
	if byName["fallback"] != "first line here" {
		t.Errorf("fallback = %q", byName["fallback"])
	}
	if byName["front"] != "from frontmatter" {
		t.Errorf("frontmatter description must override body: %q", byName["front"])
	}
	want := long[:60] + "…" // 61 chars truncated to 60 + ellipsis
	if byName["long"] != want {
		t.Errorf("long = %q, want %q", byName["long"], want)
	}
	if byName["exact"] != strings.Repeat("y", 60) {
		t.Errorf("exact 60 chars must not gain an ellipsis: %q", byName["exact"])
	}
}

func TestDiscoverCommandsSkips(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".xdev", "commands")
	writeFile(t, filepath.Join(root, ".hidden.md"), "body\n")
	writeFile(t, filepath.Join(root, "notes.txt"), "body\n")
	writeFile(t, filepath.Join(root, "sub", "nested.md"), "body\n") // non-recursive
	writeFile(t, filepath.Join(root, "open.md"), "---\nname: x\nno close\n")
	writeFile(t, filepath.Join(root, "empty.md"), "\n  \n")
	writeFile(t, filepath.Join(root, "real.md"), "body\n")

	got := DiscoverCommands(dir)
	if len(got) != 1 || got[0].Name != "real" {
		t.Fatalf("got %+v, want only real", got)
	}
}

func TestDiscoverCommandsEmptyWhenNoRoots(t *testing.T) {
	// No project root, user root missing too: empty, never nil panic.
	if got := DiscoverCommands(t.TempDir()); len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   \t ", nil},
		{"one two", []string{"one", "two"}},
		{"  a\tb  ", []string{"a", "b"}},  // tab splits too
		{`"a b" c`, []string{"a b", "c"}}, // double quotes group
		{`'a b' c`, []string{"a b", "c"}}, // single quotes group
		{`a "b c" "d e"`, []string{"a", "b c", "d e"}},
		{`"a'b"`, []string{"a'b"}}, // other quote inside quotes kept
		{`'say "hi"'`, []string{`say "hi"`}},
		{`a "b c`, []string{"a", "b c"}}, // unmatched quote: rest is one arg
		{`a 'b`, []string{"a", "b"}},
		{`"" x`, []string{"", "x"}},           // empty quoted arg preserved
		{`a"b c"d e`, []string{"ab cd", "e"}}, // quotes mid-token group
		{`it's`, []string{"its"}},             // no escapes; stray quote toggles
	}
	for _, tt := range tests {
		got := SplitArgs(tt.raw)
		if len(got) != len(tt.want) {
			t.Errorf("SplitArgs(%q) = %q, want %q", tt.raw, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("SplitArgs(%q)[%d] = %q, want %q", tt.raw, i, got[i], tt.want[i])
			}
		}
	}
}

func TestExpandArgsPositional(t *testing.T) {
	tests := []struct {
		template, raw string
		want          string
	}{
		{"run $1 now", "alpha beta", "run alpha now"},
		{"$2 then $1", "alpha beta", "beta then alpha"},
		{"$1-$2-$3", "a b", "a-b-"},    // absent arg expands empty
		{"$1", "", ""},                 // no args: empty
		{"$1", "  \t", ""},             // whitespace-only raw: empty
		{"cost $100", "x", "cost "},    // whole digit run is the index: arg 100 absent → empty
		{"a $2 b", `"x y" z`, "a z b"}, // quoted raw splits before indexing
		{"$1 $1", "dup", "dup dup"},    // repeat allowed
		{"$1$2", "ab", "ab"},           // adjacent placeholders
	}
	for _, tt := range tests {
		got := ExpandArgs(tt.template, tt.raw)
		if got != tt.want {
			t.Errorf("ExpandArgs(%q, %q) = %q, want %q", tt.template, tt.raw, got, tt.want)
		}
	}
}

func TestExpandArgsTenArgs(t *testing.T) {
	raw := "1 2 3 4 5 6 7 8 9 10"
	if got := ExpandArgs("$10", raw); got != "10" {
		t.Errorf("$10 with 10 args = %q, want 10 (whole digit run is the index)", got)
	}
	if got := ExpandArgs("$10", "1 2"); got != "" {
		t.Errorf("$10 with 2 args = %q, want empty", got)
	}
	if got := ExpandArgs("$2", "1 2 3 4 5 6 7 8 9 10"); got != "2" {
		t.Errorf("$2 = %q, want 2", got)
	}
}

func TestExpandArgsAllAndSlices(t *testing.T) {
	tests := []struct {
		template, raw string
		want          string
	}{
		{"$@", "a b c", "a b c"},
		{"$@", "", ""},
		{"pre $@ post", "a b", "pre a b post"},
		{"$@[1]", "a b c", "a b c"}, // start 1 = everything
		{"$@[2]", "a b c", "b c"},   // from 2 to end
		{"$@[3]", "a b c", "c"},
		{"$@[4]", "a b c", ""},      // start past end: empty
		{"$@[1]", "", ""},           // no args: empty
		{"$@[1:2]", "a b c", "a b"}, // length clamps at end
		{"$@[2:2]", "a b c", "b c"},
		{"$@[2:5]", "a b c", "b c"}, // length past end → to the end
		{"$@[3:1]", "a b c", "c"},
		{"$@[4:2]", "a b c", ""}, // start past end wins
		{"$@[2:0]", "a b c", ""}, // zero length: empty
		{"$@[1:0]", "", ""},
	}
	for _, tt := range tests {
		got := ExpandArgs(tt.template, tt.raw)
		if got != tt.want {
			t.Errorf("ExpandArgs(%q, %q) = %q, want %q", tt.template, tt.raw, got, tt.want)
		}
	}
}

func TestExpandArgsArguments(t *testing.T) {
	if got := ExpandArgs("review: $ARGUMENTS", "fix the bug"); got != "review: fix the bug" {
		t.Errorf("got %q", got)
	}
	if got := ExpandArgs("$ARGUMENTS", ""); got != "" {
		t.Errorf("empty raw: got %q, want empty", got)
	}
	if got := ExpandArgs("$ARGUMENTS", "  "); got != "  " {
		t.Errorf("$ARGUMENTS is the raw text verbatim, not trimmed: got %q", got)
	}
	// $ARGUMENTS is replaced before $@ variants: raw args containing the
	// literal word are NOT re-expanded.
	if got := ExpandArgs("$ARGUMENTS", "$ARGUMENTS $@"); got != "$ARGUMENTS $@" {
		t.Errorf("raw args must not re-expand: got %q", got)
	}
	if got := ExpandArgs("$ARGUMENTS", "$1 hi"); got != "$1 hi" {
		t.Errorf("got %q", got)
	}
}

func TestExpandArgsFallbackAppend(t *testing.T) {
	tests := []struct {
		template, raw string
		want          string
	}{
		{"explain this", "go maps", "explain this\n\ngo maps"}, // blank-line separator
		{"explain this", "", "explain this"},                   // empty raw appends nothing
		{"explain this", "   ", "explain this"},
		{"multi\nline", "x", "multi\nline\n\nx"},
		{"no placeholder $", "x", "no placeholder $\n\nx"}, // lone $ is not a placeholder
		{"$ARGUMENTS", "", ""},                             // real placeholder: no fallback
		{"$1", "", ""},                                     // absent-but-present placeholder: no fallback
		{"$@", "", ""},                                     // empty expand still counts
		{"$@[1]", "", ""},
	}
	for _, tt := range tests {
		got := ExpandArgs(tt.template, tt.raw)
		if got != tt.want {
			t.Errorf("ExpandArgs(%q, %q) = %q, want %q", tt.template, tt.raw, got, tt.want)
		}
	}
}

func TestExpandArgsLiteralsPassThrough(t *testing.T) {
	tests := []struct {
		template, raw string
		want          string
	}{
		{"price $FOO", "x", "price $FOO\n\nx"},   // unknown $name literal → no placeholder → fallback
		{"cmd `ls` $1", "x", "cmd `ls` x"},       // backticks untouched
		{"$ARGUMENTS2", "x", "$ARGUMENTS2\n\nx"}, // longer word: not a placeholder → fallback
		{"a $ b", "x", "a $ b\n\nx"},             // lone $ literal → fallback
		{"$@[x]", "a b", "$@[x]\n\na b"},         // malformed bracket: literal → fallback
		{"$@[1", "a b", "$@[1\n\na b"},           // unclosed bracket: literal → fallback
		{"$@[0]", "a b", "$@[0]\n\na b"},         // 0 is not 1-based: literal → fallback
	}
	for _, tt := range tests {
		if got := ExpandArgs(tt.template, tt.raw); got != tt.want {
			t.Errorf("ExpandArgs(%q, %q) = %q, want %q", tt.template, tt.raw, got, tt.want)
		}
	}
}
