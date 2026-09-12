package tool

// Session persistence for the todo tool: a tiny adapter from *session.Store
// to TodoSink. Lives here so every cmd entrypoint (print, tui, rpc) can
// wire it with one line next to wireTaskParent.

import (
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
// every state-changing todo call also appends a user_todo_edit entry.
// Returns false when the registry holds no todo tool.
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
	return true
}
