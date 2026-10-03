package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/FreePeak/xdev/internal/codegraph"
)

// ImpactToolName is the model-facing name.
const ImpactToolName = "impact"

// ImpactIndex is the one-line catalog entry. It teaches the *when*, not just
// the what: a model that only knows the tool exists will not reach for it
// before the edit it is meant to precede.
const ImpactIndex = "who depends on this symbol or file (callers/callees/blast radius) — check before editing widely-used code"

// maxImpactRows caps one answer. A hub symbol has hundreds of callers; the
// model needs the shape of the blast radius and a count, not the file.
const maxImpactRows = 25

// ImpactTool answers "what does changing this touch?" from the code graph.
//
// It exists because of the failure this repo has already measured once: an
// agent that cannot see dependents edits a shared helper and only discovers
// the fallout from a build several turns later. The graph answers it in one
// call, and the answer is advisory — it is a map, so the model confirms with
// read/lsp before asserting (the prompt says so too).
type ImpactTool struct {
	// Graph is the LeanKG client. A nil client makes the tool report itself
	// as unconfigured rather than failing obscurely.
	Graph *codegraph.Client
	CWD   string
}

var _ Tool = (*ImpactTool)(nil)

func (t *ImpactTool) Name() string { return ImpactToolName }

func (t *ImpactTool) Description() string {
	return "Code-graph impact: resolve a symbol or file, then list its callers, callees, dependents or dependencies from LeanKG. Use before editing shared code."
}

func (t *ImpactTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "required": ["symbol"],
  "properties": {
    "symbol": {"type": "string", "description": "Symbol name (preferred), qualified name, or a file path. Resolved through the code graph's own ranking."},
    "relation": {
      "type": "string",
      "enum": ["callers", "callees", "impact", "path"],
      "default": "callers",
      "description": "callers = who invokes it; callees = what it invokes; impact = transitive blast radius; path = callers reaching a target (requires \"to\")."
    },
    "depth": {"type": "integer", "minimum": 1, "maximum": 5, "default": 2, "description": "Depth for relation=impact."},
    "to": {"type": "string", "description": "Target symbol for relation=path."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 25, "default": 20}
  }
}`)
}

type impactArgs struct {
	Symbol   string `json:"symbol"`
	Relation string `json:"relation"`
	Depth    int    `json:"depth"`
	To       string `json:"to"`
	Limit    int    `json:"limit"`
}

func (t *ImpactTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a impactArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return Result{}, fmt.Errorf("impact: invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Symbol) == "" {
		return Result{IsError: true, Text: "impact: symbol is required"}, nil
	}
	if t.Graph == nil {
		return Result{IsError: true, Text: "impact: code graph is not configured — continue with grep/lsp"}, nil
	}
	if a.Relation == "" {
		a.Relation = "callers"
	}
	if a.Depth <= 0 {
		a.Depth = 2
	}
	if a.Depth > 5 {
		a.Depth = 5
	}
	if a.Limit <= 0 || a.Limit > maxImpactRows {
		a.Limit = maxImpactRows
	}

	// A file path is not an element name, so it takes a different route: the
	// graph's answer about a file is its importers, which is what "what
	// depends on this file" actually means.
	if looksLikePath(a.Symbol) {
		return t.fileImpact(ctx, a)
	}

	qn, err := t.resolve(ctx, a.Symbol, a.Limit)
	if err != nil {
		return impactFailure(err), nil
	}
	switch a.Relation {
	case "path":
		if strings.TrimSpace(a.To) == "" {
			return Result{IsError: true, Text: `impact: relation "path" needs "to" (the target symbol)`}, nil
		}
		toQN, terr := t.resolve(ctx, a.To, 1)
		if terr != nil {
			return impactFailure(terr), nil
		}
		body, qerr := t.Graph.Query(ctx, codegraph.Request{
			Action: "path", Query: qn, Limit: a.Limit,
			Args: map[string]any{"to": toQN},
		})
		if qerr != nil {
			return impactFailure(qerr), nil
		}
		return Result{Text: renderGraphAnswer("path "+a.Symbol+" -> "+a.To, body)}, nil
	case "callers", "callees", "impact":
		args := map[string]any{}
		if a.Relation == "impact" {
			args["depth"] = a.Depth
		}
		body, qerr := t.Graph.Query(ctx, codegraph.Request{
			Action: a.Relation, Query: qn, Limit: a.Limit, Args: args,
		})
		if qerr != nil {
			return impactFailure(qerr), nil
		}
		return Result{Text: renderGraphAnswer(a.Relation+" of "+qn, body)}, nil
	default:
		return Result{IsError: true, Text: "impact: unknown relation " + a.Relation}, nil
	}
}

// resolve turns a name into the qualified name the graph verbs require. The
// server refuses a bare name and says so in guidance, which is the right
// behavior for a model and an unhelpful one for a tool, so the hop is here.
func (t *ImpactTool) resolve(ctx context.Context, symbol string, limit int) (string, error) {
	if strings.Contains(symbol, "::") {
		return symbol, nil
	}
	hits, err := t.Graph.Locate(ctx, symbol, limit)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "", &codegraph.UnavailableError{Reason: fmt.Sprintf("no element named %q in this repository", symbol)}
	}
	for _, h := range hits {
		if h.QualifiedName != "" {
			return h.QualifiedName, nil
		}
	}
	return "", &codegraph.UnavailableError{Reason: fmt.Sprintf("no element named %q in this repository", symbol)}
}

// fileImpact answers "what imports this file", which is the file-level form of
// the same question.
func (t *ImpactTool) fileImpact(ctx context.Context, a impactArgs) (Result, error) {
	rel := a.Symbol
	if filepath.IsAbs(rel) && t.CWD != "" {
		if r, err := filepath.Rel(t.CWD, rel); err == nil {
			rel = r
		}
	}
	rel = filepath.ToSlash(rel)
	hits, err := t.Graph.Locate(ctx, rel, a.Limit)
	if err != nil {
		return impactFailure(err), nil
	}
	rows := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.File == rel || h.QualifiedName == "" {
			continue
		}
		rows = append(rows, fmt.Sprintf("  %s (%s) %s", h.QualifiedName, h.Type, where(h.File, h.Line)))
	}
	if len(rows) == 0 {
		return Result{Text: fmt.Sprintf("code graph: nothing indexed that references %s", rel)}, nil
	}
	if len(rows) > maxImpactRows {
		rows = rows[:maxImpactRows]
	}
	return Result{Text: fmt.Sprintf("code graph references to %s (%d shown):\n%s",
		rel, len(rows), strings.Join(rows, "\n"))}, nil
}

// impactFailure turns a transport failure into an actionable tool result. The
// session continues; the model is told what to do instead.
func impactFailure(err error) Result {
	if err == nil {
		return Result{Text: "impact: no answer"}
	}
	var unavail *codegraph.UnavailableError
	if errors.As(err, &unavail) {
		return Result{Text: unavail.Error()}
	}
	if errors.Is(err, codegraph.ErrUnconfigured) {
		return Result{Text: "impact: no code graph configured — continue with grep/lsp"}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The caller aborted or the timeout fired. That is not a broken
		// graph, and saying so would send the model hunting a dead server.
		return Result{IsError: true, Text: "impact: " + err.Error()}
	}
	return Result{Text: "impact: code graph unavailable — continue with grep/lsp (" + err.Error() + ")"}
}

// renderGraphAnswer keeps the server's own guidance and rows, dropping the
// bookkeeping fields that mean nothing to the model.
func renderGraphAnswer(head string, body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return head + "\n\n" + string(body)
	}
	var b strings.Builder
	b.WriteString(head)
	if g, ok := m["guidance"].(string); ok && strings.TrimSpace(g) != "" {
		b.WriteString("\n" + g)
	}
	rows := 0
	for _, key := range []string{"callers", "callees", "impact", "hits", "path"} {
		v, ok := m[key]
		if !ok || v == nil {
			continue
		}
		items, ok := v.([]any)
		if !ok || len(items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%d %s:", len(items), key)
		for i, it := range items {
			if i >= maxImpactRows {
				fmt.Fprintf(&b, "\n  (+%d more)", len(items)-maxImpactRows)
				break
			}
			row, _ := it.(map[string]any)
			if row == nil {
				continue
			}
			fmt.Fprintf(&b, "\n  %v", firstString(row, "qualified_name", "name", "content", "callee", "caller"))
		}
		rows += len(items)
	}
	if rows == 0 {
		b.WriteString("\n(no results)")
	}
	return b.String()
}

func where(file string, line int) string {
	if line <= 0 {
		return file
	}
	return fmt.Sprintf("%s:%d", file, line)
}

func firstString(row map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := row[k].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

func looksLikePath(s string) bool {
	return strings.Contains(s, "/") || strings.HasSuffix(s, ".go") ||
		strings.HasSuffix(s, ".ts") || strings.HasSuffix(s, ".py")
}
