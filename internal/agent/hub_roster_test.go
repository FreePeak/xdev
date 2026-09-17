package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// parked → revive round-trip: the revived child re-runs with the prior
// handoff injected as context, and the transcript accumulates both runs.
func TestHubParkReviveRoundTrip(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"first-pass"}`)},
		{events: yieldEvents(`{"result":"second-pass"}`)},
	}}
	h := NewHub()
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "revivable", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 3,
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
	rows := h.Roster()
	if len(rows) != 1 || rows[0].Status != "parked" {
		t.Fatalf("roster after park = %+v", rows)
	}
	if info, _ := h.Status(id); !info.Parked {
		t.Fatal("JobInfo.Parked not set")
	}

	// Send to a parked job revives it (issue #37).
	if err := h.Send(id, "keep going"); err != nil {
		t.Fatalf("send to parked: %v", err)
	}
	if rows := h.Roster(); len(rows) != 1 || rows[0].Status == "parked" {
		t.Fatalf("revived job still parked: %+v", rows)
	}
	if len(h.Wait(context.Background(), []string{id}, 10*time.Second)) == 0 {
		t.Fatal("revived job did not settle")
	}
	res, ok := h.Result(id)
	if !ok || !strings.Contains(res.Text, "second-pass") {
		t.Fatalf("revived result = %v ok=%v", res, ok)
	}
	// The second request carries the prior output plus the new instruction.
	if len(p.gotReqs) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(p.gotReqs))
	}
	revived := messageText(p.gotReqs[1].Messages[0])
	if !strings.Contains(revived, "first-pass") || !strings.Contains(revived, "keep going") {
		t.Fatalf("revival prompt missing context: %q", revived)
	}
	// Revive (no steering text) is refused unless the job is parked again.
	if err := h.Revive(id, "again"); err == nil {
		t.Fatal("revive of an unfinished/unparked job must error")
	}
	if err := h.Revive("hub-99", ""); err == nil {
		t.Fatal("revive of an unknown job must error")
	}
	if h.Park("hub-99") {
		t.Fatal("park of an unknown job must be false")
	}
}

// Revive re-launches a parked job with a generic continuation when no text
// is given, and refreshes the roster status to running.
func TestHubReviveWithoutText(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"one"}`)},
		{events: yieldEvents(`{"result":"two"}`)},
	}}
	h := NewHub()
	id, _ := h.Start(context.Background(), SubagentSpec{
		Name: "r", Prompt: "work", Provider: p, Model: "m", Tools: []tool.Tool{}, MaxTurns: 2,
	})
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	if !h.Park(id) {
		t.Fatal("park refused")
	}
	if err := h.Revive(id, ""); err != nil {
		t.Fatalf("revive: %v", err)
	}
	if len(h.Wait(context.Background(), []string{id}, 10*time.Second)) == 0 {
		t.Fatal("revived job did not settle")
	}
	if got := messageText(p.gotReqs[1].Messages[0]); !strings.Contains(got, "one") {
		t.Fatalf("generic revival lost the prior result: %q", got)
	}
}

// Roster snapshot fields: id/name/status/model/activity/cost, with the
// normalized status enum (running|idle|parked|done|failed).
func TestHubRosterSnapshotFields(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"done"}`)}}}
	h := NewHub()
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "snapshot", Prompt: "work", Provider: p, Model: "m-free",
		Tools: []tool.Tool{}, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	early := h.Roster()
	if len(early) != 1 {
		t.Fatalf("roster = %+v", early)
	}
	switch early[0].Status {
	case "idle", "running", "done":
	default:
		t.Fatalf("unexpected early status %q", early[0].Status)
	}
	if early[0].ID != id || early[0].Name != "snapshot" || early[0].Model != "m-free" {
		t.Fatalf("roster row = %+v", early[0])
	}
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	done := h.Roster()
	if len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("settled roster = %+v", done)
	}
	if done[0].Activity == "" {
		t.Fatal("activity is empty")
	}

	// A failed child maps to the "failed" status.
	pf := &fakeProvider{calls: []fakeScript{{err: &ai.HTTPError{API: "a", Status: 500, Body: "boom"}}}}
	idf, _ := h.Start(context.Background(), SubagentSpec{
		Name: "doomed", Prompt: "work", Provider: pf, Model: "m", Tools: []tool.Tool{}, MaxTurns: 1,
	})
	h.Wait(context.Background(), []string{idf}, 10*time.Second)
	rows := h.Roster()
	if len(rows) != 2 || rows[1].Status != "failed" {
		t.Fatalf("failed roster = %+v", rows)
	}
}

// Transcript serves the child's messages incrementally via fromSeq.
func TestHubTranscriptIncremental(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"tx-done"}`)}}}
	h := NewHub()
	id, _ := h.Start(context.Background(), SubagentSpec{
		Name: "tx", Prompt: "work", Provider: p, Model: "m", Tools: []tool.Tool{}, MaxTurns: 2,
	})
	h.Wait(context.Background(), []string{id}, 10*time.Second)

	all, total, ok := h.Transcript(id, 0)
	if !ok {
		t.Fatal("transcript of a known job must be ok")
	}
	if total < 2 || len(all) != total {
		t.Fatalf("transcript total = %d entries = %d", total, len(all))
	}
	if all[0].Role != string(ai.RoleUser) || all[0].Text != "work" {
		t.Fatalf("first transcript entry = %+v", all[0])
	}
	roles := map[string]bool{}
	var sawHandoff bool
	for _, e := range all {
		roles[e.Role] = true
		if strings.Contains(e.Text, "yield accepted") {
			sawHandoff = true
		}
	}
	if !roles[string(ai.RoleUser)] || !roles[string(ai.RoleAssistant)] || !sawHandoff {
		t.Fatalf("transcript missing run evidence: %+v", all)
	}
	if all[len(all)-1].Seq != total-1 {
		t.Fatalf("last entry seq = %d, total = %d", all[len(all)-1].Seq, total)
	}
	// Incremental fetch: only the tail after fromSeq.
	tail, total2, _ := h.Transcript(id, total-1)
	if total2 != total || len(tail) != 1 || tail[0].Seq != total-1 {
		t.Fatalf("incremental fetch = %d entries total=%d (want 1/%d)", len(tail), total2, total)
	}
	if _, _, ok := h.Transcript("hub-99", 0); ok {
		t.Fatal("transcript of an unknown job must not be ok")
	}
}

// HubTool list/inbox ops expose the roster and steering traffic.
func TestHubToolRosterAndInboxOps(t *testing.T) {
	blocker := &blockingTool{release: make(chan struct{})}
	defer blocker.unblock()
	p := &fakeProvider{calls: []fakeScript{
		{events: toolCallEvents("block", `{}`)},
		{events: yieldEvents(`{"result":"ops-done"}`)},
	}}
	h := NewHub()
	ht := &HubTool{Hub: h}
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "ops", Prompt: "go", Provider: p, Model: "m-ops",
		Tools: []tool.Tool{blocker}, MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitChildStarted(t, h, id)

	res, _ := ht.Execute(context.Background(), json.RawMessage(`{"op":"list"}`))
	if res.IsError || !strings.Contains(res.Text, id) || !strings.Contains(res.Text, "m-ops") {
		t.Fatalf("list op: %q", res.Text)
	}
	if _, err := ht.Execute(context.Background(), json.RawMessage(`{"op":"send","id":"`+id+`","text":"steer me"}`)); err != nil {
		t.Fatal(err)
	}
	res, _ = ht.Execute(context.Background(), json.RawMessage(`{"op":"inbox","id":"`+id+`"}`))
	if res.IsError || !strings.Contains(res.Text, "steer me") {
		t.Fatalf("inbox op: %q", res.Text)
	}
	if h.Park(id) {
		t.Fatal("park of a running job must be false")
	}
	blocker.unblock()
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	res, _ = ht.Execute(context.Background(), json.RawMessage(`{"op":"list"}`))
	if !strings.Contains(res.Text, "done") {
		t.Fatalf("settled list op: %q", res.Text)
	}
}

// waitChildStarted blocks until the job's child agent is live.
func waitChildStarted(t *testing.T, h *Hub, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ag := h.jobs[id].Agent
		h.mu.Unlock()
		if ag != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child of %s never started", id)
}
