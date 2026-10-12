// Package testimpact answers one question for a coding session: given the
// change I just made, which existing tests should I re-run?
//
// The answer has two halves, and they are deliberately different kinds of
// claim:
//
//   - CANDIDATES are deterministic. The changed files are mapped to Go
//     packages, `go list` gives the reverse-dependency closure (every
//     package that transitively imports a changed one), and every Test/Fuzz
//     function in those packages is a candidate. This half is free, exact,
//     and reproducible: a test in the closure IS affected by the change in
//     the only sense the compiler can prove, and no test outside it can be.
//
//   - ORDER is a judgment. The dependency closure is usually far too large
//     to run (a change to internal/tool reaches 26 of this repo's 39
//     packages), so something has to say which of those candidates is worth
//     the wall time first. That is what the TypeSafe/Laya call does, in two
//     stages — which package, then which test inside it — because a single
//     `choice` over 60 tests is measurably flat. ranker.go carries the
//     measurements that fix the shape.
//
// The judgment half is advisory, and the package treats it that way. A
// backend that is unreachable, slow, or answers garbage does not fail the
// call — it degrades to the deterministic order and says so in the footer.
// The candidates are never wrong; only their order is a guess.
//
// What the measurement says about that guess (2026-10-11, live Laya on
// :8093, English checkpoint):
//
//   - The winner tracks the change text. A change to the Telegram poll loop
//     ranks the poll/lease tests; a change to reply formatting ranks the
//     chunking tests; a change to README.md ranks nothing in particular.
//   - The winner is also whoever sorts first among near-equal options.
//     Reordering the same criteria moves the winner from one option to
//     another with the same probabilities in a different order. A `choice`
//     is a weak instrument, so its answer is reported as a distribution and
//     never as "run this one".
//   - Per-candidate `noul` questions are worse, not better: one call with a
//     question per test returned 0.177 for the poll test and 0.131 for the
//     markdown test — a flat spread that ranks nothing. One `choice` is both
//     cheaper and more informative, and two staged `choice`s are better
//     still.
//   - A `score` question per candidate saturates the backend: four of them
//     in one call made the live server answer "server busy" for a minute.
//     Hence the single-`choice`-per-stage design.
package testimpact

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Defaults. Every bound here exists because the tool runs inside an agent
// turn: an unbounded subprocess or an unbounded candidate list turns one
// tool call into a stalled session.
const (
	// DefaultMaxCandidates caps how many tests are inventoried and ranked.
	// Measured: one choice call over ~40 options takes 3-7s and still shows
	// structure; over ~90 it is ~7s and the distribution flattens to noise.
	DefaultMaxCandidates = 60
	// MaxCandidatesCeiling clamps the caller's max_candidates argument.
	MaxCandidatesCeiling = 200

	// DefaultCommandTimeout bounds ONE subprocess (git, go list).
	DefaultCommandTimeout = 60 * time.Second

	// outputCap bounds captured subprocess output. `go list -e -json ./...`
	// on this repo is ~285 KB; 8 MiB is the bash tool's own ceiling and
	// leaves room for a much larger module.
	outputCap = 8 << 20

	// maxDiffBytes caps the change text handed to the model. The state
	// string is the prompt, and a whole-file rewrite of a large file would
	// otherwise be the whole prompt.
	maxDiffBytes = 8 << 10

	// maxCriterionRunes caps one candidate's criterion text.
	maxCriterionRunes = 240
)

// Options configures a selection run.
type Options struct {
	// CWD is the repository root the tool works in.
	CWD string
	// MaxCandidates caps the inventoried candidate list (0 = default).
	MaxCandidates int
	// Timeout bounds one subprocess (0 = default).
	Timeout time.Duration
	// Base is the git revision the change is measured against ("" = HEAD).
	// The diff is Base..worktree, so both committed and uncommitted edits
	// count.
	Base string
	// Files, when non-empty, replaces the diff: the caller already knows
	// which files changed and does not want a second opinion from git.
	Files []string

	// Ranker orders candidates. nil = no ranking (deterministic order).
	Ranker Ranker
	// run executes one subprocess; nil = the real runner. Tests inject
	// fixtures here so no git or go toolchain is needed.
	run runner
}

// runner executes argv in dir and returns stdout. It is the single process
// seam: everything this package shells out to goes through it.
type runner func(ctx context.Context, dir string, argv ...string) (string, error)

// Candidate is one existing test that could be affected by a change.
type Candidate struct {
	// Package is the import path of the package the test lives in.
	Package string
	// Name is the test function name (Test… or Fuzz…).
	Name string
	// File is the test file's base name, relative to the package dir.
	File string
	// PkgDir is the package's directory relative to the repository root
	// ("internal/tool"), the form a `go test` invocation takes. Empty when
	// the graph could not place the package.
	PkgDir string
	// Doc is the test's leading comment block, collapsed to one line. It is
	// the only thing the ranker has to judge a test by beyond its name.
	Doc string
	// Direct is true when the candidate's own package was changed (as
	// opposed to reaching a changed package through imports). A direct hit
	// is the strongest deterministic signal available, and it survives
	// ranking: the report always puts direct candidates first within their
	// score band.
	Direct bool
	// Score is the ranker's probability for this candidate, 0 when nothing
	// ranked it.
	Score float64
}

// Result is one selection run.
type Result struct {
	// Candidates is the ranked list, best first.
	Candidates []Candidate
	// ChangedPackages are the import paths of the changed files' packages.
	ChangedPackages []string
	// DepPackages are the packages in the reverse-dependency closure.
	DepPackages []string
	// TotalTests is how many tests the closure contains before the cap.
	TotalTests int
	// Truncated is true when the cap dropped candidates.
	Truncated bool
	// Ranked is true when the ranker answered and the order came from it.
	Ranked bool
	// RankNote explains the ranking outcome (the backend that answered, the
	// reason there is no ranking, or the error that degraded it).
	RankNote string
	// Command is the suggested `go test` invocation for the top candidates,
	// ready to run as-is. It is a suggestion, never an execution: this tool
	// selects, it does not run.
	Command string
	// Notes are diagnostics that belong in the result (skipped paths, a
	// toolchain that is missing, a diff that was empty).
	Notes []string
}

// Ranker orders candidates against the change. It is a one-method seam so
// the ranking backend (TypeSafe/Laya today) can be swapped or faked in tests
// without the selector knowing what a System One question is.
type Ranker interface {
	// Rank returns a score per candidate key (Package + "::" + Name).
	// Implementations return an error rather than a partial answer when the
	// backend cannot be reached; the caller degrades.
	Rank(ctx context.Context, change string, cands []Candidate) (map[string]float64, string, error)
}

// Select runs the whole selection: diff → packages → closure → candidates →
// ranking.
func Select(ctx context.Context, opts Options) (*Result, error) {
	if strings.TrimSpace(opts.CWD) == "" {
		return nil, errors.New("testimpact: cwd is required")
	}
	run := opts.run
	if run == nil {
		run = execRunner
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	max := opts.MaxCandidates
	if max <= 0 {
		max = DefaultMaxCandidates
	}
	if max > MaxCandidatesCeiling {
		max = MaxCandidatesCeiling
	}

	res := &Result{}

	files, notes, err := changedFiles(ctx, run, opts, timeout)
	if err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes, notes...)
	if len(files) == 0 {
		res.Notes = append(res.Notes, "no changed files against "+
			baseLabel(opts.Base)+" — nothing to select")
		return res, nil
	}

	graph, err := loadGraph(ctx, run, opts.CWD, timeout)
	if err != nil {
		return nil, err
	}

	changed := graph.packagesForFiles(files)
	if len(changed) == 0 {
		res.Notes = append(res.Notes, "the changed files are not in any package `go list` reports "+
			"(a non-Go file, or a file in an ignored directory) — no Go test can be selected from them")
		return res, nil
	}
	res.ChangedPackages = changed

	closure := graph.reverseClosure(changed)
	res.DepPackages = closure

	cands, total := graph.candidates(closure, changed, max)
	res.Candidates = cands
	res.TotalTests = total
	res.Truncated = total > len(cands)

	if len(cands) == 0 {
		res.Notes = append(res.Notes, "the dependency closure of the changed packages contains no test functions")
		return res, nil
	}

	change := changeSummary(files, opts)
	if opts.Ranker != nil {
		scores, note, rerr := opts.Ranker.Rank(ctx, change, cands)
		res.RankNote = note
		switch {
		case rerr != nil:
			// Degrading is the designed behaviour, not a failure: the
			// candidate list is still exact, only the order is arbitrary.
			res.RankNote = "unranked: " + rerr.Error()
		case len(scores) > 0:
			applyScores(res.Candidates, scores)
			res.Ranked = true
		}
	}
	if !res.Ranked {
		sortCandidates(res.Candidates)
	}

	res.Command = command(res.Candidates, 10)
	return res, nil
}

// changedFiles resolves the change: an explicit file list when the caller
// gave one, else git. The diff is base..worktree so uncommitted edits count
// — a session that just edited a file has not committed it yet, and a
// selection that only saw HEAD would answer for the wrong change.
func changedFiles(ctx context.Context, run runner, opts Options, timeout time.Duration) ([]string, []string, error) {
	if len(opts.Files) > 0 {
		out := make([]string, 0, len(opts.Files))
		for _, f := range opts.Files {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, filepath.ToSlash(f))
			}
		}
		return out, nil, nil
	}
	base := strings.TrimSpace(opts.Base)
	if base == "" {
		base = "HEAD"
	}
	argv := append([]string{"git"}, "-C", opts.CWD, "diff", "--name-only", base, "--")
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := run(cctx, opts.CWD, argv...)
	if err != nil {
		return nil, nil, fmt.Errorf("testimpact: git diff: %w", err)
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, filepath.ToSlash(line))
		}
	}
	return files, nil, nil
}

// baseLabel names the diff base for a note.
func baseLabel(base string) string {
	if strings.TrimSpace(base) == "" {
		return "HEAD"
	}
	return base
}

// execRunner is the production process seam: discrete argv (never a shell),
// the caller's cwd, bounded output, and the context deadline as the only
// timeout.
func execRunner(ctx context.Context, dir string, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("no command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	out := &cappedBuf{limit: outputCap}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.buf.String(), ctx.Err()
		}
		msg := strings.TrimSpace(out.buf.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.buf.String(), fmt.Errorf("%s: %s", argv[0], capText(msg, 400))
	}
	return out.buf.String(), nil
}

// capText clips s to n bytes on a rune boundary.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// cappedBuf keeps at most limit bytes (the bash tool's bounded-everything
// rule; the same type the github tool uses, kept local so this package
// imports nothing from internal/tool).
type cappedBuf struct {
	buf   strings.Builder
	limit int
}

func (c *cappedBuf) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.limit - c.buf.Len(); room > 0 {
		if n > room {
			p = p[:room]
		}
		c.buf.Write(p)
	}
	return n, nil
}

// changeSummary is the state string the ranker judges against: the changed
// files plus, when it is cheap, the diff itself. The diff is what makes the
// ranking track the change instead of the file names, and it is capped
// because it is the prompt.
func changeSummary(files []string, opts Options) string {
	var b strings.Builder
	b.WriteString("changed files:\n")
	for _, f := range files {
		b.WriteString("  " + f + "\n")
	}
	if len(opts.Files) > 0 {
		return b.String()
	}
	base := strings.TrimSpace(opts.Base)
	if base == "" {
		base = "HEAD"
	}
	// The diff is best-effort: the file list alone is a usable state, so a
	// diff that is slow, empty or unavailable is not an error.
	run := opts.run
	if run == nil {
		run = execRunner
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := run(ctx, opts.CWD, "git", "-C", opts.CWD, "diff", "--unified=0", base, "--")
	if err != nil {
		return b.String()
	}
	if d := strings.TrimSpace(out); d != "" {
		b.WriteString("\ndiff:\n")
		b.WriteString(capText(d, maxDiffBytes))
		b.WriteString("\n")
	}
	return b.String()
}

// applyScores writes the ranker's probabilities onto the candidates.
func applyScores(cands []Candidate, scores map[string]float64) {
	for i := range cands {
		if v, ok := scores[cands[i].key()]; ok {
			cands[i].Score = v
		}
	}
	sortCandidates(cands)
}

// sortCandidates orders by score (descending), then by the deterministic
// signal: a direct hit before a transitive one, then package, then name.
// Sorting the deterministic ties is what keeps the output stable across
// runs when the ranker is flat — and the measurement says it often is.
func sortCandidates(cands []Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Direct != b.Direct {
			return a.Direct
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.Name < b.Name
	})
}

// key is the identity the ranker and the scorer agree on. The package is
// part of it because test names are not unique across packages
// (TestSessionSurvivesRestart exists in more than one).
func (c Candidate) key() string { return c.Package + "::" + c.Name }

// Key is the candidate's stable identity, exported for ranker
// implementations that need to build their own maps.
func (c Candidate) Key() string { return c.key() }

// command builds the suggested `go test` invocation for the top n
// candidates. It is a string, not a process: the session runs it (or not),
// which is the whole point of "select only".
//
// One package is one exact invocation: `go test ./internal/tool -run
// '^(TestA|TestB)$'`. Several packages cannot share a `-run` regex in one
// command — the flag is per package — so the honest answer is one line per
// package, each with its own regex, rather than a union regex that silently
// runs the wrong tests in every package but the first.
func command(cands []Candidate, n int) string {
	if len(cands) == 0 {
		return ""
	}
	if n <= 0 || n > len(cands) {
		n = len(cands)
	}
	byPkg := map[string][]string{}
	var order []string
	for _, c := range cands[:n] {
		dir := c.PkgDir
		if dir == "" {
			dir = c.Package
		}
		if _, seen := byPkg[dir]; !seen {
			order = append(order, dir)
		}
		byPkg[dir] = append(byPkg[dir], c.Name)
	}
	sort.Strings(order)
	lines := make([]string, 0, len(order))
	for _, dir := range order {
		names := byPkg[dir]
		sort.Strings(names)
		lines = append(lines, "go test ./"+dir+" -run '^("+strings.Join(names, "|")+")$'")
	}
	return strings.Join(lines, "\n")
}
