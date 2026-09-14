package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// Goal mode (M11 #40): one session-scoped objective with an optional token
// budget, tracked across turns. The goal tool is the only writer; the agent
// loop reads the same state at turn boundaries (reminder injection, budget
// accounting). Every state change appends a goal_updated session entry — the
// persisted event — and fires the live event hook.
//
// Completion is explicit and evidence-gated: a goal completes only after at
// least one evidence note has been recorded, and spending the budget flips it
// to budget_exhausted — never an implicit completion.

// Goal statuses.
const (
	GoalActive          = "active"
	GoalCompleted       = "completed"
	GoalDropped         = "dropped"
	GoalBudgetExhausted = "budget_exhausted"
)

// goalReminderObjectiveCap bounds the objective text in a per-turn reminder:
// the reminder rides in every turn's context, so it must stay small.
const goalReminderObjectiveCap = 240

// Goal is one tracked objective.
type Goal struct {
	Objective string
	// TokenBudget caps the goal's token spend; 0 means unbounded.
	TokenBudget int64
	Created     time.Time
	Status      string
}

// GoalView is a Goal plus its live accounting (what the tool and /goal show).
type GoalView struct {
	Goal
	Spent    int64
	Evidence []string
}

// Remaining reports the budget left, or 0 when unbounded/exhausted.
func (v GoalView) Remaining() int64 {
	if v.TokenBudget <= 0 || v.Spent >= v.TokenBudget {
		return 0
	}
	return v.TokenBudget - v.Spent
}

// GoalHook is the optional TurnHooks extension for goal events: a hooks
// implementation that also implements GoalHook receives every goal state
// change. Separate from TurnHooks so existing implementations stay valid.
type GoalHook interface{ OnGoalUpdated(g Goal) }

// GoalNotify adapts a TurnHooks to a goal event callback (nil when the hooks
// do not implement GoalHook).
func GoalNotify(h TurnHooks) func(Goal) {
	if gh, ok := h.(GoalHook); ok {
		return gh.OnGoalUpdated
	}
	return nil
}

// GoalState is the session-scoped goal holder. Safe for concurrent use: the
// goal tool runs on the tool pool while Run reads state at turn boundaries.
type GoalState struct {
	mu       sync.Mutex
	store    *session.Store
	current  *Goal
	evidence []string
	spent    int64
	onUpdate func(Goal)
}

// NewGoalState returns an unbound state; Bind attaches (and hydrates from) a
// session store.
func NewGoalState(store *session.Store) *GoalState {
	g := &GoalState{}
	if store != nil {
		g.Bind(store)
	}
	return g
}

// Bind attaches the session store and hydrates the goal from its
// goal_updated entries (the last snapshot wins). Called on session open and
// again on every session switch (/resume, /new), where the new session starts
// with its own — usually empty — goal.
func (g *GoalState) Bind(store *session.Store) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.store = store
	g.current, g.evidence, g.spent = nil, nil, 0
	if store == nil {
		return
	}
	for _, e := range store.Entries() {
		gu, ok := e.(*session.GoalUpdatedEntry)
		if !ok {
			continue
		}
		g.current = &Goal{
			Objective:   gu.Goal.Objective,
			TokenBudget: gu.Goal.TokenBudget,
			Created:     gu.Goal.Created,
			Status:      gu.Goal.Status,
		}
		g.evidence = append([]string(nil), gu.Goal.Evidence...)
		g.spent = gu.Goal.Spent
	}
}

// SetOnUpdate installs the live event callback fired on every state change
// (nil clears). The loop binds it from the active TurnHooks.
func (g *GoalState) SetOnUpdate(fn func(Goal)) {
	g.mu.Lock()
	g.onUpdate = fn
	g.mu.Unlock()
}

// View returns the current goal and its accounting; ok=false when the session
// has no goal yet.
func (g *GoalState) View() (GoalView, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.current == nil {
		return GoalView{}, false
	}
	return GoalView{
		Goal:     *g.current,
		Spent:    g.spent,
		Evidence: append([]string(nil), g.evidence...),
	}, true
}

// Reminder renders the bounded turn-start reminder ("" when there is nothing
// to remind: no goal, or the goal left the active state).
func (g *GoalState) Reminder() string {
	g.mu.Lock()
	cur, spent := g.current, g.spent
	g.mu.Unlock()
	if cur == nil || cur.Status != GoalActive {
		return ""
	}
	obj := cur.Objective
	if r := []rune(obj); len(r) > goalReminderObjectiveCap {
		obj = string(r[:goalReminderObjectiveCap]) + "…"
	}
	if cur.TokenBudget > 0 {
		rem := cur.TokenBudget - spent
		if rem < 0 {
			rem = 0
		}
		return fmt.Sprintf("goal reminder — objective: %s (token budget: %d of %d remaining; call the goal tool to complete when done)", obj, rem, cur.TokenBudget)
	}
	return fmt.Sprintf("goal reminder — objective: %s (call the goal tool to complete when done)", obj)
}

// Describe renders the current goal for /goal.
func (g *GoalState) Describe() string {
	v, ok := g.View()
	if !ok {
		return "goal: none — start one with /goal create <objective> (or the goal tool, op create)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "goal: %s\nobjective: %s", v.Status, v.Objective)
	if v.TokenBudget > 0 {
		fmt.Fprintf(&b, "\nbudget: %d/%d tokens used (%d remaining)", v.Spent, v.TokenBudget, v.Remaining())
	} else {
		b.WriteString("\nbudget: none")
	}
	if len(v.Evidence) == 0 {
		b.WriteString("\nevidence: none yet (completion requires at least one note)")
	} else {
		fmt.Fprintf(&b, "\nevidence: %d note(s)", len(v.Evidence))
		for _, e := range v.Evidence {
			fmt.Fprintf(&b, "\n  - %s", e)
		}
	}
	if !v.Created.IsZero() {
		fmt.Fprintf(&b, "\ncreated: %s", v.Created.Format(time.RFC3339))
	}
	return b.String()
}

// Create starts a new objective. One active goal at a time: an existing active
// goal must be completed or dropped first.
func (g *GoalState) Create(objective string, budget int64) (Goal, error) {
	obj := strings.TrimSpace(objective)
	if obj == "" {
		return Goal{}, errors.New("goal create: objective is required")
	}
	if budget < 0 {
		return Goal{}, errors.New("goal create: token_budget must be positive")
	}
	return g.mutate(func() error {
		if g.current != nil && g.current.Status == GoalActive {
			return fmt.Errorf("goal create: %q is still active — complete or drop it first", g.current.Objective)
		}
		g.current = &Goal{Objective: obj, TokenBudget: budget, Created: time.Now().UTC(), Status: GoalActive}
		g.evidence, g.spent = nil, 0
		return nil
	})
}

// Resume re-activates a dropped or budget-exhausted goal, optionally revising
// the objective. A completed goal is final — create a new one.
func (g *GoalState) Resume(objective string) (Goal, error) {
	return g.mutate(func() error {
		if g.current == nil {
			return errors.New("goal resume: no goal in this session — create one first")
		}
		switch g.current.Status {
		case GoalActive:
			return errors.New("goal resume: the goal is already active")
		case GoalCompleted:
			return errors.New("goal resume: the goal is completed — create a new one")
		}
		if obj := strings.TrimSpace(objective); obj != "" {
			g.current.Objective = obj
		}
		g.current.Status = GoalActive
		return nil
	})
}

// AddEvidence records one evidence note. Only an active goal takes evidence.
func (g *GoalState) AddEvidence(note string) (Goal, error) {
	n := strings.TrimSpace(note)
	if n == "" {
		return Goal{}, errors.New("goal evidence: note is required")
	}
	return g.mutate(func() error {
		if g.current == nil {
			return errors.New("goal evidence: no goal in this session")
		}
		if g.current.Status != GoalActive {
			return fmt.Errorf("goal evidence: goal is %s, not active", g.current.Status)
		}
		g.evidence = append(g.evidence, n)
		return nil
	})
}

// Complete closes the goal. Completion requires evidence: at least one note
// recorded here or earlier via the evidence op.
func (g *GoalState) Complete(notes []string) (Goal, error) {
	return g.mutate(func() error {
		if g.current == nil {
			return errors.New("goal complete: no goal in this session")
		}
		switch g.current.Status {
		case GoalCompleted:
			return errors.New("goal complete: the goal is already completed")
		case GoalDropped:
			return errors.New("goal complete: the goal was dropped — resume it first")
		}
		total := len(g.evidence)
		for _, n := range notes {
			if n = strings.TrimSpace(n); n != "" {
				g.evidence = append(g.evidence, n)
				total++
			}
		}
		if total == 0 {
			return errors.New(`goal complete: refusing an evidence-free completion — record what verified the objective first (op "evidence" with a note, or evidence:["..."] on complete)`)
		}
		g.current.Status = GoalCompleted
		return nil
	})
}

// Drop abandons the goal; the snapshot stays on disk.
func (g *GoalState) Drop() (Goal, error) {
	return g.mutate(func() error {
		if g.current == nil {
			return errors.New("goal drop: no goal in this session")
		}
		if g.current.Status == GoalDropped {
			return errors.New("goal drop: the goal is already dropped")
		}
		g.current.Status = GoalDropped
		return nil
	})
}

// AddUsage counts one turn's token spend against the budget and flips the goal
// to budget_exhausted when the budget is crossed (persisting and publishing
// that event). Reminders stop from then on; completion stays explicit.
// Returns true on the exhaustion flip.
//
// ponytail: spend is persisted only when a state change records a snapshot, so
// a crash mid-run can lose the turns since the last goal_updated entry; the
// ceiling is one turn's drift and the upgrade path is an entry per turn.
func (g *GoalState) AddUsage(tokens int64) bool {
	if tokens <= 0 {
		return false
	}
	g.mu.Lock()
	if g.current == nil || g.current.TokenBudget <= 0 {
		g.mu.Unlock()
		return false
	}
	g.spent += tokens
	if g.current.Status != GoalActive || g.spent < g.current.TokenBudget {
		g.mu.Unlock()
		return false
	}
	g.current.Status = GoalBudgetExhausted
	g.recordLocked()
	cur := *g.current
	cb := g.onUpdate
	g.mu.Unlock()
	if cb != nil {
		cb(cur)
	}
	return true
}

// mutate runs fn under the state lock and, when it succeeds, persists and
// publishes the resulting snapshot.
func (g *GoalState) mutate(fn func() error) (Goal, error) {
	g.mu.Lock()
	if err := fn(); err != nil {
		g.mu.Unlock()
		return Goal{}, err
	}
	g.recordLocked()
	cur := *g.current
	cb := g.onUpdate
	g.mu.Unlock()
	if cb != nil {
		cb(cur)
	}
	return cur, nil
}

// recordLocked appends the full goal snapshot to the session: the persisted
// goal_updated event. A memory-only state (no store) still works.
func (g *GoalState) recordLocked() {
	if g.current == nil || g.store == nil {
		return
	}
	if err := g.store.Append(&session.GoalUpdatedEntry{Goal: session.GoalPayload{
		Objective:   g.current.Objective,
		Status:      g.current.Status,
		TokenBudget: g.current.TokenBudget,
		Spent:       g.spent,
		Evidence:    append([]string(nil), g.evidence...),
		Created:     g.current.Created,
	}}); err != nil {
		logx.Errorf("goal: persist goal_updated: %v", err)
	}
}

// GoalToolName is the model-facing goal tool name.
const GoalToolName = "goal"

// GoalTool is the goal tool (M11 #40). It is the only writer of the session
// goal; the loop reads the same state for reminders and accounting.
type GoalTool struct{ Goals *GoalState }

func (t *GoalTool) Name() string { return GoalToolName }

func (t *GoalTool) Description() string {
	return "goal ops create/get/resume/evidence/complete/drop; needs evidence."
}

func (t *GoalTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {"type": "string", "enum": ["create", "get", "resume", "evidence", "complete", "drop"], "description": "operation"},
    "objective": {"type": "string", "description": "create: the objective (resume: optional revised objective)"},
    "token_budget": {"type": "integer", "minimum": 1, "description": "create: optional positive token budget for the whole goal"},
    "note": {"type": "string", "description": "evidence: one note recording what verified progress"},
    "evidence": {"type": "array", "items": {"type": "string"}, "description": "evidence/complete: evidence notes"}
  },
  "required": ["op"]
}`)
}

func (t *GoalTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	if t.Goals == nil {
		return tool.Result{Text: "goal: not configured", IsError: true}, nil
	}
	var a struct {
		Op          string   `json:"op"`
		Objective   string   `json:"objective"`
		TokenBudget *int64   `json:"token_budget"`
		Note        string   `json:"note"`
		Evidence    []string `json:"evidence"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "goal: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	var (
		g   Goal
		err error
	)
	switch strings.ToLower(strings.TrimSpace(a.Op)) {
	case "create":
		var budget int64
		if a.TokenBudget != nil {
			budget = *a.TokenBudget
		}
		g, err = t.Goals.Create(a.Objective, budget)
	case "get":
		v, ok := t.Goals.View()
		if !ok {
			return tool.Result{Text: "goal: none in this session — op create with an objective"}, nil
		}
		return tool.Result{Text: renderGoal(v), Details: v}, nil
	case "resume":
		g, err = t.Goals.Resume(a.Objective)
	case "evidence":
		notes := evidenceNotes(a.Note, a.Evidence)
		if len(notes) == 0 {
			return tool.Result{Text: "goal evidence: note is required", IsError: true}, nil
		}
		for _, n := range notes {
			if g, err = t.Goals.AddEvidence(n); err != nil {
				break
			}
		}
	case "complete":
		g, err = t.Goals.Complete(evidenceNotes(a.Note, a.Evidence))
	case "drop":
		g, err = t.Goals.Drop()
	default:
		return tool.Result{Text: fmt.Sprintf("goal: unknown op %q — use create|get|resume|evidence|complete|drop", a.Op), IsError: true}, nil
	}
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	return tool.Result{Text: renderGoal(GoalView{Goal: g, Spent: spentOf(t.Goals), Evidence: evidenceOf(t.Goals)}), Details: g}, nil
}

// evidenceNotes merges a single note and a list into trimmed, non-empty notes.
func evidenceNotes(note string, list []string) []string {
	var out []string
	if n := strings.TrimSpace(note); n != "" {
		out = append(out, n)
	}
	for _, n := range list {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func spentOf(g *GoalState) int64 {
	v, _ := g.View()
	return v.Spent
}

func evidenceOf(g *GoalState) []string {
	v, _ := g.View()
	return v.Evidence
}

// renderGoal renders the tool's view of a goal.
func renderGoal(v GoalView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "goal %s: %s", v.Status, v.Objective)
	if v.TokenBudget > 0 {
		fmt.Fprintf(&b, "\nbudget: %d/%d tokens used (%d remaining)", v.Spent, v.TokenBudget, v.Remaining())
	}
	if len(v.Evidence) == 0 {
		b.WriteString("\nevidence: none (completion requires at least one note)")
	} else {
		fmt.Fprintf(&b, "\nevidence: %d note(s)", len(v.Evidence))
		for _, e := range v.Evidence {
			fmt.Fprintf(&b, "\n  - %s", e)
		}
	}
	return b.String()
}

// GoalStateOf returns the live goal state wired into a registry (nil when the
// registry has no goal tool) — how the loop and cmd reach the same state.
func GoalStateOf(reg *tool.Registry) *GoalState {
	if reg == nil {
		return nil
	}
	t, ok := reg.Get(GoalToolName)
	if !ok {
		return nil
	}
	gt, ok := t.(*GoalTool)
	if !ok {
		return nil
	}
	return gt.Goals
}
