package agent

import (
	"errors"
	"testing"
)

func TestTodoTrackerNilRegistry(t *testing.T) {
	if NewTodoTracker(nil) != nil {
		t.Fatal("nil registry should yield nil tracker")
	}
	if TodoStateOf(nil) != nil {
		t.Fatal("TodoStateOf(nil) should return nil")
	}
}

func TestTodoTrackerRemindersDefaultOn(t *testing.T) {
	tt := &TodoTracker{reminders: true, remindersMax: 3, nudgePerCycle: 2}
	if tt.ReminderReady() {
		t.Fatal("tracker with no mutations should not be reminder-ready")
	}
}

func TestTodoTrackerRemindersOff(t *testing.T) {
	tt := &TodoTracker{reminders: false, remindersMax: 3, nudgePerCycle: 2}
	tt.nudgeMutations = 100
	if tt.ReminderReady() {
		t.Fatal("reminders off should never fire")
	}
	tt.AcknowledgeReminder()
	tt.AcknowledgeNudge()
}

func TestTodoTrackerRemindersMaxCap(t *testing.T) {
	tt := &TodoTracker{reminders: true, remindersMax: 2, nudgePerCycle: 2}
	for i := 0; i < 5; i++ {
		tt.AcknowledgeReminder()
	}
	// remindersMax caps ReminderReady, not the counter itself —
	// AcknowledgeReminder always increments the attempt counter.
	if tt.Attempt() != 5 {
		t.Fatalf("attempt counter = %d, want 5", tt.Attempt())
	}
	if tt.ReminderReady() {
		t.Fatal("reminder should be capped by remindersMax")
	}
}

func TestTodoTrackerNudgePerCycle(t *testing.T) {
	tt := &TodoTracker{reminders: true, remindersMax: 10, nudgePerCycle: 2}
	tt.nudgeMutations = 100
	if !tt.ReminderReady() {
		t.Fatal("first reminder should be ready")
	}
	tt.AcknowledgeReminder()
	if !tt.ReminderReady() {
		t.Fatal("second reminder should still be ready within the cycle")
	}
	tt.AcknowledgeReminder()
	if tt.ReminderReady() {
		t.Fatal("third reminder should be gated by per-cycle cap")
	}
}

func TestTodoTrackerCountersReset(t *testing.T) {
	tt := &TodoTracker{reminders: true, remindersMax: 3, nudgePerCycle: 2}
	tt.nudgeMutations = 100
	tt.reminderCount = 2
	tt.nudgeThisCycle = 2
	tt.Reset()
	if tt.nudgeMutations != 0 || tt.reminderCount != 0 || tt.nudgeThisCycle != 0 {
		t.Fatal("Reset did not clear counters")
	}
}

func TestTodoTrackerFailedTodoSetsLatch(t *testing.T) {
	tt := &TodoTracker{reminders: true, remindersMax: 3, nudgePerCycle: 2}
	tt.nudgeMutations = 100
	if !tt.ReminderReady() {
		t.Fatal("should be ready before a failure")
	}
	tt.OnTodoResult(errors.New("boom"))
	if tt.ReminderReady() {
		t.Fatal("failed todo should gate the reminder until the next cycle")
	}
	// A successful todo resets the counters, so the mutation
	// threshold is no longer crossed — the latch is cleared
	// (the next cycle can fire again after fresh mutations).
	tt.OnTodoResult(nil)
	if tt.ReminderReady() {
		t.Fatal("successful todo should reset the mutation counter")
	}
	tt.nudgeMutations = 100
	if !tt.ReminderReady() {
		t.Fatal("fresh mutations after a successful todo should restore readiness")
	}
}

func TestTodoTrackerMutatingTool(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"bash", true},
		{"eval", true},
		{"edit", true},
		{"write", true},
		{"ast_edit", true},
		{"todo", false},
		{"glob", false},
		{"grep", false},
	}
	for _, tc := range tests {
		got := isTodoMutatingTool(tc.name)
		if got != tc.want {
			t.Errorf("isTodoMutatingTool(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTodoTrackerFormatReminderEmpty(t *testing.T) {
	tt := &TodoTracker{remindersMax: 3}
	if got := tt.FormatReminder(1); got != "" {
		t.Fatalf("FormatReminder on empty phases = %q, want \"", got)
	}
}

func TestTodoTrackerAttempt(t *testing.T) {
	tt := &TodoTracker{remindersMax: 3}
	if tt.Attempt() != 0 {
		t.Fatalf("initial Attempt() = %d, want 0", tt.Attempt())
	}
	tt.AcknowledgeReminder()
	if tt.Attempt() != 1 {
		t.Fatalf("after one AcknowledgeReminder, Attempt() = %d, want 1", tt.Attempt())
	}
}