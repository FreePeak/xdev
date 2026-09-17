package ai

import (
	"context"
	"strings"

	"github.com/FreePeak/xdev/internal/logx"
)

// In-band tool calling for text-dialect models (M14 #62).
//
// InBandProvider wraps any Provider for a model that cannot emit structured
// tool calls. Encoding a request means: drop the native tools field, inline the
// tool catalog and dialect guide into the system prompt, and rewrite replayed
// calls/results into dialect text. Decoding a response means: scan assistant
// text back into toolcall_* events (with synthesized ids) and patch the final
// message so the tool loop sees the calls.
//
// Callers opt in per provider/model — the dialect matrix in toolconv.go only
// says which dialect a family emits; native structured calls stay the default
// for hosts that parse them server-side.
//
// ponytail: text is buffered per block, so an in-band model streams text a
// block at a time instead of token by token. Ceiling: long answers lose
// incremental rendering. Upgrade path: an incremental scanner keyed on the
// dialect's openers once a live in-band model makes it worth it.

// InBandProvider adapts a wire adapter to a text tool dialect.
type InBandProvider struct {
	inner  Provider
	format ToolFormat
}

// NewInBandProvider wraps inner with the given dialect. A nil provider or a
// native/empty dialect returns inner unchanged.
func NewInBandProvider(inner Provider, f ToolFormat) Provider {
	if inner == nil || SupportsNativeToolCalls(f) {
		return inner
	}
	return &InBandProvider{inner: inner, format: f}
}

// NewResolvedInBandProvider wraps inner with the dialect resolved from the
// model id (unknown models stay native).
func NewResolvedInBandProvider(inner Provider, model string) Provider {
	return NewInBandProvider(inner, ResolveToolFormat(model))
}

func (p *InBandProvider) Name() string { return p.inner.Name() }
func (p *InBandProvider) API() string  { return p.inner.API() }

// Stream implements Provider.
func (p *InBandProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	encoded, err := p.encodeRequest(req)
	if err != nil {
		return nil, err
	}
	in, err := p.inner.Stream(ctx, encoded)
	if err != nil {
		return nil, err
	}
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		p.decodeStream(in, out)
	}()
	return out, nil
}

// encodeRequest is the request half: inline the catalog, drop native tools, and
// convert replayed structured calls/results into dialect text.
func (p *InBandProvider) encodeRequest(req StreamRequest) (StreamRequest, error) {
	out := req
	if len(req.Tools) > 0 {
		prompt := ToolFormatPrompt(p.format, req.Tools)
		if prompt != "" {
			out.System = strings.TrimRight(req.System, "\n")
			if out.System != "" {
				out.System += "\n\n"
			}
			out.System += prompt
		}
		out.Tools = nil
	}
	messages := make([]Message, 0, len(req.Messages))
	var pending []Message
	flushResults := func() string {
		if len(pending) == 0 {
			return ""
		}
		text := EncodeToolResults(p.format, pending)
		pending = pending[:0]
		return text
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleAssistant:
			calls := m.ToolCalls()
			if len(calls) == 0 {
				messages = append(messages, m)
				continue
			}
			encoded, err := EncodeToolCalls(p.format, calls)
			if err != nil {
				return req, err
			}
			content := make([]Block, 0, len(m.Content))
			for _, b := range m.Content {
				if _, isCall := b.(ToolCallBlock); isCall {
					continue
				}
				content = append(content, b)
			}
			if encoded != "" {
				content = append(content, TextBlock{Text: encoded})
			}
			rewritten := m
			rewritten.Content = content
			messages = append(messages, rewritten)
		case RoleToolResult:
			// Consecutive results collapse into one user message: these dialects
			// correlate results to calls positionally, not by id.
			pending = append(pending, m)
		default:
			if text := flushResults(); text != "" {
				messages = append(messages, Message{Role: RoleUser, Content: []Block{TextBlock{Text: text}}})
			}
			messages = append(messages, m)
		}
	}
	if text := flushResults(); text != "" {
		messages = append(messages, Message{Role: RoleUser, Content: []Block{TextBlock{Text: text}}})
	}
	out.Messages = messages
	return out, nil
}

// decodeStream is the response half: text blocks are held until they close,
// then split into visible text plus toolcall events; the done message is
// rewritten so its content carries the decoded calls.
func (p *InBandProvider) decodeStream(in <-chan Event, out chan<- Event) {
	var (
		text     strings.Builder
		inText   bool
		textSeen bool
	)
	emitText := func(residual string) {
		if residual == "" {
			return
		}
		out <- Event{Type: EventTextStart}
		out <- Event{Type: EventTextDelta, Delta: residual, Snapshot: residual}
		out <- Event{Type: EventTextEnd}
	}
	emitCalls := func(calls []ToolCallBlock) {
		for _, c := range calls {
			out <- Event{
				Type:        EventToolcallStart,
				ToolCallID:  c.ID,
				ToolName:    c.Name,
				StreamIndex: c.StreamIndex,
			}
			out <- Event{
				Type:        EventToolcallDelta,
				ToolCallID:  c.ID,
				ToolName:    c.Name,
				StreamIndex: c.StreamIndex,
				PartialJSON: c.PartialArgs,
			}
			out <- Event{
				Type:        EventToolcallEnd,
				ToolCallID:  c.ID,
				ToolName:    c.Name,
				StreamIndex: c.StreamIndex,
				PartialJSON: c.PartialArgs,
			}
		}
	}
	for ev := range in {
		switch ev.Type {
		case EventTextStart:
			inText = true
			textSeen = true
			text.Reset()
		case EventTextDelta:
			if !inText {
				inText = true
				textSeen = true
			}
			text.WriteString(ev.Delta)
		case EventTextEnd:
			calls, residual, err := DecodeToolCalls(p.format, text.String())
			if err != nil {
				// A malformed dialect body is not a stream error: keep the model's
				// text visible (no calls), which is what decodeContent already does
				// for the terminal message. The turn ends on its own terms and the
				// model sees its own output instead of the run dying.
				logx.Errorf("%s: in-band tool call decode: %v", p.format, err)
				residual = text.String()
			}
			emitText(residual)
			emitCalls(calls)
		case EventDone:
			if ev.Message != nil {
				msg := *ev.Message
				msg.Content = p.decodeContent(msg.Content)
				ev.Message = &msg
			}
			out <- ev
			return
		default:
			out <- ev
		}
	}
	if inText && textSeen {
		// The stream ended mid text block: decode what arrived rather than
		// dropping the calls (the provider already emitted its terminal event).
		calls, residual, err := DecodeToolCalls(p.format, text.String())
		if err == nil {
			emitText(residual)
			emitCalls(calls)
		}
	}
}

// decodeContent rewrites the terminal message's text blocks into visible text
// plus decoded calls.
func (p *InBandProvider) decodeContent(content []Block) []Block {
	out := make([]Block, 0, len(content))
	for _, b := range content {
		tb, ok := b.(TextBlock)
		if !ok {
			out = append(out, b)
			continue
		}
		calls, residual, err := DecodeToolCalls(p.format, tb.Text)
		if err != nil {
			out = append(out, b)
			continue
		}
		if residual != "" {
			out = append(out, TextBlock{Text: residual})
		}
		for _, c := range calls {
			out = append(out, c)
		}
	}
	return out
}
