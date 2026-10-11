package agent

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// turnStartCut builds the shape prime's split-turn test uses: one huge turn
// (a big user message plus a big reply) followed by a small tail question.
// A keepRecent allowance between the tail's and the reply's size lands the cut
// on the reply — inside the first turn — which is the case xdev used to resolve
// by walking forward to the next user message and dropping the whole first turn
// from the summary.
func turnStartCut() []ai.Message {
	return []ai.Message{
		userText(strings.Repeat("x", 4000)),
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: strings.Repeat("y", 4000)}}},
		userText("small"),
	}
}

// TestFindCutPointReportsTheSplitTurnStart pins the missing half of the cut
// contract: when the kept-tail allowance falls inside a turn, the cut must
// report the turn's start index (the "split turn" arm) so the caller can
// summarize the turn prefix and keep the turn's own suffix verbatim.
//
// Before this the cut walked FORWARD to the next user message, which put the
// whole first turn on the discarded side: the summary replaced a turn whose
// suffix was still in the live context, so the model read the same turn twice
// — once as a summary, once as live messages — and the anchor entry sat after
// work the summary no longer described.
func TestFindCutPointReportsTheSplitTurnStart(t *testing.T) {
	msgs := turnStartCut()
	// keepRecent=1002 keeps exactly the 1002-token reply, so the cut lands on
	// that assistant message — a turn boundary the moment the reply is done,
	// but still INSIDE the first turn. That is the split arm: turnStart names
	// the turn's user message, and msgs[turnStart:cut] is the turn prefix the
	// summarizer treats apart from the older history.
	got := findCutPoint(msgs, 1002)
	if got.cut != 1 {
		t.Fatalf("cut = %d, want 1 (the reply that ends the first turn)", got.cut)
	}
	if !got.split() {
		t.Fatal("a cut that lands inside a turn must report the split-turn arm")
	}
	if got.turnStart != 0 {
		t.Fatalf("turnStart = %d, want 0 (the turn's own user message)", got.turnStart)
	}
	// The tail question alone fits in the allowance, so the cut sits on it —
	// a clean user boundary with nothing to split (the second test's shape).
	tail := findCutPoint([]ai.Message{
		userText(strings.Repeat("a", 4000)),
		userText("keep this tail question"),
	}, 5)
	if tail.cut != 1 || tail.split() {
		t.Fatalf("tail cut = %+v, want {1, no split}", tail)
	}
	// A mid-turn cut in a longer history reports the turn the cut sits in, not
	// the history start: the second turn's user message begins turn 2, so a cut
	// on the assistant reply that follows it names index 2, not 0.
	mid := findCutPoint([]ai.Message{
		userText("turn one"),
		userText("turn two: " + strings.Repeat("z", 4000)),
		{Role: ai.RoleAssistant, Content: []ai.Block{ai.TextBlock{Text: strings.Repeat("y", 4000)}}},
		userText("small"),
	}, 1002)
	if mid.cut != 2 {
		t.Fatalf("mid-turn cut = %d, want 2 (the reply following the second user message)", mid.cut)
	}
	if !mid.split() || mid.turnStart != 1 {
		t.Fatalf("mid-turn cut = %+v, want the split arm with turnStart 1", mid)
	}
}

// TestFindCutPointOnAUserBoundaryIsNotASplit is the other arm: a cut that
// already sits on a user message has no turn to split, so turnStart is
// history's start and the whole span before the cut is summarized.
func TestFindCutPointOnAUserBoundaryIsNotASplit(t *testing.T) {
	msgs := []ai.Message{
		userText(strings.Repeat("a", 4000)),
		userText("keep this tail question"),
	}
	got := findCutPoint(msgs, 5)
	if got.cut != 1 {
		t.Fatalf("cut = %d, want 1 (the tail user message)", got.cut)
	}
	if got.split() {
		t.Fatal("a user-boundary cut is not a split turn")
	}
}

// TestFindCutPointNeverSplitsAToolTurn is the invariant xdev already had and
// that must survive the walk: a kept tail that begins inside a tool exchange
// starts after the whole exchange.
func TestFindCutPointNeverSplitsAToolTurn(t *testing.T) {
	msgs := []ai.Message{
		userText("read the config"),
		callMsg("c1", "read", `{"path":"/tmp/cfg.go"}`),
		resultMsg("c1", "read", strings.Repeat("line\n", 900)),
		userText("keep this tail question"),
	}
	got := findCutPoint(msgs, 5)
	if got.cut != 3 {
		t.Fatalf("cut = %d, want 3 (past the whole tool exchange)", got.cut)
	}
	if got.split() {
		t.Fatal("a cut past a whole tool exchange is not a split turn")
	}
}
