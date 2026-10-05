package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// A foreground child is not a job: the `task` call blocks on it, so /hub (whose
// roster lists jobs only) could never show its transcript — which is the one
// thing a user watching a long spawn wants. TrackForeground is the seam that
// makes it readable, and this pins the whole chain: the spawn records it, the
// child's start event carries its id, and Transcript serves it.
func TestForegroundChildTranscriptIsReadable(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"mapped it"}`)}}}
	h := NewHub()
	tt := taskTool(p)
	tt.Hub = h
	var start agent0
	tt.OnEvent = func(ev SubagentEvent) {
		if ev.Kind == SubagentStart {
			start.TranscriptID = ev.TranscriptID
			start.Label = ev.Label
		}
	}
	res, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"map the app shell","name":"scout"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("spawn failed: %+v", res)
	}
	if start.TranscriptID == "" {
		t.Fatal("the child's start event carried no transcript id, so no host could open it")
	}
	if start.Label != "scout" {
		t.Fatalf("start event label = %q, want scout", start.Label)
	}

	rows := h.Foreground()
	if len(rows) != 1 || rows[0].ID != start.TranscriptID {
		t.Fatalf("foreground roster = %+v, want the child's own id", rows)
	}
	if rows[0].Name != "scout" || rows[0].Status != "done" {
		t.Fatalf("foreground row = %+v, want scout done", rows[0])
	}

	entries, total, ok := h.Transcript(start.TranscriptID, 0)
	if !ok {
		t.Fatal("a tracked foreground child's transcript must be readable")
	}
	if total < 2 || len(entries) != total {
		t.Fatalf("transcript total = %d, entries = %d", total, len(entries))
	}
	if entries[0].Role != string(ai.RoleUser) || !strings.Contains(entries[0].Text, "map the app shell") {
		t.Fatalf("first entry = %+v, want the child's own prompt", entries[0])
	}
	// The handoff itself is the yield tool's acceptance, and its RESULT
	// lives in the tool arguments the transcript does not carry — so the
	// evidence that this is the child's own run is the acceptance line the
	// hub's own transcript test pins, not the payload text.
	var sawHandoff bool
	for _, e := range entries {
		if strings.Contains(e.Text, "yield accepted") {
			sawHandoff = true
		}
	}
	if !sawHandoff {
		t.Fatalf("transcript missing the child's handoff: %+v", entries)
	}
}

// A foreground child must not leak into the job roster: /hub's rows carry
// kill/park/revive, and none of those act on a child the tool call owns.
func TestForegroundChildStaysOutOfTheJobRoster(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"ok"}`)}}}
	h := NewHub()
	tt := taskTool(p)
	tt.Hub = h
	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"work","name":"inline"}`)); err != nil {
		t.Fatal(err)
	}
	if got := h.Roster(); len(got) != 0 {
		t.Fatalf("job roster = %+v, want empty: a foreground child is not a job", got)
	}
	if got := h.Jobs(); len(got) != 0 {
		t.Fatalf("jobs = %+v, want empty", got)
	}
}

// Two spawns get two ids, and each child's transcript stays its own — the
// binding is per spawn, not per label.
func TestTwoForegroundChildrenKeepSeparateTranscripts(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"first"}`)},
		{events: yieldEvents(`{"result":"second"}`)},
	}}
	h := NewHub()
	tt := taskTool(p)
	tt.Hub = h
	ids := map[string]bool{}
	for _, name := range []string{"alpha", "beta"} {
		tt.OnEvent = func(ev SubagentEvent) {
			if ev.Kind == SubagentStart {
				ids[ev.Label] = ev.TranscriptID != ""
			}
		}
		if _, err := tt.Execute(context.Background(), json.RawMessage(`{"prompt":"do it","name":"`+name+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	for name, tracked := range ids {
		if !tracked {
			t.Fatalf("%s was not tracked", name)
		}
	}
	rows := h.Foreground()
	if len(rows) != 2 {
		t.Fatalf("foreground roster = %+v, want two rows", rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			t.Fatalf("two children share the id %q", r.ID)
		}
		seen[r.ID] = true
	}
}

// agent0 is a tiny receiver: the closure above needs one addressable field to
// write, and a struct literal inside the test would need a pointer anyway.
type agent0 struct{ Label, TranscriptID string }
