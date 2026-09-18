package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai/sse"
)

// AnthropicProvider speaks the anthropic-messages wire protocol:
// POST {baseURL}/v1/messages with x-api-key auth and typed SSE events.
type AnthropicProvider struct {
	httpClient   *http.Client
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	model        string // default model when a request omits Model
	name         string
}

// NewAnthropicProvider builds a provider. A nil hc uses the shared transport.
func NewAnthropicProvider(name, baseURL, apiKey string, headers map[string]string, hc *http.Client) *AnthropicProvider {
	if hc == nil {
		hc = wireHTTPClient
	}
	return &AnthropicProvider{
		httpClient:   hc,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		apiKey:       apiKey,
		extraHeaders: headers,
		name:         name,
	}
}

func (p *AnthropicProvider) Name() string { return p.name }
func (p *AnthropicProvider) API() string  { return APIAnthropicMessages }


// HealthCheck implements ai.HealthChecker. No key on the probe — the
// live call carries it; the endpoint answers catalog reads to any
// caller.
func (p *AnthropicProvider) HealthCheck(ctx context.Context) error {
	return healthCheckOneGet(ctx, p.httpClient, p.baseURL+"/v1/models", nil, APIAnthropicMessages)
}

func (p *AnthropicProvider) endpoint() string {
	if strings.HasSuffix(p.baseURL, "/v1") {
		return p.baseURL + "/messages"
	}
	return p.baseURL + "/v1/messages"
}

// Wire shapes for the request body.

type anthropicWireText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicWireBlock struct {
	Type      string              `json:"type"`
	Text      string              `json:"text,omitempty"`
	Thinking  string              `json:"thinking,omitempty"`
	ID        string              `json:"id,omitempty"`
	Name      string              `json:"name,omitempty"`
	Input     json.RawMessage     `json:"input,omitempty"`
	ToolUseID string              `json:"tool_use_id,omitempty"`
	Content   []anthropicWireText `json:"content,omitempty"`
	IsError   bool                `json:"is_error,omitempty"`
	// CacheControl ends a cached span here; nil leaves the block unmarked.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type anthropicWireMessage struct {
	Role    string               `json:"role"`
	Content []anthropicWireBlock `json:"content"`
}

type anthropicWireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	// CacheControl rides the LAST definition only. The tool array is the head of
	// the cached prefix on this wire (tools, then system, then messages), so one
	// marker writes every tool — and it survives a system-prompt change, which a
	// per-turn reminder is free to make.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type anthropicWireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicWireRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// System is a plain string, or — when the request is cacheable — an array of
	// text blocks, which is the only shape that can carry a marker. any is not
	// sloppiness: the endpoint accepts two different JSON types here.
	System   any                    `json:"system,omitempty"`
	Messages []anthropicWireMessage `json:"messages"`
	Stream   bool                   `json:"stream"`
	Tools    []anthropicWireTool    `json:"tools,omitempty"`
	Thinking *anthropicWireThinking `json:"thinking,omitempty"`
	// Metadata carries the stable installation identity the OAuth account is
	// keyed by (omp sends {device_id, session_id, account_uuid}); it rides the
	// documented `metadata.user_id` string field. #102: install-id minted a
	// file no request ever attached.
	Metadata *anthropicWireMetadata `json:"metadata,omitempty"`
}

type anthropicWireMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// buildRequest maps the unified conversation onto the Anthropic wire shape.
// Anthropic requires tool_result blocks in user role; consecutive toolResult
// messages merge into one user message content array.
func (p *AnthropicProvider) buildRequest(req StreamRequest) ([]byte, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return nil, errors.New("anthropic-messages: no model configured")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	wr := anthropicWireRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    req.System,
		Stream:    true,
	}
	// Prompt caching (#133). This wire caches a prefix in order — tools, then
	// system, then messages — so one marker per tier is what makes the write
	// worth reading back: a marker on the LAST tool writes the whole schema
	// even when a per-turn reminder changes the prompt after it, and a marker
	// at the end of the system array writes tools+system together. Only a
	// cacheable request changes shape: the system prompt has to ride as blocks
	// to hold a marker, so everything else keeps the plain string it always
	// sent. The remaining budget (Anthropic allows four) rolls over the
	// conversation tail below.
	cache := req.Cache.cacheWanted()
	if cache && req.System != "" {
		wr.System = []anthropicWireBlock{{Type: "text", Text: req.System}}
	}
	if uid := installIdentity(); uid != "" {
		wr.Metadata = &anthropicWireMetadata{UserID: uid}
	}
	if req.Thinking != nil {
		wr.Thinking = &anthropicWireThinking{Type: "enabled", BudgetTokens: req.Thinking.Tokens}
		// The API rejects max_tokens <= budget_tokens; leave room for output.
		if wr.MaxTokens <= req.Thinking.Tokens {
			wr.MaxTokens = req.Thinking.Tokens + 1024
		}
	}
	for _, t := range req.Tools {
		wr.Tools = append(wr.Tools, anthropicWireTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	if cache && len(wr.Tools) > 0 {
		wr.Tools[len(wr.Tools)-1].CacheControl = cacheMarker()
	}

	var msgs []anthropicWireMessage
	var pending []anthropicWireBlock // consecutive tool_result blocks
	flush := func() {
		if len(pending) == 0 {
			return
		}
		msgs = append(msgs, anthropicWireMessage{Role: "user", Content: pending})
		pending = nil
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			flush()
			var content []anthropicWireBlock
			for _, b := range m.Content {
				if t, ok := b.(TextBlock); ok {
					content = append(content, anthropicWireBlock{Type: "text", Text: t.Text})
				}
			}
			if len(content) > 0 {
				msgs = append(msgs, anthropicWireMessage{Role: "user", Content: content})
			}
		case RoleAssistant:
			flush()
			var content []anthropicWireBlock
			for _, b := range m.Content {
				switch t := b.(type) {
				case TextBlock:
					content = append(content, anthropicWireBlock{Type: "text", Text: t.Text})
				case ThinkingBlock:
					// Only replay thinking when the request re-enables it.
					if req.Thinking != nil {
						content = append(content, anthropicWireBlock{Type: "thinking", Thinking: t.Thinking})
					}
				case ToolCallBlock:
					// The same rule as on the stream side: a stored call whose
					// arguments are not strict JSON (an imported session, a log
					// from before the fix) is replayed as {}, never as a request
					// that dies in the encoder and takes every tool with it.
					input := parseToolArgs(APIAnthropicMessages, t.ID, string(t.Arguments))
					content = append(content, anthropicWireBlock{Type: "tool_use", ID: t.ID, Name: t.Name, Input: input})
				}
			}
			if len(content) > 0 {
				msgs = append(msgs, anthropicWireMessage{Role: "assistant", Content: content})
			}
		case RoleToolResult:
			var parts []anthropicWireText
			for _, b := range m.Content {
				if t, ok := b.(TextBlock); ok {
					parts = append(parts, anthropicWireText{Type: "text", Text: t.Text})
				}
			}
			pending = append(pending, anthropicWireBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   parts,
				IsError:   m.IsError,
			})
		}
	}
	flush()
	if cache {
		// Spend the budget bottom-up: the tail markers are the ones that make a
		// long run cheap, so the tiers above them only take what they need.
		used := 0
		if blocks, ok := wr.System.([]anthropicWireBlock); ok && len(blocks) > 0 {
			blocks[len(blocks)-1].CacheControl = cacheMarker()
			used++
		}
		if len(wr.Tools) > 0 {
			used++
		}
		tail := msgs
		if req.Cache.SideRequest {
			// A side request is served and dropped: marking its newest message
			// would write an entry nothing later reads. Stop at the last
			// completed tool round, whose prefix the next main turn does read.
			if n := lastToolResultMessage(msgs); n >= 0 {
				tail = msgs[:n+1]
			} else {
				tail = nil
			}
		}
		applyAnthropicBreakpoints(tail, cacheMaxBreakpoints-used)
	}
	wr.Messages = msgs
	return json.Marshal(wr)
}

// lastToolResultMessage is the index of the newest tool-result turn, or -1 when
// the conversation has none: the point a side request may still cache.
func lastToolResultMessage(msgs []anthropicWireMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, b := range msgs[i].Content {
			if b.Type == "tool_result" {
				return i
			}
		}
	}
	return -1
}

func (p *AnthropicProvider) headers() map[string]string {
	h := map[string]string{
		"anthropic-version": "2023-06-01",
	}
	if p.apiKey != "" {
		h["x-api-key"] = p.apiKey
	}
	for k, v := range p.extraHeaders {
		h[k] = v
	}
	return h
}

// Stream implements Provider.
func (p *AnthropicProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	// start is the fetch origin: connect + gateway queue + prefill belong
	// to the user's wait, so recorded ttft/duration match what is felt
	// (#283: stamping it after the headers made xdev's own metrics show
	// 7.3 s steps while users waited 17 s).
	start := time.Now()
	sctx, cancel := context.WithCancel(ctx)
	body, err := p.buildRequest(req)
	if err != nil {
		cancel()
		return nil, err
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	resp, err := wirePost(sctx, p.httpClient, p.endpoint(), p.headers(), body, APIAnthropicMessages)
	if err != nil && req.Cache.cacheWanted() && cacheRejected(err) {
		// The endpoint does not understand markers — a gateway, an older proxy.
		// Rebuild without them and retry once on the same context: one lost turn
		// is a bad trade for a cache that never fills, and the latch keeps every
		// later turn of this process from paying for the mistake again.
		disableCacheMarkers("endpoint rejected cache_control")
		req.Cache = CacheOpts{}
		if body, err = p.buildRequest(req); err != nil {
			cancel()
			return nil, err
		}
		resp, err = wirePost(sctx, p.httpClient, p.endpoint(), p.headers(), body, APIAnthropicMessages)
	}
	if err != nil {
		cancel() // no goroutine will own it on this path
		return nil, err
	}
	ch := make(chan Event, 64)
	go func() {
		defer resp.Body.Close()
		defer cancel()
		defer close(ch)
		p.stream(sctx, resp.Body, model, ch, start)
	}()
	return withWatchdog(ctx, cancel, ch, FirstProgressTimeout, IdleTimeout), nil
}

// anthropicBlockState tracks one open content block keyed by stream index.
type anthropicBlockState struct {
	kind  string // "text" | "thinking" | "tool_use"
	index int
	id    string
	name  string
	sig   string
	text  strings.Builder
	args  strings.Builder
}

func (p *AnthropicProvider) stream(ctx context.Context, r io.Reader, model string, ch chan<- Event, start time.Time) {
	var (
		ttft     int64
		emitted  bool // any event emitted yet (start sentinel)
		response string
		usage    *Usage
		outUsage Usage
		stop     StopReason
		blocks   []*anthropicBlockState
		open     = map[int]*anthropicBlockState{}
		msg      = Message{Role: RoleAssistant}
	)
	emit := func(ev Event) {
		if !emitted {
			emitted = true
			ch <- Event{Type: EventStart, Provider: p.name, API: APIAnthropicMessages, Model: model}
		}
		if ttft == 0 {
			switch ev.Type {
			case EventTextStart, EventTextDelta, EventThinkingStart, EventThinkingDelta,
				EventToolcallStart, EventToolcallDelta:
				ttft = time.Since(start).Milliseconds()
			}
		}
		ch <- ev
	}
	// fail sends a terminal error event without the start sentinel.
	fail := func(ev Event) {
		ch <- ev
	}
	beginBlock := func(st *anthropicBlockState) {
		blocks = append(blocks, st)
		open[st.index] = st
		switch st.kind {
		case "text":
			emit(Event{Type: EventTextStart, StreamIndex: st.index})
		case "thinking":
			emit(Event{Type: EventThinkingStart, StreamIndex: st.index})
		case "tool_use":
			emit(Event{Type: EventToolcallStart, ToolCallID: st.id, ToolName: st.name, StreamIndex: st.index})
		}
	}
	endBlock := func(st *anthropicBlockState) {
		switch st.kind {
		case "text":
			emit(Event{Type: EventTextEnd, StreamIndex: st.index})
		case "thinking":
			emit(Event{Type: EventThinkingEnd, StreamIndex: st.index})
		case "tool_use":
			emit(Event{Type: EventToolcallEnd, ToolCallID: st.id, ToolName: st.name, StreamIndex: st.index})
		}
		delete(open, st.index)
	}

	reader := sse.NewReader(r)
	for {
		frame, err := reader.Next(ctx)
		// A final partial frame may arrive together with EOF; it is
		// dispatched below before the end of stream is handled.
		if err != nil && !errors.Is(err, io.EOF) {
			if ctx.Err() != nil {
				fail(Errorf(contextErr(ctx)))
				return
			}
			fail(Errorf(fmt.Errorf("anthropic-messages: read stream: %w", err)))
			return
		}
		eof := errors.Is(err, io.EOF)
		var data struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			ContentBlock struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Thinking  string          `json:"thinking"`
				Text      string          `json:"text"`
				Input     json.RawMessage `json:"input"`
				Signature string          `json:"signature"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				ID    string `json:"id"`
				Usage struct {
					InputTokens              int64 `json:"input_tokens"`
					CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
					OutputTokens             int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if eof && frame.Data == "" {
			// Clean end of body without message_stop.
			fail(Errorf(errors.New("anthropic-messages: stream ended without message_stop")))
			return
		}
		if err := json.Unmarshal([]byte(frame.Data), &data); err != nil {
			fail(Errorf(malformedStream("anthropic-messages", "decode event", err)))
			return
		}
		switch data.Type {
		case "message_start":
			response = data.Message.ID
			outUsage = Usage{
				Input:      data.Message.Usage.InputTokens,
				CacheRead:  data.Message.Usage.CacheReadInputTokens,
				CacheWrite: data.Message.Usage.CacheCreationInputTokens,
			}
		case "content_block_start":
			st := &anthropicBlockState{index: data.Index}
			switch data.ContentBlock.Type {
			case "text":
				st.kind = "text"
				st.text.WriteString(data.ContentBlock.Text)
			case "thinking":
				st.kind = "thinking"
				st.text.WriteString(data.ContentBlock.Thinking)
			case "tool_use":
				st.kind = "tool_use"
				st.id = data.ContentBlock.ID
				st.name = data.ContentBlock.Name
			default:
				continue
			}
			beginBlock(st)
		case "content_block_delta":
			st := open[data.Index]
			if st == nil {
				continue
			}
			switch data.Delta.Type {
			case "text_delta":
				st.text.WriteString(data.Delta.Text)
				emit(Event{Type: EventTextDelta, Delta: data.Delta.Text, Snapshot: st.text.String(), StreamIndex: st.index})
			case "thinking_delta":
				st.text.WriteString(data.Delta.Thinking)
				emit(Event{Type: EventThinkingDelta, Delta: data.Delta.Thinking, StreamIndex: st.index})
			case "signature_delta":
				st.sig = data.Delta.Signature
			case "input_json_delta":
				st.args.WriteString(data.Delta.PartialJSON)
				emit(Event{Type: EventToolcallDelta, ToolCallID: st.id, ToolName: st.name, StreamIndex: st.index, PartialJSON: st.args.String()})
			}
		case "content_block_stop":
			if st := open[data.Index]; st != nil {
				endBlock(st)
			}
		case "message_delta":
			outUsage.Output = data.Usage.OutputTokens
			switch data.Delta.StopReason {
			case "end_turn", "stop_sequence":
				stop = StopReasonStop
			case "max_tokens":
				stop = StopReasonLength
			case "tool_use":
				stop = StopReasonStop
			case "refusal":
				fail(Errorf(errors.New("anthropic-messages: refusal")))
				return
			}
		case "error":
			fail(Errorf(fmt.Errorf("anthropic-messages: %s: %s", data.Error.Type, data.Error.Message)))
			return
		case "message_stop":
			if stop == "" {
				stop = StopReasonStop
			}
			usage = &outUsage
			usage.TotalTokens = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
			for _, st := range blocks {
				switch st.kind {
				case "text":
					msg.Content = append(msg.Content, TextBlock{Text: st.text.String()})
				case "thinking":
					msg.Content = append(msg.Content, ThinkingBlock{Thinking: st.text.String(), ThinkingSignature: st.sig})
				case "tool_use":
					args := parseToolArgs(APIAnthropicMessages, st.id, st.args.String())
					msg.Content = append(msg.Content, ToolCallBlock{
						ID:          st.id,
						Name:        st.name,
						Arguments:   args,
						PartialArgs: st.args.String(),
						StreamIndex: st.index,
					})
				}
			}
			msg.Provider, msg.API, msg.Model = p.name, APIAnthropicMessages, model
			msg.ResponseID, msg.StopReason, msg.Usage = response, stop, usage
			msg.DurationMS, msg.TTFTMS = time.Since(start).Milliseconds(), ttft
			emit(Donef(stop, usage, &msg))
			return
		}
	}
}

// contextErr resolves the cause of a canceled context.
func contextErr(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return context.Canceled
}
