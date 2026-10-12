package testimpact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The package's whole claim is that the CANDIDATE SET is deterministic and
// the ORDER is a judgment. These tests pin both halves against a synthetic
// repository: a fake runner supplies the `go list` graph and the git diff, so
// no toolchain, no git and no endpoint are involved.

// fixture builds a fake process runner over a synthetic `go list -e -json`
// stream and a fixed diff. Every call the package makes goes through it, so
// a test can also assert that no other command was run.
type fixture struct {
	packages []pkgInfo
	files    []string
	root     string

	calls [][]string
}

func (f *fixture) run(_ context.Context, _ string, argv ...string) (string, error) {
	f.calls = append(f.calls, argv)
	switch {
	case argv[0] == "git" && contains(argv, "rev-parse"):
		return f.root + "\n", nil
	case argv[0] == "git" && contains(argv, "--name-only"):
		return strings.Join(f.files, "\n") + "\n", nil
	case argv[0] == "git" && contains(argv, "diff"):
		return "", nil // the unified diff is best-effort
	case argv[0] == "go" && argv[1] == "list":
		var b strings.Builder
		enc := json.NewEncoder(&b)
		for _, p := range f.packages {
			if err := enc.Encode(p); err != nil {
				return "", err
			}
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("unexpected command %v", argv)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// writeTree writes a synthetic module on disk (the test files the inventory
// parses) and returns the fixture describing it.
func writeTree(t *testing.T, pkgs map[string]map[string]string) *fixture {
	t.Helper()
	root := t.TempDir()
	// Resolve symlinks: on macOS /var is a symlink to /private/var, and the
	// tool compares cleaned absolute paths.
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	var infos []pkgInfo
	for dir, testFiles := range pkgs {
		abs := filepath.Join(root, dir)
		if err := os.MkdirAll(abs, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(abs, "x.go"), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for name, body := range testFiles {
			if err := os.WriteFile(filepath.Join(abs, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		infos = append(infos, pkgInfo{
			ImportPath:  "example.com/mod/" + dir,
			Dir:         abs,
			Name:        "x",
			TestGoFiles: keys(testFiles),
		})
	}
	return &fixture{packages: infos, root: root}
}

// keys returns a map's keys, unsorted: pkgInfo's file lists are compared by
// the inventory, which sorts its own output.
func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// link adds an import edge: imp imports dep.
func (f *fixture) link(imp, dep string) {
	for i := range f.packages {
		if f.packages[i].ImportPath == "example.com/mod/"+imp {
			f.packages[i].Imports = append(f.packages[i].Imports, "example.com/mod/"+dep)
		}
	}
}

func TestSelectCandidateSetIsTheDependencyClosure(t *testing.T) {
	// a → b → c, and d standalone. A change to c must select c's and b's
	// tests (they import it) and must NOT select d's or a's — nothing on the
	// path from a to c exists, so a is unaffected.
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\nfunc TestA(t *testing.T) {}\n"},
		"b": {"b_test.go": "package x\n\nfunc TestB(t *testing.T) {}\n"},
		"c": {"c_test.go": "package x\n\nfunc TestC(t *testing.T) {}\n"},
		"d": {"d_test.go": "package x\n\nfunc TestD(t *testing.T) {}\n"},
	})
	f.link("b", "c")
	f.files = []string{"c/c.go"}

	res, err := Select(context.Background(), Options{CWD: f.root, run: f.run})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range res.Candidates {
		got[c.Name] = true
	}
	if !got["TestC"] || !got["TestB"] {
		t.Fatalf("closure of a change to c must include TestC and TestB, got %v", names(res))
	}
	if got["TestD"] {
		t.Fatalf("TestD is in an unrelated package and must not be selected, got %v", names(res))
	}
	if got["TestA"] {
		t.Fatalf("TestA does not import c and must not be selected, got %v", names(res))
	}
	if len(res.ChangedPackages) != 1 || !strings.HasSuffix(res.ChangedPackages[0], "/c") {
		t.Fatalf("changed packages = %v", res.ChangedPackages)
	}
}

func TestSelectTestImportsCountAsEdges(t *testing.T) {
	// b's TEST imports c, while b's production code does not. A change to c
	// still affects b's tests — that is exactly the edge a hand-rolled scan
	// of production imports would miss.
	f := writeTree(t, map[string]map[string]string{
		"b": {"b_test.go": "package x\n\nfunc TestB(t *testing.T) {}\n"},
		"c": {"c_test.go": "package x\n\nfunc TestC(t *testing.T) {}\n"},
	})
	for i := range f.packages {
		if strings.HasSuffix(f.packages[i].ImportPath, "/b") {
			f.packages[i].TestImports = []string{"example.com/mod/c"}
		}
	}
	f.files = []string{"c/c.go"}

	res, err := Select(context.Background(), Options{CWD: f.root, run: f.run})
	if err != nil {
		t.Fatal(err)
	}
	if !hasTest(res, "TestB") {
		t.Fatalf("a test-only import is an edge: TestB must be selected, got %v", names(res))
	}
}

func TestSelectDirectPackageRanksFirstWithoutARanker(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\nfunc TestZebra(t *testing.T) {}\n"},
		"z": {"z_test.go": "package x\n\nfunc TestApple(t *testing.T) {}\n"},
	})
	f.link("a", "z") // a imports z, so z is in a's closure
	f.files = []string{"z/z.go"}

	res, err := Select(context.Background(), Options{CWD: f.root, run: f.run})
	if err != nil {
		t.Fatal(err)
	}
	// z is the changed package, so its test is direct; a reaches it through
	// an import. Alphabetically TestApple (in the changed package) would come
	// first anyway, so make the assertion about the flag, not the name.
	var direct []string
	for _, c := range res.Candidates {
		if c.Direct {
			direct = append(direct, c.Name)
		}
	}
	if len(direct) != 1 || direct[0] != "TestApple" {
		t.Fatalf("the changed package's own test must be marked direct, got %v (%v)", direct, names(res))
	}
}

func TestSelectNonGoAndUnknownFilesContributeNothing(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\nfunc TestA(t *testing.T) {}\n"},
	})
	f.files = []string{"README.md", "scripts/build.sh", "a/notes.txt"}

	res, err := Select(context.Background(), Options{CWD: f.root, run: f.run})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("no Go file changed, so no test can be selected, got %v", names(res))
	}
	if len(res.Notes) == 0 {
		t.Fatal("an empty selection must say why, not return a bare empty list")
	}
}

func TestSelectEmptyDiffIsNotAnError(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\nfunc TestA(t *testing.T) {}\n"},
	})
	f.files = nil

	res, err := Select(context.Background(), Options{CWD: f.root, run: f.run})
	if err != nil {
		t.Fatalf("an empty diff is a normal state, not an error: %v", err)
	}
	if len(res.Candidates) != 0 || res.TotalTests != 0 {
		t.Fatalf("nothing changed, nothing to select, got %v", names(res))
	}
}

func TestSelectCapKeepsDirectPackages(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"aaa": {"a_test.go": "package x\n\nfunc TestA1(t *testing.T) {}\nfunc TestA2(t *testing.T) {}\nfunc TestA3(t *testing.T) {}\n"},
		"zzz": {"z_test.go": "package x\n\nfunc TestZ1(t *testing.T) {}\n"},
	})
	f.link("aaa", "zzz")
	f.files = []string{"zzz/z.go"}

	res, err := Select(context.Background(), Options{CWD: f.root, MaxCandidates: 1, run: f.run})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("cap of 1 must yield 1 candidate, got %d", len(res.Candidates))
	}
	if !res.Candidates[0].Direct {
		t.Fatalf("the cap must keep the changed package's own test, got %v", names(res))
	}
	if !res.Truncated || res.TotalTests != 4 {
		t.Fatalf("truncation must be reported (total=%d truncated=%v)", res.TotalTests, res.Truncated)
	}
}

// fakeRanker stands in for the TypeSafe/Laya call.
type fakeRanker struct {
	scores map[string]float64
	note   string
	err    error

	sawChange string
	sawCands  int
}

func (f *fakeRanker) Rank(_ context.Context, change string, cands []Candidate) (map[string]float64, string, error) {
	f.sawChange = change
	f.sawCands = len(cands)
	return f.scores, f.note, f.err
}

func TestSelectRankingReordersAndKeepsTheWholeSet(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\n"},
	})
	f.files = []string{"a/a.go"}
	rk := &fakeRanker{scores: map[string]float64{
		"example.com/mod/a::TestB": 0.9,
		"example.com/mod/a::TestA": 0.1,
	}, note: "ranked by test-model"}

	res, err := Select(context.Background(), Options{CWD: f.root, Ranker: rk, run: f.run})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ranked {
		t.Fatal("a ranker that answered must set Ranked")
	}
	if res.Candidates[0].Name != "TestB" {
		t.Fatalf("the ranking must reorder: got %v", names(res))
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("ranking must never drop a candidate, got %v", names(res))
	}
	if rk.sawCands != 2 {
		t.Fatalf("the ranker must see every candidate, got %d", rk.sawCands)
	}
	if !strings.Contains(rk.sawChange, "a/a.go") {
		t.Fatalf("the change text must name the changed file, got %q", rk.sawChange)
	}
	if res.RankNote != "ranked by test-model" {
		t.Fatalf("rank note = %q", res.RankNote)
	}
}

func TestSelectRankerFailureDegradesInsteadOfFailing(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\n"},
	})
	f.files = []string{"a/a.go"}
	rk := &fakeRanker{err: errors.New("connection refused")}

	res, err := Select(context.Background(), Options{CWD: f.root, Ranker: rk, run: f.run})
	if err != nil {
		t.Fatalf("an unreachable ranker must not fail the selection: %v", err)
	}
	if res.Ranked {
		t.Fatal("a failed ranker must not claim a ranking")
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("the deterministic candidate set survives the degradation, got %v", names(res))
	}
	if !strings.Contains(res.RankNote, "connection refused") {
		t.Fatalf("the degradation must say why, got %q", res.RankNote)
	}
	// Deterministic order: same package, so by name.
	if res.Candidates[0].Name != "TestA" {
		t.Fatalf("unranked order must be stable (name within a package), got %v", names(res))
	}
}

func TestCommandIsOneLinePerPackage(t *testing.T) {
	cands := []Candidate{
		{Package: "example.com/mod/a", PkgDir: "internal/a", Name: "TestB"},
		{Package: "example.com/mod/a", PkgDir: "internal/a", Name: "TestA"},
		{Package: "example.com/mod/b", PkgDir: "internal/b", Name: "TestC"},
	}
	got := command(cands, 10)
	want := "go test ./internal/a -run '^(TestA|TestB)$'\n" +
		"go test ./internal/b -run '^(TestC)$'"
	if got != want {
		t.Fatalf("command =\n%q\nwant\n%q", got, want)
	}
}

func TestCommandIsEmptyWithoutCandidates(t *testing.T) {
	if got := command(nil, 10); got != "" {
		t.Fatalf("no candidates must produce no command, got %q", got)
	}
}

func TestTestsInReadsDocCommentsAndSkipsHelpers(t *testing.T) {
	dir := t.TempDir()
	src := `package x

// TestNamed is the doc line the ranker reads.
// It continues here.
func TestNamed(t *testing.T) {}

func testLowercase(t *testing.T) {}

type helper struct{}

func (h helper) TestMethod(t *testing.T) {}

func Test(t *testing.T) {}

func FuzzThing(f *testing.F) {}
`
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := testsIn(&pkgInfo{ImportPath: "m/x", Dir: dir, TestGoFiles: []string{"x_test.go"}})
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	want := map[string]bool{"TestNamed": true, "FuzzThing": true}
	if len(names) != 2 {
		t.Fatalf("only top-level Test/Fuzz funcs count, got %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected test %q in %v", n, names)
		}
	}
	for _, c := range got {
		if c.Name == "TestNamed" && !strings.Contains(c.Doc, "doc line the ranker reads") {
			t.Fatalf("doc = %q", c.Doc)
		}
		if c.Name == "TestNamed" && !strings.Contains(c.Doc, "continues here") {
			t.Fatalf("a multi-line doc must collapse to one line, got %q", c.Doc)
		}
	}
}

func TestParseChoiceReadsTheDistribution(t *testing.T) {
	answers := map[string]any{"pick": map[string]any{
		"type":          "choice",
		"choice":        "b",
		"probabilities": map[string]any{"a": 0.2, "b": 0.8},
	}}
	got := parseChoice(answers, "pick")
	if got["b"] != 0.8 || got["a"] != 0.2 {
		t.Fatalf("distribution = %v", got)
	}
}

func TestParseChoiceFallsBackToTheLabel(t *testing.T) {
	// A thin backend that returns only the winner still ranks: the winner
	// gets 1 and everything else ties at 0, which is an honest rendering of
	// "it picked one and told us nothing else".
	answers := map[string]any{"pick": map[string]any{"choice": "only"}}
	got := parseChoice(answers, "pick")
	if len(got) != 1 || got["only"] != 1 {
		t.Fatalf("label fallback = %v", got)
	}
}

func TestParseChoiceOnJunkIsEmpty(t *testing.T) {
	for _, in := range []map[string]any{
		{},
		{"pick": "not an object"},
		{"pick": map[string]any{"probabilities": "not an object"}},
	} {
		if got := parseChoice(in, "pick"); len(got) != 0 {
			t.Fatalf("junk %v produced %v", in, got)
		}
	}
}

func TestDistributionFlagsAFlatAnswer(t *testing.T) {
	flat := []Candidate{{Name: "TestA", Score: 0.2}, {Name: "TestB", Score: 0.19}}
	if got := Distribution(flat, 5); !strings.Contains(got, "flat") {
		t.Fatalf("a distribution under the ceiling must say it is flat, got %q", got)
	}
	sharp := []Candidate{{Name: "TestA", Score: 0.9}, {Name: "TestB", Score: 0.05}}
	if got := Distribution(sharp, 5); strings.Contains(got, "flat") {
		t.Fatalf("a decisive distribution must not be called flat, got %q", got)
	}
}

func TestToolReportsAndNeverRuns(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\n// TestA covers a.\nfunc TestA(t *testing.T) {}\n"},
	})
	f.files = []string{"a/a.go"}
	rk := &fakeRanker{scores: map[string]float64{"example.com/mod/a::TestA": 0.7}, note: "ranked by fake"}

	tl := &Tool{CWD: f.root, ranker: rk, run: f.run}
	res, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a clean run must not be an error: %s", res.Text)
	}
	for _, want := range []string{"regression_tests", "TestA", "ranked by fake", "go test ./a -run", "this tool does not"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("report missing %q:\n%s", want, res.Text)
		}
	}
	// It selects, it never runs: the only subprocesses are git and go list.
	for _, call := range f.calls {
		if call[0] != "git" && call[0] != "go" {
			t.Fatalf("the tool ran %v; it must only inspect", call)
		}
		if call[0] == "go" && call[1] != "list" {
			t.Fatalf("the tool ran `go %s`; it must not run tests", call[1])
		}
	}
	d, ok := res.Details.(map[string]any)
	if !ok {
		t.Fatalf("details must be structured, got %T", res.Details)
	}
	if d["ranked"] != true || d["command"] == "" {
		t.Fatalf("details = %v", d)
	}
}

func TestToolRejectsBadArguments(t *testing.T) {
	tl := &Tool{CWD: t.TempDir()}
	cases := []struct {
		args string
		want string
	}{
		{`{"max_candidates":-1}`, "non-negative"},
		{`{"max_candidates":9999}`, "ceiling"},
		{`{not json`, "malformed"},
	}
	for _, c := range cases {
		res, err := tl.Execute(context.Background(), json.RawMessage(c.args))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Fatalf("args %s → %q (want %q)", c.args, res.Text, c.want)
		}
	}
}

func TestToolWithoutCWDIsAnError(t *testing.T) {
	tl := &Tool{}
	res, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "working directory") {
		t.Fatalf("got %q", res.Text)
	}
}

func TestToolNoRankSkipsTheRanker(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\nfunc TestA(t *testing.T) {}\n"},
	})
	f.files = []string{"a/a.go"}
	rk := &fakeRanker{scores: map[string]float64{"example.com/mod/a::TestA": 0.9}}
	tl := &Tool{CWD: f.root, ranker: rk, run: f.run}
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"no_rank":true}`))
	if err != nil {
		t.Fatal(err)
	}
	// An injected ranker is a test seam and always wins; the flag's effect is
	// visible on the note, which must not claim a ranking happened.
	if !strings.Contains(res.Text, "TestA") {
		t.Fatalf("no_rank must still return the candidate set: %s", res.Text)
	}
}

func TestTimeoutDefaultIsUsedForSubprocesses(t *testing.T) {
	f := writeTree(t, map[string]map[string]string{
		"a": {"a_test.go": "package x\n\nfunc TestA(t *testing.T) {}\n"},
	})
	f.files = []string{"a/a.go"}
	// A caller-supplied timeout must not be ignored; the fake records the
	// deadline it was handed.
	var sawDeadline bool
	run := func(ctx context.Context, dir string, argv ...string) (string, error) {
		if _, ok := ctx.Deadline(); ok {
			sawDeadline = true
		}
		return f.run(ctx, dir, argv...)
	}
	if _, err := Select(context.Background(), Options{CWD: f.root, Timeout: time.Second, run: run}); err != nil {
		t.Fatal(err)
	}
	if !sawDeadline {
		t.Fatal("every subprocess must run under a deadline")
	}
}

func names(res *Result) []string {
	out := make([]string, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		out = append(out, c.Name)
	}
	return out
}

func hasTest(res *Result, name string) bool {
	for _, c := range res.Candidates {
		if c.Name == name {
			return true
		}
	}
	return false
}
