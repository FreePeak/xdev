package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/session"
)

func testScheduleClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestScheduleStatePersistsAndHydrates(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	store := session.OpenMem("/proj", "schedule test")
	state := NewScheduleState(store)
	state.now = testScheduleClock(now)

	got, err := state.Create(ScheduleInput{Prompt: "check the deploy", AfterSeconds: 60})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.ID != "schedule-1" || got.Kind != "after" {
		t.Fatalf("created = %+v", got)
	}

	reopened := NewScheduleState(nil)
	reopened.Bind(store)
	list := reopened.List()
	if len(list) != 1 || list[0].Prompt != "check the deploy" {
		t.Fatalf("hydrated list = %+v", list)
	}
}

func TestScheduleStateValidatesSelectors(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	state := NewScheduleState(session.OpenMem("/proj", "schedule test"))
	state.now = testScheduleClock(now)

	cases := []ScheduleInput{
		{Prompt: "missing"},
		{Prompt: "two", AfterSeconds: 1, At: now.Add(time.Minute)},
		{Prompt: "past", At: now.Add(-time.Second)},
		{Prompt: "too often", EverySeconds: 299},
		{Prompt: "empty prompt", AfterSeconds: -1},
	}
	for i, in := range cases {
		if _, err := state.Create(in); err == nil {
			t.Errorf("case %d accepted invalid input: %+v", i, in)
		}
	}
}

func TestScheduleStateClaimsOneShotAndAdvancesRecurring(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	state := NewScheduleState(session.OpenMem("/proj", "schedule test"))
	state.now = testScheduleClock(now)
	once, err := state.Create(ScheduleInput{Prompt: "once", AfterSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	recurring, err := state.Create(ScheduleInput{Prompt: "repeat", EverySeconds: 300})
	if err != nil {
		t.Fatal(err)
	}

	var admitted []Schedule
	admit := func(batch []Schedule) bool {
		admitted = append(admitted, batch...)
		return true
	}
	persist := func(batch []Schedule) error { return nil }
	if err := state.DeliverDue(now.Add(2*time.Second), admit, persist, nil); err != nil {
		t.Fatal(err)
	}
	if len(admitted) != 1 || admitted[0].ID != once.ID {
		t.Fatalf("first delivery = %+v", admitted)
	}
	if err := state.DeliverDue(now.Add(301*time.Second), admit, persist, nil); err != nil {
		t.Fatal(err)
	}
	if len(admitted) != 2 || admitted[1].ID != recurring.ID {
		t.Fatalf("recurring delivery = %+v", admitted)
	}
	if len(state.List()) != 1 || state.List()[0].ID != recurring.ID {
		t.Fatalf("active after one-shot delivery: %+v", state.List())
	}
	active := state.List()[0]
	if !active.ScheduledAt.After(now.Add(301 * time.Second)) {
		t.Fatalf("recurring schedule did not advance: %s", active.ScheduledAt)
	}
	if err := state.DeliverDue(now.Add(20*time.Minute), admit, persist, nil); err != nil {
		t.Fatal(err)
	}
	active = state.List()[0]
	if !active.ScheduledAt.After(now.Add(20 * time.Minute)) {
		t.Fatalf("recurring schedule is still due: %s", active.ScheduledAt)
	}
	if err := state.Delete(once.ID); err == nil {
		t.Fatal("deleted one-shot tombstone as active")
	}
}

func TestScheduleStateDeliversEarliestDue(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	state := NewScheduleState(session.OpenMem("/proj", "ordering"))
	state.now = testScheduleClock(now)
	if _, err := state.Create(ScheduleInput{Prompt: "later", AfterSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Create(ScheduleInput{Prompt: "earlier", AfterSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	var delivered []Schedule
	if err := state.DeliverDue(now.Add(3*time.Minute), nil, func(batch []Schedule) error {
		delivered = append(delivered, batch...)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || delivered[0].Prompt != "earlier" {
		t.Fatalf("delivered = %+v", delivered)
	}
}

func TestScheduleStateDeletePersistsTombstone(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	store := session.OpenMem("/proj", "schedule test")
	state := NewScheduleState(store)
	state.now = testScheduleClock(now)
	created, err := state.Create(ScheduleInput{Prompt: "delete me", AfterSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Delete(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(state.List()) != 0 {
		t.Fatalf("active after delete: %+v", state.List())
	}
	reopened := NewScheduleState(nil)
	reopened.Bind(store)
	if len(reopened.List()) != 0 {
		t.Fatalf("tombstone did not hydrate: %+v", reopened.List())
	}
}

func TestScheduleStateRefoldActiveDropsAbandonedBranch(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	store := session.OpenMem("/proj", "branch")
	state := NewScheduleState(store)
	state.now = testScheduleClock(now)
	if _, err := state.Create(ScheduleInput{Prompt: "before", AfterSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	root := store.LeafID()
	if _, err := state.Create(ScheduleInput{Prompt: "abandoned", AfterSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := store.Branch(root); err != nil {
		t.Fatal(err)
	}
	state.RefoldActive()
	if got := state.List(); len(got) != 1 || got[0].Prompt != "before" {
		t.Fatalf("active after rewind = %+v", got)
	}
}

func TestScheduleStateDeliverPersistsBeforeDispatch(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	state := NewScheduleState(session.OpenMem("/proj", "delivery"))
	state.now = testScheduleClock(now)
	created, err := state.Create(ScheduleInput{Prompt: "retry me", AfterSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("write failed")
	afterRan := false
	if err := state.DeliverDue(now.Add(2*time.Second), nil,
		func([]Schedule) error { return want },
		func([]Schedule, error) { afterRan = true },
	); !errors.Is(err, want) {
		t.Fatalf("delivery error = %v", err)
	}
	if afterRan {
		t.Fatal("after callback ran despite failed conversation persistence")
	}
	if got := state.List(); len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("failed delivery changed active state: %+v", got)
	}
	if err := state.DeliverDue(now.Add(2*time.Second), nil,
		func([]Schedule) error { return nil },
		func([]Schedule, error) { afterRan = true },
	); err != nil || !afterRan {
		t.Fatalf("retry = %v, after=%v", err, afterRan)
	}
	if len(state.List()) != 0 {
		t.Fatalf("successful delivery left schedule active: %+v", state.List())
	}
}

func TestScheduleStateBatchesEveryDueAfterOneShot(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	state := NewScheduleState(session.OpenMem("/proj", "batch"))
	state.now = testScheduleClock(now)
	first, err := state.Create(ScheduleInput{Prompt: "first", AfterSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Create(ScheduleInput{Prompt: "repeat a", EverySeconds: 300}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Create(ScheduleInput{Prompt: "repeat b", EverySeconds: 300}); err != nil {
		t.Fatal(err)
	}
	var got []Schedule
	if err := state.DeliverDue(now.Add(2*time.Second), nil, func(batch []Schedule) error {
		got = append(got, batch...)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("one-shot batch = %+v", got)
	}
	if err := state.DeliverDue(now.Add(301*time.Second), nil, func(batch []Schedule) error {
		got = append(got, batch...)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].Kind != ScheduleKindEvery || got[2].Kind != ScheduleKindEvery {
		t.Fatalf("every batch = %+v", got)
	}
}

func TestScheduleStateLimitsActiveRecords(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	state := NewScheduleState(session.OpenMem("/proj", "limit"))
	state.now = testScheduleClock(now)
	for i := 0; i < MaxActiveSchedules; i++ {
		if _, err := state.Create(ScheduleInput{Prompt: "reminder", AfterSeconds: 60}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := state.Create(ScheduleInput{Prompt: "overflow", AfterSeconds: 60}); err == nil {
		t.Fatal("active schedule limit was not enforced")
	}
}

func TestScheduleStateBindRejectsMalformedEverySnapshot(t *testing.T) {
	store := session.OpenMem("/proj", "malformed every")
	bad := &session.ScheduleChangedEntry{
		Env:      session.Envelope{Type: session.TypeScheduleChange, ID: "bad", Timestamp: nowFixture()},
		Sequence: 1,
		Active: []session.SchedulePayload{{
			ID: "schedule-1", Kind: ScheduleKindEvery, Prompt: "repeat",
			EverySeconds: MinScheduleEverySeconds - 1, ScheduledAt: nowFixture().Add(time.Minute),
		}},
	}
	if err := store.Append(bad); err != nil {
		t.Fatal(err)
	}
	state := NewScheduleState(nil)
	state.Bind(store)
	if got := state.List(); len(got) != 0 {
		t.Fatalf("malformed snapshot hydrated records: %+v", got)
	}
}

func TestScheduleStateDeliverCallbackCanList(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	state := NewScheduleState(session.OpenMem("/proj", "reentrant"))
	state.now = testScheduleClock(now)
	created, err := state.Create(ScheduleInput{Prompt: "list me", AfterSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := state.DeliverDue(now.Add(2*time.Second), nil, func([]Schedule) error { return nil }, func([]Schedule, error) {
		called = true
		if got := state.List(); len(got) != 0 {
			t.Errorf("active after claim = %+v", got)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !called || len(state.List()) != 0 {
		t.Fatalf("callback=%v created=%s", called, created.ID)
	}
}

func nowFixture() time.Time { return time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC) }
