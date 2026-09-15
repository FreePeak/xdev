package tool

import (
	"fmt"
	"strings"
	"testing"
)

func TestUnifiedDiffKinds(t *testing.T) {
	cases := []struct {
		name          string
		before, after []string
		wantRows      []string // must appear in this order
	}{
		{
			name:   "append",
			before: []string{"one"},
			after:  []string{"one", "two"},
			wantRows: []string{
				"--- a/f.go", "+++ b/f.go", "@@ -1 +1,2 @@",
				" one", "+two",
			},
		},
		{
			name:   "remove",
			before: []string{"gone", "keep"},
			after:  []string{"keep"},
			wantRows: []string{
				"@@ -1,2 +1 @@", "-gone", " keep",
			},
		},
		{
			name:     "replace keeps both sides",
			before:   []string{"var a = 1"},
			after:    []string{"var a = 2"},
			wantRows: []string{"-var a = 1", "+var a = 2"},
		},
		{
			name:   "create is all additions",
			before: nil,
			after:  []string{"first", "second"},
			wantRows: []string{
				"--- /dev/null", "+++ b/f.go", "@@ -0,0 +1,2 @@", "+first", "+second",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := UnifiedDiff("f.go", tc.before, tc.after)
			if !ok {
				t.Fatalf("no diff for %v → %v", tc.before, tc.after)
			}
			rows := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			if !inOrder(rows, tc.wantRows) {
				t.Errorf("rows wrong:\n%s\nwant in order: %q", got, tc.wantRows)
			}
		})
	}
}

func TestUnifiedDiffUnchanged(t *testing.T) {
	if got, ok := UnifiedDiff("f.go", []string{"a", "b"}, []string{"a", "b"}); ok || got != "" {
		t.Fatalf("identical input diffed: %q ok=%v", got, ok)
	}
	if _, ok := UnifiedDiff("f.go", nil, nil); ok {
		t.Fatal("empty input diffed")
	}
}

// A one-line change in a big file renders as one hunk with three context rows
// either side of it — not as the whole file.
func TestUnifiedDiffTrimsAndCarriesContext(t *testing.T) {
	const n = 200
	filler := func(i int) string {
		return fmt.Sprintf("%d same", i)
	}
	before, after := make([]string, n), make([]string, n)
	for i := range before {
		before[i], after[i] = filler(i), filler(i)
	}
	after[100] = "CHANGED"
	got, ok := UnifiedDiff("f.go", before, after)
	if !ok {
		t.Fatal("no diff")
	}
	if n := strings.Count(got, "\n@@ "); n != 1 {
		t.Fatalf("%d hunks, want 1:\n%s", n, got)
	}
	head, tail, seen := 0, 0, false
	for _, r := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		switch {
		case strings.HasPrefix(r, "@@"), strings.HasPrefix(r, "--"), strings.HasPrefix(r, "++"):
		case strings.HasPrefix(r, "-"), strings.HasPrefix(r, "+"):
			seen = true
		case !seen:
			head++
		default:
			tail++
		}
	}
	if head != 3 || tail != 3 {
		t.Errorf("context = %d above / %d below the change, want 3/3:\n%s", head, tail, got)
	}
}

// Past the size ceiling a diff is dropped rather than computed: a 4000-line
// change would paint a screenful of noise, and the LCS table costs to build.
func TestUnifiedDiffSizeCeiling(t *testing.T) {
	before, after := []string{"a"}, []string{"b"}
	for i := 0; i < diffSizeCeiling/2+10; i++ {
		before = append(before, "line")
		after = append(after, "line")
	}
	if _, ok := UnifiedDiff("f.go", before, after); ok {
		t.Fatal("oversized input produced a diff")
	}
}

// The hunk header's counts must agree with the rows it introduces. Three
// context rows either side of a change at lines 4-5 of a 7-line file reach
// both ends, so the single hunk covers the whole file.
func TestUnifiedDiffHeaderCounts(t *testing.T) {
	before := []string{"a", "b", "c", "d", "e", "f", "g"}
	after := []string{"a", "b", "c", "X", "Y", "f", "g"}
	got, _ := UnifiedDiff("f.go", before, after)
	if !strings.Contains(got, "@@ -1,7 +1,7 @@") {
		t.Errorf("hunk header wrong:\n%s", got)
	}
	minus, plus := tallyRows(got)
	if minus != 2 || plus != 2 {
		t.Errorf("row counts -%d +%d, want 2/2\n%s", minus, plus, got)
	}
}

// A change whose middle is past the LCS table budget still yields a
// well-formed diff: the fallback is one whole-middle replace, not a crash and
// not an unbounded allocation.
func TestUnifiedDiffFallsBackPastDPBudget(t *testing.T) {
	const n = 1100 // n*n is past diffDPBudget
	before, after := []string{"keep"}, []string{"keep"}
	for i := 0; i < n; i++ {
		before = append(before, fmt.Sprintf("old %d", i))
		after = append(after, fmt.Sprintf("new %d", i))
	}
	got, ok := UnifiedDiff("f.go", before, after)
	if !ok {
		t.Fatal("no diff")
	}
	if n := strings.Count(got, "\n@@ "); n != 1 {
		t.Fatalf("%d hunks, want 1", n)
	}
	if !strings.Contains(got, fmt.Sprintf("@@ -1,%d +1,%d @@", n+1, n+1)) {
		t.Errorf("hunk header wrong:\n%s", firstLines(got, 4))
	}
	minus, plus := tallyRows(got)
	if minus != n || plus != n {
		t.Errorf("row counts -%d +%d, want %d/%d", minus, plus, n, n)
	}
}

// tallyRows counts a diff's changed rows, file headers excluded.
func tallyRows(diff string) (minus, plus int) {
	for _, r := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(r, "---"), strings.HasPrefix(r, "+++"):
		case strings.HasPrefix(r, "-"):
			minus++
		case strings.HasPrefix(r, "+"):
			plus++
		}
	}
	return
}

// inOrder reports whether want appears inside rows, in sequence: the header has
// to sit above its rows, so set membership is not enough.
func inOrder(rows, want []string) bool {
	i := 0
	for _, r := range rows {
		if i < len(want) && r == want[i] {
			i++
		}
	}
	return i == len(want)
}

func firstLines(s string, n int) string {
	rows := strings.Split(s, "\n")
	if len(rows) > n {
		rows = rows[:n]
	}
	return strings.Join(rows, "\n")
}

func TestSplitLines(t *testing.T) {
	if got := SplitLines([]byte("a\nb\n")); len(got) != 2 || got[1] != "b" {
		t.Errorf("trailing newline made a line: %q", got)
	}
	if got := SplitLines(nil); got != nil {
		t.Errorf("empty file = %q, want none", got)
	}
}
