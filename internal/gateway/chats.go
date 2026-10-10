package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Chats is the per-chat registry: which session a chat owns, and whether a
// turn is running for it right now. One mutex, one map — the daemon holds no
// other shared state.
//
// The session FILES are the durable conversations; this only records the
// pointer (chat → session id) plus the live worker, so a daemon restart
// resumes the same conversations and /stop can find the running child.
type Chats struct {
	mu    sync.Mutex
	state map[int64]*chatState
	path  string
}

type chatState struct {
	sessionID string
	title     string
	updated   time.Time
	busy      bool
	worker    *worker
}

// NewChats opens the registry. path is gateway/sessions.json under the data
// dir; a missing file starts empty (a fresh install).
func NewChats(path string) (*Chats, error) {
	c := &Chats{state: map[int64]*chatState{}, path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return c, nil
	}
	var rows map[string]chatRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		// A corrupt map must not take the bridge down: the session files on
		// disk are intact, so the cost of ignoring it is one fresh session.
		return c, nil
	}
	for _, r := range rows {
		if r.ChatID == 0 {
			continue
		}
		c.state[r.ChatID] = &chatState{sessionID: r.SessionID, title: r.Title, updated: r.Updated}
	}
	return c, nil
}

type chatRow struct {
	ChatID    int64     `json:"chatId"`
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title,omitempty"`
	Updated   time.Time `json:"updated"`
}

// Session returns the chat's session id (false when it never ran).
func (c *Chats) Session(chat int64) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state[chat]
	if st == nil || st.sessionID == "" {
		return "", false
	}
	return st.sessionID, true
}

// Count is how many chats own a session.
func (c *Chats) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, st := range c.state {
		if st.sessionID != "" {
			n++
		}
	}
	return n
}

// Path is the map file (for status output).
func (c *Chats) Path() string { return c.path }

// SetSession records which session a chat owns and persists the map. An empty
// sessionID clears the row (/new). A write failure is returned, never fatal:
// losing the pointer costs the next turn a fresh session, not the transcript.
func (c *Chats) SetSession(chat int64, sessionID, title string) error {
	c.mu.Lock()
	st := c.state[chat]
	if st == nil {
		st = &chatState{}
		c.state[chat] = st
	}
	st.sessionID, st.title = sessionID, title
	if sessionID != "" {
		st.updated = time.Now().UTC()
	}
	rows := make(map[string]chatRow, len(c.state))
	for id, s := range c.state {
		if s.sessionID == "" {
			continue
		}
		rows[fmt.Sprint(id)] = chatRow{ChatID: id, SessionID: s.sessionID, Title: s.title, Updated: s.updated}
	}
	c.mu.Unlock()

	raw, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// Forget drops the chat's session so the next message starts fresh (/new).
// The session file itself is left alone — the transcript is the user's.
func (c *Chats) Forget(chat int64) error { return c.SetSession(chat, "", "") }

// Acquire takes the chat's turn lease, waiting up to wait for it to free.
// A non-nil error means the chat is still busy: fail-closed, so a second
// message is answered rather than queueing unbounded work.
func (c *Chats) Acquire(chat int64, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		c.mu.Lock()
		st := c.state[chat]
		if st == nil {
			st = &chatState{}
			c.state[chat] = st
		}
		if !st.busy {
			st.busy = true
			c.mu.Unlock()
			return func() {
				c.mu.Lock()
				if s := c.state[chat]; s != nil {
					s.busy, s.worker = false, nil
				}
				c.mu.Unlock()
			}, nil
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("still working on the previous message — /stop cancels it")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// AttachWorker records the worker running for chat (nil clears it), so /stop
// can signal it and /status can report it.
func (c *Chats) AttachWorker(chat int64, w *worker) {
	c.mu.Lock()
	if st := c.state[chat]; st != nil {
		st.worker = w
	}
	c.mu.Unlock()
}

// Worker returns the chat's current worker (nil when none is running).
func (c *Chats) Worker(chat int64) *worker {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.state[chat]; st != nil {
		return st.worker
	}
	return nil
}
