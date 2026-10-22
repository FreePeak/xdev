package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// AgentRecords projects the hub's live state into omp's vocabulary: a live
// agent is running, a settled one is idle with an outcome, a parked one is
// parked, and a canceled one is aborted. xdev's own roster words — done and
// failed — are outcomes here, not statuses.
//
// Every row asserts on the records the hub really returns, so a reverted
// projection fails the test rather than passing it on a fixture.
func TestHubAgentRecordsStatus(t *testing.T) {
	cases := []struct {
		name        string
		build       func(t *testing.T) (*Hub, string)
		wantStatus  AgentStatus
		wantOutcome AgentOutcome
	}{
		{
			name: "a live agent is running",
			build: func(t *testing.T) (*Hub, string) {
				blocker := &blockingTool{release: make(chan struct{})}
				t.Cleanup(blocker.unblock)
				p := &fakeProvider{calls: []fakeScript{
					{events: toolCallEvents("block", `{}`)},
					{events: yieldEvents(`{"result":"live"}`)},
				}}
				h := NewHub()
				id, err := h.Start(context.Background(), SubagentSpec{
					Name: "live", Prompt: "work", Provider: p, Model: "m-live",
					Tools: []tool.Tool{blocker}, MaxTurns: 3,
				})
				if err != nil {
					t.Fatal(err)
				}
				waitChildStarted(t, h, id)
				return h, id
			},
			wantStatus: AgentRunning,
		},
		{
			name: "a settled agent is idle with outcome done",
			build: func(t *testing.T) (*Hub, string) {
				p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"done"}`)}}}
				h := NewHub()
				id, err := h.Start(context.Background(), SubagentSpec{
					Name: "done", Prompt: "work", Provider: p, Model: "m",
					Tools: []tool.Tool{}, MaxTurns: 2,
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(h.Wait(context.Background(), []string{id}, 10*time.Second)) == 0 {
					t.Fatal("job did not settle")
				}
				return h, id
			},
			wantStatus: AgentIdle, wantOutcome: AgentOutcomeDone,
		},
		{
			name: "a failed agent is idle with outcome failed",
			build: func(t *testing.T) (*Hub, string) {
				pf := &fakeProvider{calls: []fakeScript{{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}}}}
				h := NewHub()
				id, err := h.Start(context.Background(), SubagentSpec{
					Name: "doomed", Prompt: "work", Provider: pf, Model: "m",
					Tools: []tool.Tool{}, MaxTurns: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(h.Wait(context.Background(), []string{id}, 10*time.Second)) == 0 {
					t.Fatal("job did not settle")
				}
				return h, id
			},
			wantStatus: AgentIdle, wantOutcome: AgentOutcomeFailed,
		},
		{
			name: "a parked job is parked",
			build: func(t *testing.T) (*Hub, string) {
				p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"parked"}`)}}}
				h := NewHub()
				id, err := h.Start(context.Background(), SubagentSpec{
					Name: "parked", Prompt: "work", Provider: p, Model: "m",
					Tools: []tool.Tool{}, MaxTurns: 2,
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(h.Wait(context.Background(), []string{id}, 10*time.Second)) == 0 {
					t.Fatal("job did not settle")
				}
				if !h.Park(id) {
					t.Fatal("park refused a settled job")
				}
				return h, id
			},
			// A parked job settled before it was parked, so it still
			// carries the outcome of that run.
			wantStatus: AgentParked, wantOutcome: AgentOutcomeDone,
		},
		{
			name: "a canceled agent is aborted",
			build: func(t *testing.T) (*Hub, string) {
				// The provider stalls before its first event, so the job is
				// still live when the cancel lands.
				p := &fakeProvider{calls: []fakeScript{{delay: 500 * time.Millisecond, events: yieldEvents(`{"result":"late"}`)}}}
				h := NewHub()
				id, err := h.Start(context.Background(), SubagentSpec{
					Name: "killed", Prompt: "work", Provider: p, Model: "m",
					Tools: []tool.Tool{}, MaxTurns: 2,
				})
				if err != nil {
					t.Fatal(err)
				}
				if !h.Cancel(id) {
					t.Fatal("cancel refused a live job")
				}
				return h, id
			},
			wantStatus: AgentAborted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, id := tc.build(t)
			rec := recordByID(t, h, id)
			if rec.Status != tc.wantStatus {
				t.Fatalf("record status = %q, want %q (record %+v)", rec.Status, tc.wantStatus, rec)
			}
			if rec.Outcome != tc.wantOutcome {
				t.Fatalf("record outcome = %q, want %q (record %+v)", rec.Outcome, tc.wantOutcome, rec)
			}
		})
	}
}

// A record carries the agent's identity and kind: the id the hub addresses
// it by, the child's label, and whether it is a background job or a
// foreground task child.
func TestHubAgentRecordIdentity(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"ident"}`)}}}
	h := NewHub()
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "ident", Prompt: "work", Provider: p, Model: "m-ident",
		Tools: []tool.Tool{}, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	rec := recordByID(t, h, id)
	if rec.ID != id {
		t.Fatalf("record id = %q, want %q", rec.ID, id)
	}
	if rec.Name != "ident" {
		t.Fatalf("record name = %q, want %q", rec.Name, "ident")
	}
	if rec.Model != "m-ident" {
		t.Fatalf("record model = %q, want %q", rec.Model, "m-ident")
	}
	if rec.Kind != AgentKindJob {
		t.Fatalf("record kind = %q, want %q", rec.Kind, AgentKindJob)
	}
	if rec.Activity == "" {
		t.Fatal("record activity is empty")
	}
}

// A foreground task child is its own kind, and it is settled by the tool
// call that spawned it rather than by a job id.
func TestHubAgentRecordForegroundChild(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"fg"}`)}}}
	h := NewHub()
	fgID, capture := h.TrackForeground(SubagentSpec{Name: "fg-child", Model: "m-fg"})
	res, err := SpawnChild(context.Background(), SubagentSpec{
		Name: "fg-child", Prompt: "work", Provider: p, Model: "m-fg",
		Tools: []tool.Tool{}, MaxTurns: 2, OnRun: capture,
	})
	h.SettleForeground(fgID, res, err)
	rec := recordByID(t, h, fgID)
	if rec.Kind != AgentKindChild {
		t.Fatalf("record kind = %q, want %q", rec.Kind, AgentKindChild)
	}
	if rec.Name != "fg-child" {
		t.Fatalf("record name = %q, want %q", rec.Name, "fg-child")
	}
	if rec.Status != AgentIdle || rec.Outcome != AgentOutcomeDone {
		t.Fatalf("settled foreground record = %+v", rec)
	}
}

// A child's parent is recorded as a LABEL (SubagentSpec.Parent), and the
// record resolves that label to the parent's record id inside the same hub.
func TestHubAgentRecordCarriesParentID(t *testing.T) {
	h := NewHub()
	parentID, _ := h.TrackForeground(SubagentSpec{Name: "scout", Model: "m-scout"})

	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"leaf"}`)}}}
	childID, err := h.Start(context.Background(), SubagentSpec{
		Name: "leaf", Prompt: "work", Provider: p, Model: "m-leaf",
		Tools: []tool.Tool{}, MaxTurns: 2, Parent: "scout",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Wait(context.Background(), []string{childID}, 10*time.Second)

	child := recordByID(t, h, childID)
	if child.ParentID != parentID {
		t.Fatalf("child parent id = %q, want %q", child.ParentID, parentID)
	}
	// A child with no parent carries none.
	parent := recordByID(t, h, parentID)
	if parent.ParentID != "" {
		t.Fatalf("top-level agent parent id = %q, want empty", parent.ParentID)
	}
}

// A child that produced output has tokens to report: the record's metrics
// come from the same walk the roster uses, so they cannot disagree with it.
func TestHubAgentRecordMetrics(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEventsWithUsage(`{"result":"measured"}`, 321)}}}
	h := NewHub()
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "measured", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	rec := recordByID(t, h, id)
	if rec.Metrics.Tokens <= 0 {
		t.Fatalf("record tokens = %d, want > 0 (record %+v)", rec.Metrics.Tokens, rec)
	}
	if rec.Metrics.Cost <= 0 {
		t.Fatalf("record cost = %v, want > 0 (record %+v)", rec.Metrics.Cost, rec)
	}
	if rec.Metrics.Requests <= 0 {
		t.Fatalf("record requests = %d, want > 0 (record %+v)", rec.Metrics.Requests, rec)
	}
	if rec.Metrics.Duration <= 0 {
		t.Fatalf("record duration = %v, want > 0 (record %+v)", rec.Metrics.Duration, rec)
	}
	// The record and the roster read the same walk, so they agree.
	for _, row := range h.Roster() {
		if row.ID != id {
			continue
		}
		if row.Tokens != rec.Metrics.Tokens || row.Cost != rec.Metrics.Cost {
			t.Fatalf("roster row %+v disagrees with record metrics %+v", row, rec.Metrics)
		}
	}
}

// SetAgentChange is the seam a panel repaints on: it fires when an agent
// starts and when it settles, so the panel never polls.
func TestHubSetAgentChangeFiresOnStartAndSettle(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"changed"}`)}}}
	h := NewHub()
	var mu sync.Mutex
	var fired int
	h.SetAgentChange(func() {
		mu.Lock()
		fired++
		mu.Unlock()
	})
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "changed", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	mu.Lock()
	n := fired
	mu.Unlock()
	if n < 2 {
		t.Fatalf("agent-change fired %d times, want at least 2 (start + settle)", n)
	}
}

// recordByID finds one record in the hub's projection. It asserts on the
// records the hub really returns — never on a copy of the fixture.
func recordByID(t *testing.T, h *Hub, id string) AgentRecord {
	t.Helper()
	for _, rec := range h.AgentRecords() {
		if rec.ID == id {
			return rec
		}
	}
	t.Fatalf("no agent record for %q (records %+v)", id, h.AgentRecords())
	return AgentRecord{}
}

// yieldEventsWithUsage is yieldEvents plus the token and cost the provider
// reported, so a record's metrics have something to measure.
func yieldEventsWithUsage(args string, tokens int64) []ai.Event {
	evs := yieldEvents(args)
	last := evs[len(evs)-1]
	last.Usage = &ai.Usage{
		TotalTokens: tokens,
		Cost:        &ai.UsageCost{Total: 0.01},
	}
	evs[len(evs)-1] = last
	return evs
}
