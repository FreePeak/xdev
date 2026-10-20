package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// The picker is the point of the /model list: a phone user must be able to
// CHOOSE a model with one tap, not type a ref.
func TestModelCommandOffersAPicker(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.modelCommand(context.Background(), 1, "/model")

	kb := b.keyboardOf(1)
	if len(kb) == 0 {
		t.Fatal("the model list came without a keyboard — there is nothing to tap")
	}
	// One button per model, each carrying the ref it picks.
	want := map[string]bool{"onegw/execution": false, "onegw/free": false, "onegw/shared": false, "local/shared": false}
	for _, row := range kb {
		for _, button := range row {
			ref, ok := strings.CutPrefix(button.CallbackData, modelPickPrefix)
			if !ok {
				t.Errorf("button %q carries %q, want a model pick", button.Text, button.CallbackData)
				continue
			}
			if _, ok := want[ref]; !ok {
				continue
			}
			want[ref] = true
		}
	}
	for ref, seen := range want {
		if !seen {
			t.Errorf("no button for %q: the picker does not offer it", ref)
		}
	}
}

// A tap picks the model: the chat's choice changes, the tap is closed, and
// the list is re-marked in place.
func TestModelCallbackPicksTheModel(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.handle(context.Background(), callbackUpdate(modelPickPrefix+"onegw/free", 1))

	if ref := d.modelFor(1); ref != "onegw/free" {
		t.Errorf("after the tap the chat resolves to %q, want onegw/free", ref)
	}
	answers := b.sentAnswers()
	if len(answers) != 1 {
		t.Fatalf("answered %d taps, want 1 — a tap left open keeps spinning on the phone", len(answers))
	}
	if !strings.Contains(answers[0].Text, "onegw/free") {
		t.Errorf("the tap was closed with %q, want it to name the model", answers[0].Text)
	}
	// The list under the button is re-marked, not sent again.
	edits := b.sentEdits()
	if len(edits) == 0 {
		t.Fatal("the list was not re-marked after the tap")
	}
	if !strings.Contains(edits[len(edits)-1].Text, "onegw/free") {
		t.Errorf("the re-marked list does not carry the new choice: %q", edits[len(edits)-1].Text)
	}
}

// A tap on a model the catalog no longer has must not pin it: the worker
// would fail one turn later, and the user would blame the model.
func TestModelCallbackRefusesAnUnknownProvider(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.handle(context.Background(), callbackUpdate(modelPickPrefix+"nope/anything", 1))

	if ref := d.modelFor(1); ref != "" {
		t.Errorf("an unknown provider was pinned anyway: %q", ref)
	}
	answers := b.sentAnswers()
	if len(answers) != 1 {
		t.Fatalf("answered %d taps, want 1", len(answers))
	}
	if !strings.Contains(answers[0].Text, "unknown provider") {
		t.Errorf("the refusal does not name the problem: %q", answers[0].Text)
	}
}

// A tap in a chat that is not on the allowlist changes nothing: a button is
// not a pass.
func TestModelCallbackRefusesAChatOutsideTheAllowlist(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.handle(context.Background(), callbackUpdate(modelPickPrefix+"onegw/free", 99))

	if ref := d.modelFor(99); ref != "" {
		t.Errorf("a chat outside the allowlist pinned %q", ref)
	}
}
