package tool

// Session persistence for the todo tool: a tiny adapter from *session.Store
// to TodoSink. Lives here so every cmd entrypoint (print, tui, rpc) can
// wire it with one line next to wireTaskParent.

import (
	"encoding/json"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// sessionTodoSink persists todo snapshots as `user_todo_edit` custom session
// entries — omp's canonical replay record (the newest such entry wins over
// tool-result details when state is reconstructed).
type sessionTodoSink struct{ store *session.Store }

// AppendTodo implements TodoSink.
func (s sessionTodoSink) AppendTodo(phases []TodoPhase) error {
	return s.store.Append(&session.CustomEntry{
		CustomType: "user_todo_edit",
		Data:       map[string]any{"phases": phases},
	})
}

// WireTodoSink attaches the session store to the registry's todo tool so
// every state-changing todo call also appends a user_todo_edit entry, and
// restores the list the session already has. Returns false when the registry
// holds no todo tool.
//
// Hydrating here (rather than in each entrypoint) is why /resume and /new are
// both correct: every caller already funnels through this one call.
func WireTodoSink(reg *Registry, store *session.Store) bool {
	if reg == nil || store == nil {
		return false
	}
	t, ok := reg.Get("todo")
	if !ok {
		return false
	}
	tt, ok := t.(*TodoTool)
	if !ok {
		return false
	}
	tt.Sink = sessionTodoSink{store: store}
	tt.adopt(replayPhases(store))
	return true
}

// adopt replaces the live list under the lock (session hydration).
func (t *TodoTool) adopt(phases []TodoPhase) {
	t.mu.Lock()
	t.phases = phases
	t.mu.Unlock()
}

// todoChain walks the active branch leaf→root, newest entry first, stopping
// at a reset boundary: /clear hides the lists written before it, exactly as
// it hides earlier notebook revisions (notesChain, internal/agent).
func todoChain(store *session.Store) []session.Entry {
	limit := len(store.Entries()) + 1 // cycle bound
	out := make([]session.Entry, 0, 8)
	for id := store.LeafID(); id != "" && len(out) < limit; {
		e := store.Entry(id)
		if e == nil {
			break
		}
		if _, isReset := e.(*session.ResetBoundaryEntry); isReset {
			break
		}
		out = append(out, e)
		id = e.Envelope().ParentID
	}
	return out
}

// replayPhases reconstructs the phased list from the transcript, newest
// source first: a `user_todo_edit` entry (the canonical record), else a
// non-error `todo` toolResult's details.phases (omp's replay order). nil
// means the session has no list — a real state, not a failure.
//
// A windowed store may have pruned everything before the last boundary, so
// finding nothing here is normal, not an error.
func replayPhases(store *session.Store) []TodoPhase {
	if store == nil {
		return nil
	}
	var fallback []TodoPhase
	for _, e := range todoChain(store) {
		switch v := e.(type) {
		case *session.CustomEntry:
			if v.CustomType != "user_todo_edit" || v.Data == nil {
				continue
			}
			if phases := decodePhases(v.Data["phases"]); phases != nil {
				return phases
			}
		case *session.MessageEntry:
			if fallback != nil || v.Message.Role != ai.RoleToolResult || v.Message.IsError {
				continue
			}
			if v.Message.ToolName != (&TodoTool{}).Name() {
				continue
			}
			if d, ok := v.Message.Details.(map[string]any); ok {
				fallback = decodePhases(d["phases"])
			}
		}
	}
	return fallback
}

// decodePhases turns either the in-process []TodoPhase payload or its disk
// round-trip ([]any of maps) back into the live type.
func decodePhases(v any) []TodoPhase {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var phases []TodoPhase
	if err := json.Unmarshal(raw, &phases); err != nil {
		return nil
	}
	return phases
}
