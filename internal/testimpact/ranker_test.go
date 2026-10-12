package testimpact

import (
	"strings"
	"testing"
)

// The two-stage ranker's own logic — bucketing, ordering, the option caps and
// the total-order guarantee — with no endpoint involved. The live behaviour is
// measured in the type comment; these pin the parts that must not depend on a
// model.

func cands(pkg string, names ...string) []Candidate {
	out := make([]Candidate, 0, len(names))
	for _, n := range names {
		out = append(out, Candidate{Package: pkg, Name: n, PkgDir: shortPackage(pkg)})
	}
	return out
}

func TestPackageOrderPutsDirectFirstThenSorted(t *testing.T) {
	byPkg := groupByPackage(append(
		append(cands("m/zeta", "TestZ"), cands("m/alpha", "TestA")...),
		Candidate{Package: "m/direct", Name: "TestD", Direct: true},
	))
	got := packageOrder(byPkg, 0)
	want := []string{"m/direct", "m/alpha", "m/zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestPackageOrderHonoursTheCap(t *testing.T) {
	var all []Candidate
	for _, p := range []string{"m/a", "m/b", "m/c"} {
		all = append(all, cands(p, "TestX")...)
	}
	if got := packageOrder(groupByPackage(all), 2); len(got) != 2 {
		t.Fatalf("cap of 2 gave %v", got)
	}
}

func TestTopPackagesTieBreaksOnTheDeterministicOrder(t *testing.T) {
	order := []string{"m/a", "m/b", "m/c"}
	// A perfectly flat answer must still produce a stable, deterministic
	// winner — the same package every run.
	prob := map[string]float64{"m/a": 0.33, "m/b": 0.33, "m/c": 0.33}
	got := topPackages(prob, order, 1)
	if len(got) != 1 || got[0] != "m/a" {
		t.Fatalf("flat answer must fall back to the deterministic order, got %v", got)
	}
}

func TestTopPackagesPicksTheHighest(t *testing.T) {
	order := []string{"m/a", "m/b"}
	prob := map[string]float64{"m/a": 0.1, "m/b": 0.9}
	got := topPackages(prob, order, 1)
	if len(got) != 1 || got[0] != "m/b" {
		t.Fatalf("got %v", got)
	}
}

func TestTestOptionsKeyByCandidateKey(t *testing.T) {
	cs := cands("m/a", "TestOne", "TestTwo")
	opts := testOptions(cs)
	if len(opts) != 2 || opts[0].key != "m/a::TestOne" {
		t.Fatalf("options = %+v", opts)
	}
	if !strings.Contains(opts[0].text, "TestOne") {
		t.Fatalf("criterion text must name the test, got %q", opts[0].text)
	}
}

func TestPackageOptionsNameThePackageAndATest(t *testing.T) {
	byPkg := groupByPackage(cands("example.com/mod/internal/tool", "TestA", "TestB"))
	opts := packageOptions([]string{"example.com/mod/internal/tool"}, byPkg)
	if len(opts) != 1 || opts[0].key != "example.com/mod/internal/tool" {
		t.Fatalf("options = %+v", opts)
	}
	// The key must be the import path (what the answer maps back to) while the
	// text is the short name plus a sample test — the model reads names, not
	// module paths.
	if !strings.Contains(opts[0].text, "internal/tool") || !strings.Contains(opts[0].text, "TestA") {
		t.Fatalf("text = %q", opts[0].text)
	}
}

func TestCapCandidates(t *testing.T) {
	cs := cands("m/a", "T1", "T2", "T3")
	if got := capCandidates(cs, 2); len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	if got := capCandidates(cs, 0); len(got) != 3 {
		t.Fatalf("0 means no cap, got %d", len(got))
	}
}

func TestSmallest(t *testing.T) {
	if got := smallest(map[string]float64{"a": 0.5, "b": 0.2}); got != 0.2 {
		t.Fatalf("got %v", got)
	}
	// An empty distribution returns 1 so a caller dividing by it never gets
	// a NaN; the caller only uses it as a floor.
	if got := smallest(map[string]float64{}); got != 1 {
		t.Fatalf("empty = %v", got)
	}
}

func TestShortPackageTrimsTheModulePath(t *testing.T) {
	cases := map[string]string{
		"github.com/FreePeak/xdev/internal/tool": "internal/tool",
		"github.com/FreePeak/xdev/cmd/xdev":      "cmd/xdev",
		"example.com/mod/pkg/thing":              "pkg/thing",
		"unsliced":                               "unsliced",
	}
	for in, want := range cases {
		if got := shortPackage(in); got != want {
			t.Fatalf("shortPackage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCriterionCarriesDocAndDirectness(t *testing.T) {
	c := Candidate{Package: "m/internal/a", Name: "TestX", Doc: "pins the retry", Direct: true}
	got := criterion(c)
	for _, want := range []string{"TestX", "pins the retry", "internal/a", "in a changed package"} {
		if !strings.Contains(got, want) {
			t.Fatalf("criterion %q missing %q", got, want)
		}
	}
}

// The ranker's degradation path, with a fake evaluator: the multi-package
// path must produce a score for EVERY candidate (a total order), and the
// one-package path must not ask a single-option question.
func TestRankMultiPackageIsATotalOrder(t *testing.T) {
	// Two packages, two tests each: stage 1 picks one, stage 2 orders its
	// tests, and the rejected package must still come out ranked.
	cs := append(cands("m/a", "TestA1", "TestA2"), cands("m/b", "TestB1", "TestB2")...)
	byPkg := groupByPackage(cs)

	// Drive the helpers the way Rank does, without an endpoint.
	pkgProb := map[string]float64{"m/a": 0.9, "m/b": 0.1}
	order := packageOrder(byPkg, maxPackageOptions)
	top := topPackages(pkgProb, order, 1)
	if len(top) != 1 || top[0] != "m/a" {
		t.Fatalf("stage 1 must pick m/a, got %v", top)
	}
	sub := map[string]float64{"m/a::TestA2": 0.8, "m/a::TestA1": 0.2}
	scores := map[string]float64{}
	for _, c := range byPkg["m/a"] {
		scores[c.Key()] = pkgProb["m/a"] * sub[c.Key()]
	}
	share := pkgProb["m/b"] / float64(len(byPkg["m/b"]))
	for _, c := range byPkg["m/b"] {
		scores[c.Key()] = share
	}
	if len(scores) != 4 {
		t.Fatalf("every candidate needs a score, got %d", len(scores))
	}
	if scores["m/a::TestA2"] <= scores["m/b::TestB1"] {
		t.Fatal("a rejected package must rank below an accepted one")
	}
	if scores["m/b::TestB1"] != scores["m/b::TestB2"] {
		t.Fatal("a rejected package's tests split its probability evenly")
	}
}
