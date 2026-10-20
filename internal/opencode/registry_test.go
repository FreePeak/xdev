package opencode

import (
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

func regBridge(t *testing.T, id, title string, unread int) *Bridge {
	t.Helper()
	f := &fake{routes: map[string]reply{}}
	c := &Client{Exec: f.exec}
	b := newBridgeWithStore(c, SessionInfo{
		ID: id, Title: title, Agent: "build", Cost: 0.25,
		Model:    ModelRef{ID: "m", ProviderID: "opencode", Variant: "high"},
		Tokens:   TokenUsage{Input: 10, Output: 2},
		Time:     TimeInfo{Created: 1000, Updated: 2000},
		Location: Location{Directory: "/tmp"},
	}, session.OpenMem("/tmp", title))
	for i := 0; i < unread; i++ {
		b.Handle(eventOf(t, "session.text.delta", map[string]any{
			"sessionID": id, "assistantMessageID": "m", "delta": "x",
		}))
	}
	t.Cleanup(func() { b.Store.Close() })
	return b
}

// The registry exists so N sessions cost one subscription. Routing by
// sessionID is what makes that correct.
func TestRegistryRoutesEventsToTheRightBridge(t *testing.T) {
	r := NewRegistry(nil)
	a := regBridge(t, "ses_a", "alpha", 0)
	bb := regBridge(t, "ses_b", "beta", 0)
	if _, err := r.Add(a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(bb); err != nil {
		t.Fatal(err)
	}
	r.Handle(eventOf(t, "session.text.delta", map[string]any{
		"sessionID": "ses_a", "assistantMessageID": "m", "delta": "for a",
	}))
	if n := len(a.Store.Entries()); n != 1 {
		t.Errorf("bridge a got %d entries, want 1", n)
	}
	if n := len(bb.Store.Entries()); n != 0 {
		t.Errorf("bridge b got %d entries, want 0", n)
	}
}

// opencode publishes every session on one stream, including ones xdev was
// never asked to show. An unknown session must not create a bridge.
func TestRegistryDropsUnknownSessions(t *testing.T) {
	r := NewRegistry(nil)
	r.Handle(eventOf(t, "session.text.delta", map[string]any{
		"sessionID": "ses_stranger", "delta": "not ours",
	}))
	if r.Len() != 0 {
		t.Fatalf("registry grew to %d for an unknown session", r.Len())
	}
}

func TestRegistryIgnoresSessionlessEvents(t *testing.T) {
	r := NewRegistry(nil)
	if _, err := r.Add(regBridge(t, "ses_a", "alpha", 0)); err != nil {
		t.Fatal(err)
	}
	before := r.Len()
	r.Handle(eventOf(t, "server.connected", map[string]any{}))
	r.Handle(eventOf(t, "project.updated", map[string]any{}))
	if r.Len() != before {
		t.Errorf("registry size changed to %d", r.Len())
	}
}

// Re-registering must return the live bridge, not replace it: replacing would
// orphan the store the tab is already reading.
func TestRegistryAddIsIdempotent(t *testing.T) {
	r := NewRegistry(nil)
	a := regBridge(t, "ses_a", "alpha", 0)
	first, err := r.Add(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Add(regBridge(t, "ses_a", "alpha", 5))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("Add replaced a registered bridge")
	}
	if r.Len() != 1 {
		t.Errorf("registry size = %d, want 1", r.Len())
	}
}

func TestRegistryBoundsItsSet(t *testing.T) {
	r := NewRegistry(nil)
	for i := 0; i < maxBridges; i++ {
		if _, err := r.Add(regBridge(t, idOf(i), "s", 0)); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := r.Add(regBridge(t, "ses_overflow", "overflow", 0)); err != ErrRegistryFull {
		t.Errorf("err = %v, want ErrRegistryFull", err)
	}
}

func idOf(i int) string { return "ses_" + string(rune('a'+i)) }

// The roster is what a dock or a status line renders. Every field must come
// from Session.Info or usage.updated — reading a message to draw a row would
// make a periodic refresh expensive.
func TestRegistryRosterCarriesStatusWithoutReadingMessages(t *testing.T) {
	r := NewRegistry(nil)
	b := regBridge(t, "ses_a", "alpha", 3)
	if _, err := r.Add(b); err != nil {
		t.Fatal(err)
	}
	rows := r.Roster(time.UnixMilli(2500))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ID != "ses_a" || row.Title != "alpha" || row.Agent != "build" {
		t.Errorf("row = %+v", row)
	}
	if row.Model != "opencode/m:high" {
		t.Errorf("model = %q, want opencode/m:high", row.Model)
	}
	if row.Cost != 0.25 {
		t.Errorf("cost = %v", row.Cost)
	}
	if row.Tokens.Input != 10 || row.Unread != 3 {
		t.Errorf("tokens/unread = %+v/%d", row.Tokens, row.Unread)
	}
	if row.Idle != 500*time.Millisecond {
		t.Errorf("idle = %v, want 500ms", row.Idle)
	}
}

// Most-recently-active first, so the session you are looking at stays put.
func TestRegistryRosterOrdersByRecentActivity(t *testing.T) {
	r := NewRegistry(nil)
	a := regBridge(t, "ses_a", "alpha", 0)
	bb := regBridge(t, "ses_b", "beta", 0)
	if _, err := r.Add(a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(bb); err != nil {
		t.Fatal(err)
	}
	// a becomes active again.
	r.Handle(eventOf(t, "session.text.delta", map[string]any{
		"sessionID": "ses_a", "delta": "again",
	}))
	rows := r.Roster(time.Now())
	if rows[0].ID != "ses_a" {
		t.Errorf("first row = %q, want ses_a (most recently active)", rows[0].ID)
	}
}

func TestSortRowsByIdle(t *testing.T) {
	rows := []RosterRow{
		{ID: "fresh", Idle: time.Second},
		{ID: "stuck", Idle: time.Hour},
	}
	SortRowsByIdle(rows)
	if rows[0].ID != "stuck" {
		t.Errorf("first = %q, want the most idle one first", rows[0].ID)
	}
}

func TestRegistryGet(t *testing.T) {
	r := NewRegistry(nil)
	b := regBridge(t, "ses_a", "alpha", 0)
	if _, err := r.Add(b); err != nil {
		t.Fatal(err)
	}
	if r.Get("ses_a") != b {
		t.Error("Get did not return the registered bridge")
	}
	if r.Get("ses_nope") != nil {
		t.Error("Get on a miss should be nil")
	}
}

// The bridged store must be renderable by the existing block renderer, which is
// the whole claim of R1. Assert on the block types the store ended up with.
func TestBridgedStoreRendersThroughExistingBlocks(t *testing.T) {
	b := regBridge(t, "ses_a", "alpha", 0)
	b.Handle(eventOf(t, "session.text.delta", map[string]any{
		"sessionID": "ses_a", "assistantMessageID": "m", "delta": "hi",
	}))
	b.Handle(eventOf(t, "session.tool.called", map[string]any{
		"sessionID": "ses_a", "assistantMessageID": "m", "id": "c1", "name": "read",
		"input": map[string]any{"path": "/tmp"},
	}))
	ents := b.Store.Entries()
	if len(ents) != 2 {
		t.Fatalf("entries = %d, want 2", len(ents))
	}
	first := messageOf(t, ents[0])
	if _, ok := first.Content[0].(ai.TextBlock); !ok {
		t.Errorf("entry 0 block = %T, want ai.TextBlock", first.Content[0])
	}
	second := messageOf(t, ents[1])
	if _, ok := second.Content[0].(ai.ToolCallBlock); !ok {
		t.Errorf("entry 1 block = %T, want ai.ToolCallBlock", second.Content[0])
	}
}
