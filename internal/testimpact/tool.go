package testimpact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
	"github.com/FreePeak/xdev/internal/typesafe"
)

const ToolName = "regression_tests"

// Tool answers "which of the existing tests should I re-run for this
// change?" — the deterministic candidate set from the dependency graph, and
// an order from the TypeSafe/Laya judgment over that set.
//
// It selects; it never runs. The session owns execution (via bash), which
// keeps this tool's contract narrow: no process lifecycle, no test timeouts,
// no flaky-test semantics, and no second place that decides what "the tests
// passed" means.
type Tool struct {
	// CWD is the repository root the tool works in.
	CWD string
	// Settings configures the ranking backend (typesafe.baseUrl etc.).
	Settings Settings
	// MaxCandidates caps the candidate list (0 = DefaultMaxCandidates).
	MaxCandidates int
	// Timeout bounds one subprocess (0 = DefaultCommandTimeout).
	Timeout time.Duration
	// Rank disables the model ranking when explicitly false. Nil means
	// "rank" — a configured typesafe endpoint is the whole point of the
	// tool; an unreachable one degrades to the deterministic order.
	Rank *bool

	// ranker, when set, replaces the built-in TypeSafe ranker. Tests inject a
	// fake here so no endpoint is needed.
	ranker Ranker
	// run, when set, replaces the process runner. Tests inject fixtures.
	run runner
}

// Settings is the ranking backend's configuration: the TypeSafe/Laya
// endpoint, credentials, model and timeout. It is the typesafe package's own
// type, so cmd/xdev hands over settings.TypeSafeConfig() without a
// field-by-field mapping (the same reason internal/config aliases it).
type Settings = typesafe.Settings

// NewTool builds the tool from the session cwd and the typesafe settings
// block.
func NewTool(cwd string, s Settings) *Tool { return &Tool{CWD: cwd, Settings: s} }

func (t *Tool) Name() string { return ToolName }

func (t *Tool) Description() string {
	// One terse line: the system prompt carries a budgeted tool recap (PRD §1
	// Goal 4) and degrades to name-only listing once it is spent, so the
	// usage detail ships in the schema instead.
	return "select which existing tests to re-run for the current change: the dependency graph gives the exact candidate set, a TypeSafe/Laya judgment ranks it; returns the ranked list and the go test command, and never runs it"
}

func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "base": {"type": "string", "description": "git revision to diff against (default HEAD); the diff is base..worktree, so uncommitted edits count"},
    "files": {"type": "array", "items": {"type": "string"}, "description": "changed files, when the caller already knows them (skips git); paths relative to the repo root"},
    "max_candidates": {"type": "integer", "description": "cap on the candidate list (default 60, max 200) — the ranking degrades past ~90 options"},
    "no_rank": {"type": "boolean", "description": "skip the model ranking and return the deterministic dependency order"}
  }
}`)
}

type toolArgs struct {
	Base          string   `json:"base"`
	Files         []string `json:"files"`
	MaxCandidates int      `json:"max_candidates"`
	NoRank        bool     `json:"no_rank"`
}

// Execute runs one selection and renders it for the model.
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a toolArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return tool.Result{Text: ToolName + ": malformed arguments: " + err.Error(), IsError: true}, nil
		}
	}
	if a.MaxCandidates < 0 {
		return tool.Result{Text: fmt.Sprintf("%s: max_candidates must be non-negative, got %d", ToolName, a.MaxCandidates), IsError: true}, nil
	}
	if a.MaxCandidates > MaxCandidatesCeiling {
		return tool.Result{Text: fmt.Sprintf("%s: max_candidates %d exceeds the ceiling of %d", ToolName, a.MaxCandidates, MaxCandidatesCeiling), IsError: true}, nil
	}
	if strings.TrimSpace(t.CWD) == "" {
		return tool.Result{Text: ToolName + ": no working directory configured", IsError: true}, nil
	}

	opts := Options{
		CWD:           t.CWD,
		Base:          strings.TrimSpace(a.Base),
		Files:         a.Files,
		MaxCandidates: a.MaxCandidates,
		Timeout:       t.Timeout,
		run:           t.run,
	}
	if opts.MaxCandidates == 0 {
		opts.MaxCandidates = t.MaxCandidates
	}
	opts.Ranker = t.pickRanker(a.NoRank)

	res, err := Select(ctx, opts)
	if err != nil {
		return tool.Result{Text: ToolName + ": " + err.Error(), IsError: true}, nil
	}
	return tool.Result{Text: t.render(res), Details: detailsOf(res)}, nil
}

// pickRanker resolves the ranking backend: an injected one wins (tests), then
// no_rank, then the TypeSafe/Laya ranker.
func (t *Tool) pickRanker(noRank bool) Ranker {
	if t.ranker != nil {
		return t.ranker
	}
	if noRank {
		return nil
	}
	if t.Rank != nil && !*t.Rank {
		return nil
	}
	return &TypeSafeRanker{Settings: t.Settings}
}

// render is the model-facing report. The order of the sections is the order
// of trust: what is certain (the closure), then what is judged (the
// ranking), then what to do about it (the command), then the caveats.
func (t *Tool) render(res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d changed package(s), %d package(s) in the dependency closure, %d test(s)\n",
		ToolName, len(res.ChangedPackages), len(res.DepPackages), res.TotalTests)
	for _, n := range res.Notes {
		b.WriteString("  note: " + n + "\n")
	}
	if len(res.Candidates) == 0 {
		b.WriteString("no candidates selected\n")
		return b.String()
	}

	b.WriteString("\n")
	b.WriteString(Distribution(res.Candidates, 12))

	switch {
	case res.Ranked:
		fmt.Fprintf(&b, "ranked: %d candidate(s) scored", len(res.Candidates))
		if res.RankNote != "" {
			b.WriteString(" (" + res.RankNote + ")")
		}
		b.WriteString("\n")
		b.WriteString("  the distribution above is a judgment, not a fact: the candidate set is exact, the order is not\n")
	default:
		b.WriteString("unranked — the dependency order stands")
		if res.RankNote != "" {
			b.WriteString(" (" + res.RankNote + ")")
		}
		b.WriteString("\n")
	}
	if res.Truncated {
		fmt.Fprintf(&b, "  %d of %d candidate(s) shown (max_candidates cap; direct packages first)\n",
			len(res.Candidates), res.TotalTests)
	}
	if res.Command != "" {
		b.WriteString("\nrun (this tool does not):\n")
		for _, line := range strings.Split(res.Command, "\n") {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

// detailsOf is the structured half of the result, for the TUI and the session
// store. It carries the same list in a shape a caller can act on without
// parsing the prose.
func detailsOf(res *Result) map[string]any {
	top := res.Candidates
	if len(top) > 20 {
		top = top[:20]
	}
	rows := make([]map[string]any, 0, len(top))
	for _, c := range top {
		rows = append(rows, map[string]any{
			"package": c.Package,
			"name":    c.Name,
			"file":    c.File,
			"score":   c.Score,
			"direct":  c.Direct,
		})
	}
	return map[string]any{
		"changedPackages": res.ChangedPackages,
		"depPackages":     len(res.DepPackages),
		"totalTests":      res.TotalTests,
		"truncated":       res.Truncated,
		"ranked":          res.Ranked,
		"rankNote":        res.RankNote,
		"command":         res.Command,
		"candidates":      rows,
	}
}

// compile-time interface assertion.
var _ tool.Tool = (*Tool)(nil)
