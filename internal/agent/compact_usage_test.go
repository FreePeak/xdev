package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// billingHooks counts the side-call usage the compaction path reports.
type billingHooks struct {
	TurnHooksFunc
	got []*ai.Usage
}

func (b *billingHooks) OnCompactionUsage(u *ai.Usage) { b.got = append(b.got, u) }

// billingProvider answers any request with a fixed usage on its done event.
type billingProvider struct {
	usage *ai.Usage
}

func (p *billingProvider) Stream(_ context.Context, req ai.StreamRequest) (<-chan ai.Event, error) {
	ch := make(chan ai.Event, 2)
	ch <- ai.Event{Type: ai.EventTextDelta, Delta: "SUMMARY"}
	ch <- ai.Donef(ai.StopReasonStop, p.usage, &ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.Block{ai.TextBlock{Text: "SUMMARY"}},
		StopReason: ai.StopReasonStop,
	})
	close(ch)
	return ch, nil
}

func (p *billingProvider) Name() string { return "billing" }
func (p *billingProvider) API() string  { return "billing-api" }

// TestSummarizeUsageIsCharged pins the defect: a compaction summarize is a
// billed provider request, and before this its usage was read off the done
// event and dropped on the floor. The tokens it read and wrote reached no
// counter, so a session could compact repeatedly and its report still said
// nothing had been spent.
func TestSummarizeUsageIsCharged(t *testing.T) {
	stubPressure(t, 0)
	u := &ai.Usage{
		Input: 900, Output: 120, CacheRead: 40_000, CacheWrite: 0,
		TotalTokens: 41_020, Cost: &ai.UsageCost{Total: 0.0187},
	}
	a, s := asyncAgent(t, &billingProvider{usage: u}, ladderConfig("handoff"))
	hooks := &billingHooks{}
	a.Hooks = hooks
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	// The persisted record: a resume reads the bill back off the summary, so
	// the usage has to be ON the summary, not only reported live.
	got := entries[0].Summary.Usage
	if got == nil {
		t.Fatal("the summary carries no usage — a resumed session reads $0.00 for the compaction that replaced its history")
	}
	if *got != *u {
		t.Fatalf("summary usage = %+v, want %+v", *got, *u)
	}
	if entries[0].Summary.Model != a.Model {
		t.Fatalf("summary model = %q, want %q", entries[0].Summary.Model, a.Model)
	}
	// The live report: the TUI banks it through the hook.
	if len(hooks.got) != 1 {
		t.Fatalf("compaction-usage notifications = %d, want 1", len(hooks.got))
	}
	if *hooks.got[0] != *u {
		t.Fatalf("notified usage = %+v, want %+v", *hooks.got[0], *u)
	}
}

// TestDeterministicCompactionChargesNothing is the other half of the
// contract: shake/soft/snapcompact make no provider call, so they must not
// report usage. A hook that fires with a zero usage would make /usage's
// cache-hit line wobble on every offline compaction.
func TestDeterministicCompactionChargesNothing(t *testing.T) {
	stubPressure(t, 0)
	a, s := asyncAgent(t, &billingProvider{usage: &ai.Usage{Input: 1}}, ladderConfig("soft"))
	hooks := &billingHooks{}
	a.Hooks = hooks
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	if entries[0].Summary.Usage != nil {
		t.Fatalf("a deterministic member invented usage: %+v", entries[0].Summary.Usage)
	}
	if len(hooks.got) != 0 {
		t.Fatalf("compaction-usage notifications = %d, want 0", len(hooks.got))
	}
}

// TestHandoffDocumentUsageIsCharged covers the other side call: /handoff runs
// its own provider round trip, and the document it produces is billed. The
// entry's summary is the only persisted record of that spend, so the usage
// must ride it — and the hook must see it through the same emission point, so
// a handoff cannot report a compaction notice without its cost.
func TestHandoffDocumentUsageIsCharged(t *testing.T) {
	store := session.OpenMem(t.TempDir(), "handoff")
	for _, text := range []string{
		"first turn: " + strings.Repeat("alpha ", 40),
		"second turn: " + strings.Repeat("beta ", 40),
	} {
		if err := store.Append(&session.MessageEntry{Message: ai.Message{
			Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: text}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	u := &ai.Usage{Input: 500, Output: 300, CacheRead: 1000, TotalTokens: 1800, Cost: &ai.UsageCost{Total: 0.0042}}
	hooks := &billingHooks{}
	ag := &Agent{
		Provider: &billingProvider{usage: u}, Tools: tool.NewRegistry(), Model: "big", Store: store,
		Compaction: CompactionConfig{ContextWindow: 200_000, KeepRecentTokens: 10},
		Hooks:      hooks,
	}
	if _, err := ag.HandoffDoc(context.Background(), "sys", ""); err != nil {
		t.Fatal(err)
	}
	last := store.Entries()[len(store.Entries())-1]
	ce, ok := last.(*session.CompactionEntry)
	if !ok {
		t.Fatalf("last entry is %T, want a CompactionEntry", last)
	}
	if ce.Summary.Usage == nil {
		t.Fatal("the handoff document carries no usage — the side call's spend is lost on resume")
	}
	if *ce.Summary.Usage != *u {
		t.Fatalf("handoff usage = %+v, want %+v", *ce.Summary.Usage, *u)
	}
	if len(hooks.got) != 1 || *hooks.got[0] != *u {
		t.Fatalf("handoff usage notifications = %d, want the document's own usage", len(hooks.got))
	}
}
