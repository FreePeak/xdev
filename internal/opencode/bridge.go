package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// A Bridge is one opencode session bound to one xdev session.Store.
//
// The store is a tab. cmd/xdev/tabset.go's open() takes a *session.Store and
// keys tabs by its id, so a bridged session becomes a tab with no new tab type
// and no change to tabset.go — which is why this file can be written without
// touching the TUI layer that is currently mid-flight.
//
// The store is append-only (Store.Append), so a streaming turn arrives as a
// run of small entries rather than one entry rewritten in place. That is the
// honest shape for an append-only log, and blocks.go already renders adjacent
// text blocks as one flow.
type Bridge struct {
	// Info is the session this bridge follows.
	Info SessionInfo
	// Store is the xdev-side transcript. Hand it to tabset.open().
	Store *session.Store

	client *Client

	mu        sync.RWMutex
	cost      float64
	tokens    TokenUsage
	unread    int
	lastEvent time.Time
}

func newBridge(c *Client, info SessionInfo) *Bridge {
	return newBridgeWithStore(c, info, session.OpenMem(info.Location.Directory, info.Title))
}

func newBridgeWithStore(c *Client, info SessionInfo, st *session.Store) *Bridge {
	return &Bridge{
		Info:   info,
		Store:  st,
		client: c,
		cost:   info.Cost,
		tokens: info.Tokens,
	}
}

// Cost and Tokens report the live roll-up. session.usage.updated carries the
// same numbers as Session.Info, so a bridged tab's status row stays current
// without re-reading the session list.
func (b *Bridge) Cost() float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cost
}

func (b *Bridge) Tokens() TokenUsage {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.tokens
}

// Unread counts transcript entries appended while this tab was not focused.
// It is the whole "it did something" signal, and it is what the tabset's
// existing unread dot already expects.
func (b *Bridge) Unread() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.unread
}

// Run subscribes to the event stream and folds this session's events into the
// store until ctx ends. One Stream serves every session; Run only keeps the
// frames whose sessionID is this bridge's.
func (b *Bridge) Run(ctx context.Context, s *Stream) error {
	return s.Run(ctx, b.Handle)
}

// Handle folds one event into the bridge. It is exported so the routing can be
// tested without a live stream, and so a caller that already owns a stream
// can fan events out itself.
func (b *Bridge) Handle(e Event) {
	b.mu.Lock()
	b.lastEvent = time.UnixMilli(e.Created)
	b.mu.Unlock()

	// Usage is not transcript content: it refreshes counters only.
	if e.Type == "session.usage.updated" {
		b.mu.Lock()
		b.cost, b.tokens = e.Cost(), e.Tokens()
		b.mu.Unlock()
		return
	}
	// Everything below is another session's business. One global stream
	// serves every session, so this filter is what keeps a parked tab from
	// showing another tab's text.
	if id := e.SessionID(); id != "" && id != b.Info.ID {
		return
	}

	switch e.Type {
	case "session.text.delta", "session.reasoning.delta":
		b.appendDelta(e)

	case "session.tool.called", "session.tool.input.ended":
		b.appendToolCall(e)

	case "session.tool.success", "session.tool.error":
		b.appendToolResult(e)

	default:
		// Step boundaries, compactions, shell and project events are not
		// transcript content. Ignoring them is deliberate: appending a
		// placeholder would render a turn that never produced output.
	}
}

func (b *Bridge) append(e session.Entry) {
	if b.Store == nil {
		return
	}
	if err := b.Store.Append(e); err != nil {
		// A store write failure is not recoverable here, and silently
		// continuing would render a transcript with a hole in it. The
		// counter makes the loss visible rather than invisible.
		b.mu.Lock()
		b.unread++
		b.mu.Unlock()
		return
	}
	b.mu.Lock()
	b.unread++
	b.mu.Unlock()
}

// deltaPayload is the shared shape of the text and reasoning deltas.
type deltaPayload struct {
	SessionID string `json:"sessionID"`
	MessageID string `json:"assistantMessageID"`
	Ordinal   int    `json:"ordinal"`
	Delta     string `json:"delta"`
}

func (b *Bridge) appendDelta(e Event) {
	var p deltaPayload
	if err := json.Unmarshal(e.Data, &p); err != nil || p.Delta == "" {
		return
	}
	var blk ai.Block = ai.TextBlock{Text: p.Delta}
	if e.Type == "session.reasoning.delta" {
		blk = ai.ThinkingBlock{Thinking: p.Delta}
	}
	b.append(&session.MessageEntry{
		Env: session.Envelope{
			Type: session.TypeMessage, ID: "delta:" + p.MessageID,
			Timestamp: msTime(e.Created),
		},
		Message: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{blk}},
	})
}

// toolEventPayload is the shared shape of the tool events.
type toolEventPayload struct {
	SessionID string          `json:"sessionID"`
	MessageID string          `json:"assistantMessageID"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   []ocToolContent `json:"content"`
	Error     json.RawMessage `json:"error"`
}

func (b *Bridge) appendToolCall(e Event) {
	var p toolEventPayload
	if err := json.Unmarshal(e.Data, &p); err != nil || p.ID == "" {
		return
	}
	blk := ai.ToolCallBlock{ID: p.ID, Name: p.Name, Arguments: p.Input}
	b.append(&session.MessageEntry{
		Env: session.Envelope{
			Type: session.TypeMessage, ID: "tool:" + p.ID,
			Timestamp: msTime(e.Created),
		},
		Message: ai.Message{Role: ai.RoleAssistant, Content: []ai.Block{blk}},
	})
}

func (b *Bridge) appendToolResult(e Event) {
	var p toolEventPayload
	if err := json.Unmarshal(e.Data, &p); err != nil || p.ID == "" {
		return
	}
	msg := ai.Message{
		Role:    ai.RoleToolResult,
		Content: []ai.Block{ai.TextBlock{Text: toolContentText(p.Content)}},
	}
	if len(p.Error) > 0 && string(p.Error) != "null" {
		msg.StopReason = ai.StopReasonError
		msg.Content = append(msg.Content, ai.TextBlock{Text: string(p.Error)})
	}
	b.append(&session.MessageEntry{
		Env: session.Envelope{
			Type: session.TypeMessage, ID: p.ID + ":result",
			Timestamp: msTime(e.Created),
		},
		Message: msg,
	})
}

// Reconcile re-reads the session's messages and appends what the store is
// missing. This is how a reconnect closes a gap.
//
// It cannot be done by replaying the stream: every *.delta is ephemeral and
// carries no durable seq, so the deltas that would fill a gap are gone the
// moment the connection drops (F2). The message list is the only durable
// record, so this is the only correct way to heal.
func (b *Bridge) Reconcile(ctx context.Context) error {
	if b.client == nil {
		return fmt.Errorf("opencode: bridge has no client")
	}
	msgs, err := b.client.Messages(ctx, b.Info.ID)
	if err != nil {
		return err
	}
	raws := make([]string, 0, len(msgs))
	for _, m := range msgs {
		raws = append(raws, string(m))
	}
	entries, err := AdaptMessages(raws)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e == nil {
			continue
		}
		// Skip what the store already holds, so calling Reconcile twice is
		// harmless — which matters because a flapping connection calls it
		// repeatedly.
		if b.Store != nil {
			if existing := b.Store.Entry(e.Envelope().ID); existing != nil {
				continue
			}
		}
		b.append(e)
	}
	// Refresh the counters from the authoritative row.
	if info, err := b.client.GetSession(ctx, b.Info.ID); err == nil {
		b.mu.Lock()
		b.Info = info
		b.cost, b.tokens = info.Cost, info.Tokens
		b.mu.Unlock()
	}
	return nil
}

// Prompt sends a turn on this session.
func (b *Bridge) Prompt(ctx context.Context, text string) error {
	if b.client == nil {
		return fmt.Errorf("opencode: bridge has no client")
	}
	return b.client.Prompt(ctx, b.Info.ID, text)
}

// Close releases the store. The caller owns the bridge's lifetime; the
// event stream is owned by whoever started Stream.Run.
func (b *Bridge) Close() error {
	if b.Store == nil {
		return nil
	}
	return b.Store.Close()
}
