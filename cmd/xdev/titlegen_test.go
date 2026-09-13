package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// cleanTitle is the guard between "a model answered" and "a usable title
// landed in the picker": format noise is normalized, and anything that is not
// a title is rejected so the mechanical title stands.
func TestCleanTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"why grass is green"`, "why grass is green"}, // wrapped in quotes
		{"  Session: DB migration plan  ", "Session: DB migration plan"},
		{"Fixing the parser.\n\nMore prose", "Fixing the parser"}, // first line, no period
		{"", ""},
		{"Title", ""},                     // refusal-shaped
		{"title", ""},                     // ditto, case-insensitive
		{"NO", "NO"},                      // short answers are fine
		{"a? b!", ""},                     // not a title
		{strings.Repeat("word ", 12), ""}, // a sentence, not a title
	}
	for _, c := range cases {
		if got := cleanTitle(c.in); got != c.want {
			t.Errorf("cleanTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Length cap holds on a rune boundary and never splits an emoji.
	long := strings.Repeat("字", 60)
	got := cleanTitle(long)
	if len([]rune(got)) > titleMaxRunes {
		t.Fatalf("cap not applied: %d runes", len([]rune(got)))
	}
}

// shouldGenerateTitle is the user-intent guard: a manual rename and a subagent
// title are authoritative, an unstamped file is not a candidate.
func TestShouldGenerateTitle(t *testing.T) {
	dir := t.TempDir()
	write := func(name, title, source string) *session.Store {
		p := dir + "/" + name
		st := session.OpenMem("/proj", title)
		if _, err := st.EnsureOnDisk(p, session.Options{}); err != nil {
			t.Fatal(err)
		}
		if source != session.TitleSourceAuto {
			if err := st.Rename(title, source); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := session.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		return reopened
	}
	if shouldGenerateTitle(nil) {
		t.Fatal("nil store")
	}
	mem := session.OpenMem("/proj", "print 2026-01-01 00:00")
	if shouldGenerateTitle(mem) {
		t.Fatal("a store with no file on disk must not be renamed")
	}
	if got := shouldGenerateTitle(write("a.jsonl", "print 2026-01-01 00:00", session.TitleSourceAuto)); !got {
		t.Fatal("a mechanical auto title is the whole point of the cascade")
	}
	if shouldGenerateTitle(write("b.jsonl", "my hand-written name", session.TitleSourceManual)) {
		t.Fatal("/rename wins over the model, always")
	}
	if shouldGenerateTitle(write("c.jsonl", "why grass is green", session.TitleSourceAuto)) {
		t.Fatal("an already-generated title must not be regenerated")
	}
}

func TestTitleSeed(t *testing.T) {
	user, asst := titleSeed([]ai.Message{
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "first question"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "first answer"}}},
		{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "second"}}},
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: "latest answer"}}},
	})
	if user != "first question" {
		t.Fatalf("seed used %q, want the opening user turn", user)
	}
	if asst != "latest answer" {
		t.Fatalf("seed used %q, want the latest assistant reply", asst)
	}
}

// The cascade must reach the session's own model. @tiny/@smol have no
// built-in mapping, so a plain models.yml resolved neither and the title
// stayed "print <timestamp>" for every default install — the feature existed
// and never ran.
func TestSessionModelRefIsTheLastResort(t *testing.T) {
	if got := sessionModelRef("onegw", "free"); got != "onegw/free" {
		t.Fatalf("ref = %q, want onegw/free", got)
	}
	for _, c := range [][2]string{{"", "free"}, {"onegw", ""}, {"  ", "  "}} {
		if got := sessionModelRef(c[0], c[1]); got != "" {
			t.Fatalf("sessionModelRef(%q,%q) = %q, want empty", c[0], c[1], got)
		}
	}
	// An empty fallback ref is skipped rather than sent to the resolver
	// (which would answer with an error naming the default model instead).
	if modelRoleRef("onegw/free") != "" {
		t.Fatal("a literal ref must not be read as a role")
	}
}

// shouldGenerateTitle must accept a mechanical title whose timestamp form
// varies by mode ("print …", "continued print …", "tui …", "imported: …") —
// those are the titles every session actually starts with.
func TestShouldGenerateTitleAcceptsMechanicalPrefixes(t *testing.T) {
	dir := t.TempDir()
	for _, mechanical := range []string{
		"print 2026-09-13 04:12", "continued print 2026-09-13 04:12",
		"tui 2026-09-13 04:12", "imported: claude-abc123",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(mechanical[:5], " ", "_")+".jsonl")
		st := session.OpenMem("/proj", mechanical)
		if _, err := st.EnsureOnDisk(p, session.Options{}); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := session.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if !shouldGenerateTitle(reopened) {
			t.Errorf("%q is mechanical and should be replaced", mechanical)
		}
		_ = reopened.Close()
	}
}
