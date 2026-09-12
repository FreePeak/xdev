package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// EventType names oneAssistantMessageEvent kind. Event names match omp's
// unified stream contract verbatim (research IV.4 / PRD §3.3).
type EventType string

const (
	EventStart         EventType = "start"
	EventTextStart     EventType = "text_start"
	EventTextDelta     EventType = "text_delta"
	EventTextEnd       EventType = "text_end"
	EventThinkingStart EventType = "thinking_start"
	EventThinkingDelta EventType = "thinking_delta"
	EventThinkingEnd   EventType = "thinking_end"
	EventToolcallStart EventType = "toolcall_start"
	EventToolcallDelta EventType = "toolcall_delta"
	EventToolcallEnd   EventType = "toolcall_end"
	EventImageEnd      EventType = "image_end"
	EventDone          EventType = "done"
	EventError         EventType = "error"
)

// Event is one element of the unified assistant stream. Exactly the fields
// relevant to Type are populated.
type Event struct {
	Type EventType

	// start
	Provider string
	API      string
	Model    string

	// text_* / thinking_*
	Delta    string // incremental text (text_delta, thinking_delta)
	Snapshot string // full accumulated text so far (text_delta)

	// toolcall_start / toolcall_delta / toolcall_end
	ToolCallID  string
	ToolName    string // toolcall_start
	StreamIndex int
	PartialJSON string // toolcall_delta: accumulated raw args JSON

	// done
	StopReason StopReason
	Usage      *Usage
	// done / error
	Message *Message // fully materialized assistant message (done only)

	// error
	Err error
}

// Errorf builds an EventError.
func Errorf(err error) Event { return Event{Type: EventError, Err: err} }

// Donef builds an EventDone.
func Donef(reason StopReason, usage *Usage, msg *Message) Event {
	return Event{Type: EventDone, StopReason: reason, Usage: usage, Message: msg}
}

// Drain consumes the rest of a stream after an error, discarding events.
func Drain(ch <-chan Event) {
	if ch == nil {
		return
	}
	for range ch {
	}
}

// FirstError returns the first error event encountered while consuming ch,
// draining the stream. Returns nil if the stream completes without error.
func FirstError(ch <-chan Event) error {
	var err error
	for ev := range ch {
		if ev.Type == EventError && err == nil {
			err = ev.Err
		}
	}
	return err
}

// Complete runs one request without tools and returns the assistant message.
// Side jobs (a session title, a summary, a memory consolidation) need the
// text of one reply and nothing else; each of them rolling its own stream loop
// is how a cancelled request or a dropped error event slips through, so the
// loop lives here. The Done event's message wins when present (it carries the
// provider's final shape); otherwise the accumulated deltas are returned.
func Complete(ctx context.Context, p Provider, model, system, prompt string, maxTokens int) (*Message, error) {
	if p == nil {
		return nil, errors.New("ai: no provider")
	}
	ch, err := p.Stream(ctx, StreamRequest{
		System: system,
		Model:  model,
		Messages: []Message{{Role: RoleUser,
			Content: []Block{TextBlock{Text: prompt}}}},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("ai: stream start: %w", err)
	}
	var (
		sb      strings.Builder
		final   *Message
		streamE error
	)
	for ev := range ch {
		switch ev.Type {
		case EventTextDelta:
			sb.WriteString(ev.Delta)
		case EventError:
			if streamE == nil {
				streamE = ev.Err
			}
		case EventDone:
			if ev.Message != nil {
				final = ev.Message
			}
			if ev.StopReason == StopReasonError && streamE == nil {
				streamE = errors.New("ai: stream ended in error")
			}
		}
	}
	if streamE != nil {
		Drain(ch)
		return nil, streamE
	}
	if final != nil {
		return final, nil
	}
	if sb.Len() == 0 {
		return nil, errors.New("ai: empty completion")
	}
	return &Message{Role: RoleAssistant, Content: []Block{TextBlock{Text: sb.String()}},
		StopReason: StopReasonStop}, nil
}

func (e Event) String() string {
	switch e.Type {
	case EventTextDelta, EventThinkingDelta:
		return fmt.Sprintf("%s(%q)", e.Type, e.Delta)
	case EventToolcallStart:
		return fmt.Sprintf("toolcall_start(id=%s name=%s idx=%d)", e.ToolCallID, e.ToolName, e.StreamIndex)
	case EventDone:
		return fmt.Sprintf("done(%s)", e.StopReason)
	case EventError:
		return fmt.Sprintf("error(%v)", e.Err)
	default:
		return string(e.Type)
	}
}
