package agent

import (
	"fmt"
	"strings"
	"sync"

	"github.com/FreePeak/xdev/internal/tool"
)

// TodoTracker runs the stop-time reminder loop, the mid-run
// nudge counters, and the failed-todo reminder — the engine omp
// ships inside TodoTracker (src/session/todo-tracker.ts). The todo
// tool itself is deliberately dumb (pure state transform in
// internal/tool/todo.go); this file holds the loop plumbing.
//
// The tracker reads phases through the registry (the todo tool is
// always a *tool.TodoTool when present), so there is no second
// copy of the state — the model's mutations are the source of truth.
type TodoTracker struct {
	mu           sync.Mutex
	reg          *tool.Registry
	reminders    bool // mirrors todo.reminders (default true)
	remindersMax int
	// reminderCount is the attempt counter within the current cycle.
	reminderCount int
	// nudgeMutations counts mutating tool results since the last
	// successful todo call (omp MID_RUN_NUDGE_MUTATION_THRESHOLD).
	nudgeMutations int
	// nudgePerCycle caps mid-run nudges per cycle
	// (omp MID_RUN_NUDGE_MAX_PER_CYCLE, default 2).
	nudgePerCycle  int
	nudgeThisCycle int
	// awaitingProgress is set when a todo call came back isError —
	// the next turn gets the failed-todo reminder (omp agent-session.ts:2882).
	awaitingProgress bool
}

// NewTodoTracker returns an engine wired to a registry (nil-safe: nil
// registry means the tracker is inactive).
func NewTodoTracker(reg *tool.Registry) *TodoTracker {
	if reg == nil {
		return nil
	}
	return &TodoTracker{
		reg:           reg,
		reminders:     true,
		remindersMax:  3,
		nudgePerCycle: 2,
	}
}

// SetReminders mirrors todo.reminders; false disables all reminders
// and mid-run nudges (omp resets counters when reminders are off).
func (tt *TodoTracker) SetReminders(on bool) {
	if tt == nil {
		return
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.reminders = on
}

// SetRemindersMax mirrors todo.remindersMax (omp: 1/2/3/5).
func (tt *TodoTracker) SetRemindersMax(n int) {
	if tt == nil {
		return
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if n >= 1 {
		tt.remindersMax = n
	}
}

// OnTodoResult is called after every todo tool result — successful
// or failed. A successful result resets the mid-run counters and clears
// the awaiting-progress latch; a failed result sets the latch so the
// next turn injects the failed-todo reminder (omp agent-session.ts:2882).
func (tt *TodoTracker) OnTodoResult(err error) {
	if tt == nil {
		return
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if err == nil {
		tt.nudgeMutations = 0
		tt.nudgeThisCycle = 0
		tt.awaitingProgress = false
		return
	}
	tt.awaitingProgress = true
}

// OnMutatingToolResult is called after every mutating tool result
// (bash, eval, edit, write, ast_edit) for the mid-run nudge counter.
// A successful todo call resets this via OnTodoResult.
func (tt *TodoTracker) OnMutatingToolResult() {
	if tt == nil {
		return
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if !tt.reminders || tt.awaitingProgress {
		return
	}
	tt.nudgeMutations++
}

// ReminderReady reports whether the stop-time reminder should fire now:
// reminders on, no pending progress (a failed todo is being retried),
// the mutation threshold (12) crossed, the per-cycle nudge cap (2) not
// exhausted, and the attempt counter below remindersMax.
func (tt *TodoTracker) ReminderReady() bool {
	if tt == nil {
		return false
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.reminders &&
		!tt.awaitingProgress &&
		tt.nudgeMutations >= 12 &&
		tt.nudgeThisCycle < tt.nudgePerCycle &&
		tt.reminderCount < tt.remindersMax
}

// AcknowledgeReminder is called when a stop-time reminder has been
// emitted (or suppressed for a guard reason): it increments both the
// reminder attempt counter and the per-cycle nudge counter, and clears
// the awaiting-progress latch so the next cycle can fire again.
func (tt *TodoTracker) AcknowledgeReminder() {
	if tt == nil {
		return
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.reminderCount++
	tt.nudgeThisCycle++
	tt.awaitingProgress = false
}

// AcknowledgeNudge is called when a mid-run nudge has been emitted:
// it increments the per-cycle counter but not the reminder attempt
// counter (omp takes separate paths for the two).
func (tt *TodoTracker) AcknowledgeNudge() {
	if tt == nil {
		return
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.nudgeThisCycle++
	tt.awaitingProgress = false
}

// Reset clears all counters (omp resets when phases are empty or no
// pending/in_progress tasks remain, or when reminders are toggled).
func (tt *TodoTracker) Reset() {
	if tt == nil {
		return
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.reminderCount = 0
	tt.nudgeMutations = 0
	tt.nudgeThisCycle = 0
	tt.awaitingProgress = false
}

// OpenPhases returns pending and in_progress tasks across all phases —
// the set a stop-time reminder reports (omp excludes blocked tasks;
// they are awaiting external input).
func (tt *TodoTracker) OpenPhases() []tool.TodoPhase {
	if tt == nil || tt.reg == nil {
		return nil
	}
	t, ok := tt.reg.Get("todo")
	if !ok {
		return nil
	}
	ttt, ok := t.(*tool.TodoTool)
	if !ok {
		return nil
	}
	return tool.OpenForReminder(ttt.Snapshot())
}

// FormatReminder renders the stop-time reminder body (omp's
// developer-role <system-reminder>). Open items are grouped under
// their phase names; a phase without open tasks is omitted.
// attempt is the N in the "(Reminder N/max)" footer.
func (tt *TodoTracker) FormatReminder(attempt int) string {
	phases := tt.OpenPhases()
	if len(phases) == 0 {
		return ""
	}
	var b strings.Builder
	total := 0
	for _, p := range phases {
		total += len(p.Tasks)
	}
	fmt.Fprintf(&b, "You stopped with %d incomplete todo item(s):\n", total)
	for _, p := range phases {
		if len(p.Tasks) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s\n", p.Name)
		for _, task := range p.Tasks {
			fmt.Fprintf(&b, "  - %s\n", task.Content)
		}
	}
	fmt.Fprintf(&b, "\nPlease continue working on these tasks or mark them complete if finished.\n(Reminder %d/%d)", attempt, tt.remindersMax)
	return b.String()
}

// Attempt returns the current reminder attempt counter (the N in the
// "(Reminder N/max)" footer) — exposed so the loop can render the
// footer without re-reading the counter itself.
func (tt *TodoTracker) Attempt() int {
	if tt == nil {
		return 0
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.reminderCount
}

// TodoStateOf returns the live todo tracker wired into a registry
// (nil when the registry holds no todo tool) — how the loop and cmd
// reach the same state (mirrors GoalStateOf).
func TodoStateOf(reg *tool.Registry) *TodoTracker {
	if reg == nil {
		return nil
	}
	t, ok := reg.Get("todo")
	if !ok {
		return nil
	}
	_, ok = t.(*tool.TodoTool)
	if !ok {
		return nil
	}
	return NewTodoTracker(reg)
}
