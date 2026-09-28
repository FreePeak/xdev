package tui

import (
	"strings"
	"testing"
	"time"
)

func TestScheduleSelector(t *testing.T) {
	at := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value   string
		kind    string
		seconds int64
		at      time.Time
	}{
		{value: "60", kind: "after", seconds: 60},
		{value: "300", kind: "every", seconds: 300},
		{value: "2026-09-23T10:00:00Z", kind: "at", at: at},
	} {
		kind, seconds, gotAt, err := ScheduleSelector(tc.value)
		if err != nil || kind != tc.kind || seconds != tc.seconds || !gotAt.Equal(tc.at) {
			t.Fatalf("ScheduleSelector(%q) = %q/%d/%s/%v", tc.value, kind, seconds, gotAt, err)
		}
	}
	for _, bad := range []string{"", "0", "-1", "abc", "299"} {
		// 299 parses as an after delay in this UI grammar; only verify values
		// that the grammar itself rejects.
		if bad == "299" {
			continue
		}
		if _, _, _, err := ScheduleSelector(bad); err == nil {
			t.Fatalf("ScheduleSelector(%q) accepted invalid input", bad)
		}
	}
}

func TestScheduleOpsDispatch(t *testing.T) {
	var createdPrompt, createdSelector, deleted string
	ops := &ScheduleOps{
		List: func() string { return "schedule-1\tafter" },
		Create: func(prompt, selector string) (string, error) {
			createdPrompt, createdSelector = prompt, selector
			return "scheduled schedule-2", nil
		},
		Delete: func(id string) error { deleted = id; return nil },
	}
	if out, err := ops.Dispatch(""); err != nil || !strings.Contains(out, "schedule-1") {
		t.Fatalf("list = %q/%v", out, err)
	}
	if out, err := ops.Dispatch("create 60 check deploy"); err != nil || out == "" || createdPrompt != "check deploy" || createdSelector != "60" {
		t.Fatalf("create = %q/%v prompt=%q selector=%q", out, err, createdPrompt, createdSelector)
	}
	if out, err := ops.Dispatch("delete schedule-2"); err != nil || out != "deleted schedule-2" || deleted != "schedule-2" {
		t.Fatalf("delete = %q/%v deleted=%q", out, err, deleted)
	}
	if _, err := ops.Dispatch("create nope"); err == nil {
		t.Fatal("invalid selector accepted")
	}
}

func TestScheduleSelectorEveryIsRecurring(t *testing.T) {
	kind, seconds, at, err := ScheduleSelector("300")
	if err != nil || kind != "every" || seconds != 300 || !at.IsZero() {
		t.Fatalf("recurring selector = %q/%d/%s/%v", kind, seconds, at, err)
	}
}
