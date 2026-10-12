package testimpact

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/typesafe"
)

// RankerName is the tool-facing name of the model-backed ranker.
const RankerName = "typesafe"

// DefaultRankTimeout bounds the WHOLE ranking — every stage, not one call.
// It is a degradation, never an error: the deterministic order stands.
const DefaultRankTimeout = 25 * time.Second

// Ranking shape: two stages, and the caps that make each one answerable.
//
// Stage 1 asks which PACKAGE the change touches (a dozen options, each a name
// with meaning); stage 2 asks which TEST inside the winning packages (16
// options, each a test name plus its doc comment). The caps come from the
// measurement in the type comment below: a `choice` splits probability across
// its options, so past ~16 a question's resolution is noise.
const (
	// maxPackageOptions caps the stage-1 question. A package list is short by
	// nature (this repo's whole closure is 27) and the cap is only a
	// backstop for a monorepo.
	maxPackageOptions = 24
	// maxTestOptions caps one stage-2 question. Measured: 16 options gave a
	// peak of 0.56 with a clear structure; 41 gave 0.33; 60 gave 0.025.
	maxTestOptions = 16
	// maxRankedPackages is how many of stage 1's winners get a stage-2
	// question. Each one costs a round trip (~5s), so the tail is not worth
	// it — and the runner-up packages are already ordered by stage 1.
	maxRankedPackages = 3
)

// Instructions for the two questions. They are separate because they ask
// genuinely different things: which AREA the change touches, then which test
// inside that area would fail.
const (
	packageInstructions = "Which package's tests are most likely to catch a regression caused by this change? " +
		"Answer with the package whose behaviour the change alters, not the package that happens to share " +
		"a word with the changed file."
	testInstructions = "Which single test in this package is most likely to catch a regression caused by this change? " +
		"Answer with the test that would fail if the change were wrong, not the test that is most closely " +
		"named after the changed file."
)

// TypeSafeRanker ranks candidates with System One `choice` questions.
//
// # Why two stages
//
// Measured 2026-10-11 against the live Laya on :8093 (English checkpoint),
// same change text, same backend, four shapes of question:
//
//	one choice over 60 test names     best 0.025, nothing distinguishable
//	one choice over 41 test names     best 0.330, thin
//	one choice over 16 test names     best 0.562, and the winner is the
//	                                  test the change actually touches
//	one choice over 12 package names  best 1.000, and correct
//
// A `choice` distributes probability across its criteria, so its resolution
// is roughly 1/N: at N=60 every option sits near 0.02 and the ranking carries
// no information, while at N=12 a confident answer is genuinely 1.0. The
// package question also has the better options — `internal/gateway` is a name
// with meaning, `TestFormatChunkIsIndependent` is a name with a little.
//
// So the ranker asks the sharp question first (which package), then the finer
// one inside the top few (which test), and multiplies the two probabilities.
// A package stage 1 rejects sends all its candidates below every candidate of
// a package it accepts, which is exactly the behaviour the flat question
// could not produce.
//
// # What it still is not
//
// The per-package answer remains order-sensitive: on a reordered criteria map
// with equal-looking options the winner moves to whichever sorts first. That
// is why the result is reported as a distribution with a flatness verdict,
// and never as "run this one".
// is why the result is reported as a distribution with a flatness verdict,
// and never as "run this one".
type TypeSafeRanker struct {
	// Settings is the typesafe config block (endpoint, key, model, timeout).
	Settings typesafe.Settings
	// Timeout bounds the whole ranking (0 = DefaultRankTimeout).
	Timeout time.Duration
	// Evaluator, when set, replaces the one built from Settings. Tests inject
	// a fake here; production leaves it nil.
	Evaluator *typesafe.Evaluator
	// MaxPackages overrides maxRankedPackages (0 = default).
	MaxPackages int
}

// Rank asks the two-stage question set and returns the probability the
// backend gave each candidate. note names the checkpoint that answered (a
// Laya router picks per request, so "which model" is a fact the caller
// needs); err means "no ranking", never "no candidates".
func (r *TypeSafeRanker) Rank(ctx context.Context, change string, cands []Candidate) (map[string]float64, string, error) {
	if len(cands) == 0 {
		return nil, "", nil
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultRankTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ev := r.Evaluator
	if ev == nil {
		s := r.Settings.Config()
		s.Timeout = timeout
		ev = typesafe.NewEvaluator(s)
	}

	byPkg := groupByPackage(cands)
	order := packageOrder(byPkg, maxPackageOptions)

	var notes []string
	// One package in the closure: stage 1 would be a question with a single
	// option, which is not a question. Go straight to the tests.
	if len(order) == 1 {
		scores, note, err := r.rankTests(cctx, ev, change, byPkg[order[0]])
		if err != nil {
			return nil, note, err
		}
		return scores, note, nil
	}

	pkgProb, note, err := r.ask(cctx, ev, change, packageInstructions, packageOptions(order, byPkg))
	if err != nil {
		return nil, note, err
	}
	if note != "" {
		notes = append(notes, note)
	}

	maxPkgs := r.MaxPackages
	if maxPkgs <= 0 {
		maxPkgs = maxRankedPackages
	}
	top := topPackages(pkgProb, order, maxPkgs)

	// Stage 2: one question per top package, over that package's tests.
	scores := map[string]float64{}
	for _, p := range top {
		sub, n2, err := r.ask(cctx, ev, change, testInstructions, testOptions(capCandidates(byPkg[p], maxTestOptions)))
		if n2 != "" {
			notes = append(notes, n2)
		}
		if err != nil || len(sub) == 0 {
			// The package question still stands: split its probability
			// evenly across its tests rather than dropping them. A dropped
			// package would be a candidate the model cannot see, and the
			// candidate set is the half of this that must stay exact.
			share := pkgProb[p] / float64(len(byPkg[p]))
			for _, c := range byPkg[p] {
				scores[c.Key()] = share
			}
			continue
		}
		for _, c := range byPkg[p] {
			scores[c.Key()] = pkgProb[p] * sub[c.Key()]
		}
	}
	// Packages stage 1 did not pick still get their (low) probability, split
	// across their tests, so the ranking is a total order over every
	// candidate rather than a partial one.
	for _, p := range order {
		if _, done := scores[byPkg[p][0].Key()]; done {
			continue
		}
		share := pkgProb[p] / float64(len(byPkg[p]))
		for _, c := range byPkg[p] {
			scores[c.Key()] = share
		}
	}
	if len(scores) == 0 {
		return nil, strings.Join(notes, " / "), fmt.Errorf("typesafe rank: the backend returned no distribution")
	}
	return scores, strings.Join(notes, " / "), nil
}

// rankTests is the single-package path: one question over that package's
// tests, capped, with the package's own probability treated as 1. It is what
// the whole ranking degrades to when the closure holds one package — the
// stage-1 question would have a single option, which is not a question.
func (r *TypeSafeRanker) rankTests(ctx context.Context, ev *typesafe.Evaluator, change string, cands []Candidate) (map[string]float64, string, error) {
	sub, note, err := r.ask(ctx, ev, change, testInstructions, testOptions(capCandidates(cands, maxTestOptions)))
	if err != nil {
		return nil, note, err
	}
	if len(sub) == 0 {
	}
	// Every candidate gets a score, including the ones past the option cap:
	// they keep a floor share so the ranking stays total.
	scores := map[string]float64{}
	for _, c := range cands {
		if v, ok := sub[c.Key()]; ok {
			scores[c.Key()] = v
		}
	}
	floor := smallest(sub) / float64(len(cands)+1)
	for _, c := range cands {
		if _, ok := scores[c.Key()]; !ok {
			scores[c.Key()] = floor
		}
	}
	return scores, note, nil
}

// ask runs one `choice` question over a set of options. The criteria map is
// keyed by the option KEY and the option text is the criterion, so the answer
// can be mapped back to a candidate without parsing prose: a package
// question keys by import path, a test question by Candidate.Key().
func (r *TypeSafeRanker) ask(ctx context.Context, ev *typesafe.Evaluator, change, instructions string, opts []option) (map[string]float64, string, error) {
	criteria := make(map[string]any, len(opts))
	for _, o := range opts {
		criteria[o.key] = o.text
	}
	questions := map[string]any{
		"pick": map[string]any{
			"type":         "choice",
			"instructions": instructions,
			"criteria":     criteria,
		},
	}
	answers, info, err := ev.EvaluateWithInfo(ctx, change, questions)
	note := infoNote(info)
	if err != nil {
		return nil, note, fmt.Errorf("typesafe rank: %w", err)
	}
	return parseChoice(answers, "pick"), note, nil
}

// option is one criterion of a choice question: the key the answer comes back
// under, and the text the model judges.
type option struct {
	key  string
	text string
}

// testOptions builds the test-level options for one package.
func testOptions(cands []Candidate) []option {
	out := make([]option, 0, len(cands))
	for _, c := range cands {
		out = append(out, option{key: c.Key(), text: criterion(c)})
	}
	return out
}

// packageOptions builds the package-level options. The key is the import
// path (what the answer must map back to) and the text is the short name the
// model reads.
func packageOptions(order []string, byPkg map[string][]Candidate) []option {
	out := make([]option, 0, len(order))
	for _, p := range order {
		text := shortPackage(p)
		if len(byPkg[p]) > 0 {
			text = fmt.Sprintf("%s — %d test(s), e.g. %s", shortPackage(p), len(byPkg[p]), byPkg[p][0].Name)
		}
		out = append(out, option{key: p, text: text})
	}
	return out
}

// groupByPackage buckets candidates by import path, preserving each
// package's own order (which the selector already sorted by name).
func groupByPackage(cands []Candidate) map[string][]Candidate {
	out := map[string][]Candidate{}
	for _, c := range cands {
		out[c.Package] = append(out[c.Package], c)
	}
	return out
}

// packageOrder lists the packages worth asking about: direct (changed)
// packages first — they are the deterministic signal — then the rest, both
// sorted, capped at max.
func packageOrder(byPkg map[string][]Candidate, max int) []string {
	var direct, indirect []string
	for p, cs := range byPkg {
		if len(cs) > 0 && cs[0].Direct {
			direct = append(direct, p)
		} else {
			indirect = append(indirect, p)
		}
	}
	sort.Strings(direct)
	sort.Strings(indirect)
	out := append(direct, indirect...)
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// topPackages returns the max highest-probability packages, in order. Ties
// fall back to the deterministic order so a flat answer still produces a
// stable result.
func topPackages(prob map[string]float64, order []string, max int) []string {
	ranked := append([]string(nil), order...)
	pos := map[string]int{}
	for i, p := range order {
		pos[p] = i
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if prob[a] != prob[b] {
			return prob[a] > prob[b]
		}
		return pos[a] < pos[b]
	})
	if max > 0 && len(ranked) > max {
		ranked = ranked[:max]
	}
	return ranked
}

// capCandidates returns at most max candidates.
func capCandidates(cands []Candidate, max int) []Candidate {
	if max <= 0 || len(cands) <= max {
		return cands
	}
	return cands[:max]
}

// smallest is the lowest probability in a distribution, used as the floor a
// capped-out candidate keeps.
func smallest(m map[string]float64) float64 {
	out := 1.0
	for _, v := range m {
		if v < out {
			out = v
		}
	}
	return out
}

// toFloat reads a JSON number. encoding/json decodes into float64 for a
// map[string]any, but a backend is free to send a string or an int, and a
// ranking that silently drops an option because of that is worse than one
// that reads it.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// parseChoice pulls the option→probability map out of one answer. The wire
// shape is `{"choice": "<label>", "probabilities": {label: p, …}}`; the
// choice label alone is not enough to rank with (it is order-sensitive), so
// the distribution is what this reads. A backend that returns only the label
// still ranks: that option gets 1.0 and the rest 0, which degrades to "the
// single pick, everything else tied" — an honest rendering of a thin answer
// rather than a made-up spread.
func parseChoice(answers map[string]any, id string) map[string]float64 {
	raw, ok := answers[id]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]float64{}
	if probs, ok := m["probabilities"].(map[string]any); ok {
		for k, v := range probs {
			if f, ok := toFloat(v); ok {
				out[k] = f
			}
		}
	}
	if len(out) == 0 {
		if label, ok := m["choice"].(string); ok && label != "" {
			out[label] = 1
		}
	}
	return out
}

// infoNote renders the backend footer: which checkpoint answered, and what
// the call cost. A Laya router chooses the checkpoint per request by script,
// so the requested model and the answering one routinely differ — and
// "answered by multilingual" explains an odd ranking in a way that no amount
// of staring at the distribution does.
func infoNote(info typesafe.Info) string {
	var parts []string
	if m := strings.TrimSpace(info.Model); m != "" {
		parts = append(parts, "ranked by "+m)
	}
	if info.Usage.LocalMS > 0 {
		parts = append(parts, fmt.Sprintf("%.0fms", info.Usage.LocalMS))
	}
	if n := info.Usage.InputTokens + info.Usage.OutputTokens; n > 0 {
		parts = append(parts, fmt.Sprintf("%d tokens", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " / ")
}

// criterion is the text the model judges one candidate by: the test's name,
// its doc comment, and its package. The doc comment is the only description
// of intent available without running the test, and the measurement says the
// ranking tracks it — a criterion of just the test name produced the same
// ranking as name+doc on the same input, so the doc is what carries the
// signal when the names are opaque.
func criterion(c Candidate) string {
	var b strings.Builder
	b.WriteString(c.Name)
	if c.Doc != "" {
		b.WriteString(" — ")
		b.WriteString(c.Doc)
	}
	b.WriteString(" (")
	b.WriteString(shortPackage(c.Package))
	if c.Direct {
		b.WriteString(", in a changed package")
	}
	b.WriteString(")")
	return b.String()
}

// shortPackage trims the module prefix so a criterion reads as
// "internal/gateway" rather than a full import path — prompt budget, and the
// model has no use for the module name.
func shortPackage(imp string) string {
	for _, marker := range []string{"/internal/", "/cmd/", "/pkg/"} {
		if i := strings.Index(imp, marker); i >= 0 {
			return imp[i+1:]
		}
	}
	return imp
}

// Distribution summarises a ranking for the model-facing report: the top n
// candidates with their probabilities, and a spread note that says whether
// the answer is decisive or flat. The second half is the point — the
// measurement above says a `choice` over many options often is flat, and a
// caller that reads a flat distribution as a ranking wastes the wall time
// this tool exists to save.
func Distribution(cands []Candidate, n int) string {
	if len(cands) == 0 {
		return ""
	}
	if n <= 0 || n > len(cands) {
		n = len(cands)
	}
	var b strings.Builder
	scored := 0
	for _, c := range cands {
		if c.Score > 0 {
			scored++
		}
	}
	if scored > 0 {
		top := cands[0].Score
		fmt.Fprintf(&b, "ranking: %d/%d candidates scored, best %.3f\n", scored, len(cands), top)
		if top < flatCeiling {
			b.WriteString("  (flat: no candidate stands out, so treat the order as a tie-break, not a verdict)\n")
		}
	}
	for _, c := range cands[:n] {
		fmt.Fprintf(&b, "  %.3f  %s  %s\n", c.Score, c.Name, shortPackage(c.Package))
	}
	return b.String()
}

// flatCeiling is the probability below which the top of a distribution is
// not a signal. Measured: a flat choice over 60 tests peaked at 0.025, and
// one over 41 real gateway tests at 0.33, so anything under a third is
// indistinguishable from noise on this backend — and saying so is the honest
// report.
const flatCeiling = 0.35
