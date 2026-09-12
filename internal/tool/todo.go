package tool

// TodoTool: the omp 9-op todo tool (docs/research/omp-todos-internals.md).
// State is a phased list the model mutates with content-addressed ops; after
// every state-changing op the earliest pending task auto-promotes to
// in_progress unless it is blocked. Each successful mutation is handed to
// the optional TodoSink so canonical state rides the session transcript.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// TodoStatus is one task state.
type TodoStatus string

// The five task statuses.
const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
	TodoBlocked    TodoStatus = "blocked"
	TodoDropped    TodoStatus = "dropped"

	// defaultInitPhase names the synthesized phase of a flat init.
	defaultInitPhase = "Tasks"
)

// TodoItem is one task. Content is the lookup key — never an ID.
type TodoItem struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
	Blocker string     `json:"blocker,omitempty"`
}

// TodoPhase groups tasks under a short noun-phrase name.
type TodoPhase struct {
	Name  string     `json:"name"`
	Tasks []TodoItem `json:"tasks"`
}

// TodoSink persists a snapshot after every state-changing op (the cmd
// adapter appends a `user_todo_edit` session entry).
type TodoSink interface {
	AppendTodo(phases []TodoPhase) error
}

var _ Tool = (*TodoTool)(nil)

// TodoTool holds the live phased list for one session. A nil Sink keeps the
// list in memory only (used when no session store is wired).
type TodoTool struct {
	mu     sync.Mutex
	phases []TodoPhase
	Sink   TodoSink
}

// NewTodoTool returns an empty TodoTool.
func NewTodoTool() *TodoTool { return &TodoTool{} }

// Name implements Tool.
func (t *TodoTool) Name() string { return "todo" }

// Description implements Tool.
func (t *TodoTool) Description() string {
	return "Phased task list. init(list:[{phase,items}]) replaces; start/done/drop/block/unblock/rm(task|phase|none); append(phase,items); view. Earliest pending auto-promotes; blocked never. Reference tasks by exact text, never IDs."
}

// Parameters implements Tool.
func (t *TodoTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "required": ["op"],
  "properties": {
    "op": {
      "type": "string",
      "enum": ["init", "start", "done", "drop", "block", "unblock", "append", "view", "rm"],
      "description": "Op"
    },
    "list": {
      "type": "array",
      "description": "init: phases; replaces the list",
      "items": {
        "type": "object",
        "required": ["phase", "items"],
        "properties": {
          "phase": {"type": "string", "description": "Phase name"},
          "items": {"type": "array", "minItems": 1, "items": {"type": "string"}, "description": "Tasks"}
        }
      }
    },
    "task": {"type": "string", "description": "Exact task text"},
    "phase": {"type": "string", "description": "Exact phase name"},
    "items": {"type": "array", "items": {"type": "string"}, "description": "Tasks (flat init / append)"},
    "reason": {"type": "string", "description": "Blocker note"}
  }
}`)
}

// todoArgs is the single-op argument shape. A missing op is inferred when
// the payload is unambiguous (omp's lenientArgValidation repair).
type todoArgs struct {
	Op     string         `json:"op"`
	List   []todoPhaseArg `json:"list,omitempty"`
	Task   string         `json:"task,omitempty"`
	Phase  string         `json:"phase,omitempty"`
	Items  []string       `json:"items,omitempty"`
	Reason string         `json:"reason,omitempty"`
}

type todoPhaseArg struct {
	Phase string   `json:"phase"`
	Items []string `json:"items"`
}

var inventedTaskID = regexp.MustCompile(`^task-\d+$`)

// Execute implements Tool.
func (t *TodoTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Text: fmt.Sprintf("todo: canceled: %v", err)}, nil
	}
	var a todoArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("todo: invalid arguments: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	op := strings.ToLower(strings.TrimSpace(a.Op))
	if op == "" {
		op = inferTodoOp(a, len(t.phases) > 0)
		if op == "" {
			return todoError("Invalid todo arguments: provide op (init|start|done|drop|block|unblock|append|view|rm); list implies init, items+phase implies append"), nil
		}
	}
	if op == "view" {
		return t.result(), nil
	}

	var err error
	switch op {
	case "init":
		t.phases, err = initPhases(a)
	case "start":
		err = todoStart(t.phases, a.Task)
	case "done":
		err = todoSetStatus(t.phases, a, TodoCompleted)
	case "drop":
		err = todoSetStatus(t.phases, a, TodoDropped)
	case "block":
		err = todoBlock(t.phases, a)
	case "unblock":
		err = todoUnblock(t.phases, a)
	case "append":
		err = todoAppend(&t.phases, a)
	case "rm":
		err = todoRemove(&t.phases, a)
	default:
		err = fmt.Errorf("unknown op %q (want init, start, done, drop, block, unblock, append, view, or rm)", a.Op)
	}
	if err != nil {
		// Discard-on-error: the list stays exactly where it was.
		return todoError(err.Error()), nil
	}

	// Auto-promotion engine: at most one in_progress task, earliest pending
	// takes over when none is active (blocked tasks are invisible to it).
	promoteInProgress(t.phases)

	res := t.result()
	if t.Sink != nil {
		if perr := t.Sink.AppendTodo(clonePhases(t.phases)); perr != nil {
			res.IsError = true
			res.Text = "todo: state updated in memory but session persistence failed: " + perr.Error() + "\n" + res.Text
		}
	}
	return res, nil
}

func todoError(msg string) Result {
	return Result{IsError: true, Text: "todo: " + msg}
}

// inferTodoOp repairs a missing op from an unambiguous payload (omp parity).
func inferTodoOp(a todoArgs, hasPhases bool) string {
	switch {
	case len(a.List) > 0:
		return "init"
	case len(a.Items) > 0 && a.Phase != "":
		return "append"
	case len(a.Items) > 0 && !hasPhases:
		return "init"
	default:
		return ""
	}
}

// initPhases builds a fresh list from the payload; every task starts
// pending (promotion runs after the op). Duplicate phase or task names are
// rejected because targeting resolves the first match.
func initPhases(a todoArgs) ([]TodoPhase, error) {
	if len(a.List) == 0 {
		if len(a.Items) == 0 {
			return nil, errors.New("Missing list for init operation")
		}
		name := strings.TrimSpace(a.Phase)
		if name == "" {
			name = defaultInitPhase
		}
		tasks, err := newTodoTasks(a.Items, nil)
		if err != nil {
			return nil, err
		}
		return []TodoPhase{{Name: name, Tasks: tasks}}, nil
	}

	seenPhase := map[string]bool{}
	seenTask := map[string]bool{}
	out := make([]TodoPhase, 0, len(a.List))
	for _, p := range a.List {
		name := strings.TrimSpace(p.Phase)
		if name == "" {
			return nil, errors.New("init list entries need a phase name")
		}
		if seenPhase[name] {
			return nil, fmt.Errorf("Duplicate phase %q in init list", name)
		}
		seenPhase[name] = true
		tasks, err := newTodoTasks(p.Items, seenTask)
		if err != nil {
			return nil, err
		}
		if len(tasks) == 0 {
			return nil, fmt.Errorf("Phase %q needs at least one item", name)
		}
		out = append(out, TodoPhase{Name: name, Tasks: tasks})
	}
	return out, nil
}

// newTodoTasks trims and de-duplicates task contents (shared across phases:
// every targeting op resolves the first match by content).
func newTodoTasks(items []string, seen map[string]bool) ([]TodoItem, error) {
	out := make([]TodoItem, 0, len(items))
	for _, raw := range items {
		content := strings.TrimSpace(raw)
		if content == "" {
			continue
		}
		if seen != nil {
			if seen[content] {
				return nil, fmt.Errorf("Duplicate task %q in init list", content)
			}
			seen[content] = true
		}
		out = append(out, TodoItem{Content: content, Status: TodoPending})
	}
	return out, nil
}

// todoStart marks one task in progress, demoting every other active task.
func todoStart(phases []TodoPhase, task string) error {
	if task == "" {
		return errors.New("Missing task content")
	}
	item := findTask(phases, task)
	if item == nil {
		return taskNotFound(phases, task)
	}
	for i := range phases {
		for j := range phases[i].Tasks {
			if &phases[i].Tasks[j] != item && phases[i].Tasks[j].Status == TodoInProgress {
				phases[i].Tasks[j].Status = TodoPending
			}
		}
	}
	item.Status = TodoInProgress
	return nil
}

// todoSetStatus applies done/drop to the resolved target (task, phase, or
// the whole list).
func todoSetStatus(phases []TodoPhase, a todoArgs, status TodoStatus) error {
	targets, err := resolveTodoTargets(phases, a.Task, a.Phase)
	if err != nil {
		return err
	}
	for _, it := range targets {
		it.Status = status
		it.Blocker = ""
	}
	return nil
}

// todoBlock marks the target blocked, keeping finished work finished: only
// pending, in_progress, and already-blocked tasks flip.
func todoBlock(phases []TodoPhase, a todoArgs) error {
	if a.Task == "" && a.Phase == "" {
		return errors.New("block requires a task or phase target")
	}
	targets, err := resolveTodoTargets(phases, a.Task, a.Phase)
	if err != nil {
		return err
	}
	note := collapseSpace(a.Reason)
	for _, it := range targets {
		switch it.Status {
		case TodoPending, TodoInProgress, TodoBlocked:
			it.Status = TodoBlocked
			it.Blocker = note
		}
	}
	return nil
}

// todoUnblock returns blocked targets to pending and clears the note;
// unblocking a non-blocked task is a silent no-op.
func todoUnblock(phases []TodoPhase, a todoArgs) error {
	if a.Task == "" && a.Phase == "" {
		return errors.New("unblock requires a task or phase target")
	}
	targets, err := resolveTodoTargets(phases, a.Task, a.Phase)
	if err != nil {
		return err
	}
	for _, it := range targets {
		if it.Status == TodoBlocked {
			it.Status = TodoPending
			it.Blocker = ""
		}
	}
	return nil
}

// todoAppend adds tasks to a phase, lazily creating it.
func todoAppend(phases *[]TodoPhase, a todoArgs) error {
	if strings.TrimSpace(a.Phase) == "" {
		return errors.New("append requires a phase name")
	}
	if len(a.Items) == 0 {
		return errors.New("Missing items for append operation")
	}
	name := strings.TrimSpace(a.Phase)
	seen := map[string]bool{}
	for i := range *phases {
		for j := range (*phases)[i].Tasks {
			seen[(*phases)[i].Tasks[j].Content] = true
		}
	}
	tasks, err := newTodoTasks(a.Items, seen)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return errors.New("Missing items for append operation")
	}
	for i := range *phases {
		if (*phases)[i].Name == name {
			(*phases)[i].Tasks = append((*phases)[i].Tasks, tasks...)
			return nil
		}
	}
	*phases = append(*phases, TodoPhase{Name: name, Tasks: tasks})
	return nil
}

// todoRemove deletes one task, empties a phase's tasks, or clears the list
// (phases themselves survive a clear).
func todoRemove(phases *[]TodoPhase, a todoArgs) error {
	switch {
	case a.Task != "":
		for i := range *phases {
			tasks := (*phases)[i].Tasks
			for j := range tasks {
				if tasks[j].Content == a.Task {
					(*phases)[i].Tasks = append(tasks[:j], tasks[j+1:]...)
					return nil
				}
			}
		}
		return taskNotFound(*phases, a.Task)
	case a.Phase != "":
		p := findPhase(*phases, a.Phase)
		if p == nil {
			return fmt.Errorf("Phase %q not found", a.Phase)
		}
		p.Tasks = nil
		return nil
	default:
		for i := range *phases {
			(*phases)[i].Tasks = nil
		}
		return nil
	}
}

// Snapshot returns a copy of the current list (session replay, external
// renderers, and the loop engine's stop-time checks read state, never the
// live slice).
func (t *TodoTool) Snapshot() []TodoPhase {
	t.mu.Lock()
	defer t.mu.Unlock()
	return clonePhases(t.phases)
}

// OpenForReminder returns the tasks a stop-time reminder should report:
// pending and in_progress only. Blocked tasks are awaiting external input
// and finished tasks are done, so neither may nag the user (omp parity).
func OpenForReminder(phases []TodoPhase) []TodoPhase {
	var out []TodoPhase
	for _, p := range phases {
		var open []TodoItem
		for _, task := range p.Tasks {
			if task.Status == TodoPending || task.Status == TodoInProgress {
				open = append(open, task)
			}
		}
		if len(open) > 0 {
			out = append(out, TodoPhase{Name: p.Name, Tasks: open})
		}
	}
	return out
}

// promoteInProgress is the auto-promotion engine: if several tasks are in
// progress only the earliest stays; if none is, the earliest pending task
// (phase order, then task order) takes over. Blocked and finished tasks are
// never promoted.
func promoteInProgress(phases []TodoPhase) {
	var active []*TodoItem
	for i := range phases {
		for j := range phases[i].Tasks {
			if phases[i].Tasks[j].Status == TodoInProgress {
				active = append(active, &phases[i].Tasks[j])
			}
		}
	}
	if len(active) > 1 {
		for _, it := range active[1:] {
			it.Status = TodoPending
		}
	}
	if len(active) > 0 {
		return
	}
	for i := range phases {
		for j := range phases[i].Tasks {
			if phases[i].Tasks[j].Status == TodoPending {
				phases[i].Tasks[j].Status = TodoInProgress
				return
			}
		}
	}
}

// resolveTodoTargets maps task/phase/none onto the matched tasks (none =
// every task in the list, so a bare done completes the whole list).
func resolveTodoTargets(phases []TodoPhase, task, phase string) ([]*TodoItem, error) {
	if task != "" {
		item := findTask(phases, task)
		if item == nil {
			return nil, taskNotFound(phases, task)
		}
		return []*TodoItem{item}, nil
	}
	if phase != "" {
		p := findPhase(phases, phase)
		if p == nil {
			return nil, fmt.Errorf("Phase %q not found", phase)
		}
		return phaseItems(p), nil
	}
	var all []*TodoItem
	for i := range phases {
		all = append(all, phaseItems(&phases[i])...)
	}
	return all, nil
}

func findTask(phases []TodoPhase, content string) *TodoItem {
	for i := range phases {
		for j := range phases[i].Tasks {
			if phases[i].Tasks[j].Content == content {
				return &phases[i].Tasks[j]
			}
		}
	}
	return nil
}

func findPhase(phases []TodoPhase, name string) *TodoPhase {
	for i := range phases {
		if phases[i].Name == name {
			return &phases[i]
		}
	}
	return nil
}

func phaseItems(p *TodoPhase) []*TodoItem {
	out := make([]*TodoItem, 0, len(p.Tasks))
	for i := range p.Tasks {
		out = append(out, &p.Tasks[i])
	}
	return out
}

// taskNotFound explains a failed content lookup, including the two common
// recovery hints (invented IDs, an empty list).
func taskNotFound(phases []TodoPhase, task string) error {
	msg := fmt.Sprintf("Task %q not found", task)
	if inventedTaskID.MatchString(task) {
		msg = "Tasks are referenced by content, not by IDs — pass the task's full text from the previous result"
	}
	if len(phases) == 0 {
		msg += " (todo list is empty — was it replaced or not yet created?)"
	}
	return errors.New(msg)
}

// collapseSpace folds whitespace runs (incl. newlines) into single spaces so
// a blocker note survives a one-line render.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func clonePhases(phases []TodoPhase) []TodoPhase {
	out := make([]TodoPhase, len(phases))
	for i := range phases {
		out[i] = TodoPhase{Name: phases[i].Name, Tasks: append([]TodoItem(nil), phases[i].Tasks...)}
	}
	return out
}

// result renders the list the model sees, with the canonical state in
// Details (details.phases is the transcript-replay record, omp parity).
func (t *TodoTool) result() Result {
	return Result{
		Text:    renderTodoPhases(t.phases),
		Details: map[string]any{"phases": clonePhases(t.phases)},
	}
}

// renderTodoPhases renders the omp-style checklist:
//
//	I. Foundation
//	  [/] wire the registry
//	  [ ] write tests
func renderTodoPhases(phases []TodoPhase) string {
	var b strings.Builder
	counted := 0
	total, done, open, blocked := 0, 0, 0, 0
	for i := range phases {
		p := &phases[i]
		if len(p.Tasks) == 0 {
			continue
		}
		counted++
		fmt.Fprintf(&b, "%s. %s\n", romanNumeral(counted), p.Name)
		for j := range p.Tasks {
			it := &p.Tasks[j]
			total++
			switch it.Status {
			case TodoCompleted, TodoDropped:
				done++
			case TodoBlocked:
				open++
				blocked++
			default:
				open++
			}
			fmt.Fprintf(&b, "  %s %s", todoMarker(it.Status), it.Content)
			if it.Status == TodoBlocked && it.Blocker != "" {
				fmt.Fprintf(&b, " (blocked: %s)", it.Blocker)
			}
			b.WriteByte('\n')
		}
	}
	if total == 0 {
		return "(todo list is empty)"
	}
	summary := fmt.Sprintf("%d/%d completed, %d open", done, total, open)
	if blocked > 0 {
		summary += fmt.Sprintf(", %d blocked", blocked)
	}
	return strings.TrimRight(b.String(), "\n") + "\n(" + summary + ")"
}

func todoMarker(s TodoStatus) string {
	switch s {
	case TodoInProgress:
		return "[/]"
	case TodoCompleted:
		return "[x]"
	case TodoBlocked:
		return "[!]"
	case TodoDropped:
		return "[-]"
	default:
		return "[ ]"
	}
}

var romanNumerals = []string{
	"I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X",
	"XI", "XII", "XIII", "XIV", "XV", "XVI", "XVII", "XVIII", "XIX", "XX",
}

func romanNumeral(n int) string {
	if n >= 1 && n <= len(romanNumerals) {
		return romanNumerals[n-1]
	}
	return strconv.Itoa(n)
}
