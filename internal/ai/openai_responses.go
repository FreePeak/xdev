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

// OpenAIResponsesProvider speaks the openai-responses wire protocol:
// POST {baseURL}/responses with typed SSE events (event: names).
type OpenAIResponsesProvider struct {
	httpClient   *http.Client
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	model        string // default model when a request omits Model
	name         string
	// apiLabel overrides the wire name reported by API() so the v2 Responses
	// variants (azure-openai-responses, openai-codex-responses) reuse this
	// adapter instead of copying its SSE mapping.
	apiLabel string
	// store is the Responses `store` flag; nil omits it. Codex requires false
	// (no server-side conversation state).
	store *bool
	// behavior tunes the variant without duplicating buildRequest: strictTools
	// controls the strict-mode pipeline, sanitize runs the Responses schema
	// normalizer, and azureURL switches to the deployment-style endpoint.
	behavior responsesBehavior
}

// responsesBehavior is the option set the Responses variants differ by.
type responsesBehavior struct {
	// sanitize runs SanitizeSchemaForOpenAIResponses over every tool schema.
	sanitize bool
	// strictTools turns on the strict-mode pipeline (and emits `strict`).
	strictTools bool
	// requestKeyHeader sends the key in this header instead of Authorization
	// (Azure's `api-key` convention).
	requestKeyHeader string
}

// NewOpenAIResponsesProvider builds a provider. A nil hc uses the shared
// transport.
func NewOpenAIResponsesProvider(name, baseURL, apiKey string, headers map[string]string, hc *http.Client) *OpenAIResponsesProvider {
	if hc == nil {
		hc = wireHTTPClient
	}
	return &OpenAIResponsesProvider{
		httpClient:   hc,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		apiKey:       apiKey,
		extraHeaders: headers,
		name:         name,
	}
}

func (p *OpenAIResponsesProvider) Name() string { return p.name }

// API reports the wire name: the Responses variants override it so sessions
// record which transport actually served the turn.
func (p *OpenAIResponsesProvider) API() string {
	if p.apiLabel != "" {
		return p.apiLabel
	}
	return APIOpenAIResponses
}

// Wire shapes for the request body.

type openaiRespContent struct {
	Type string `json:"type"` // input_text | output_text
	Text string `json:"text"`
}

type openaiRespItem struct {
	Type string `json:"type,omitempty"` // message | function_call | function_call_output
	// message
	Role    string              `json:"role,omitempty"`
	Content []openaiRespContent `json:"content,omitempty"`
	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output
	Output string `json:"output,omitempty"`
}

type openaiRespTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// Strict is emitted only when the strict-mode pipeline actually enforced the
	// schema (fail-open: an unenforced schema must not claim strict).
	Strict bool `json:"strict,omitempty"`
}

type openaiRespRequest struct {
	Model        string           `json:"model"`
	Input        []openaiRespItem `json:"input"`
	Stream       bool             `json:"stream"`
	Instructions string           `json:"instructions,omitempty"`
	Tools        []openaiRespTool `json:"tools,omitempty"`
	Store        *bool            `json:"store,omitempty"`
	Reasoning    *struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
}

// buildRequest maps the unified conversation onto the responses input shape.
func (p *OpenAIResponsesProvider) buildRequest(req StreamRequest) ([]byte, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return nil, errors.New("openai-responses: no model configured")
	}
	wr := openaiRespRequest{
		Model:        model,
		Stream:       true,
		Instructions: req.System,
	}
	if req.Thinking != nil {
		wr.Reasoning = &struct {
			Effort string `json:"effort"`
		}{Effort: reasoningEffort(req.Thinking.Tokens)}
	}
	tools := req.Tools
	if p.behavior.sanitize {
		tools = NormalizeToolsForAPI(p.API(), tools)
	}
	for _, t := range tools {
		params, strict := t.Parameters, false
		if p.behavior.strictTools {
			params, strict = AdaptSchemaForStrict(t.Parameters, true)
		}
		wr.Tools = append(wr.Tools, openaiRespTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
			Strict:      strict,
		})
	}
	if p.store != nil {
		wr.Store = p.store
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			var content []openaiRespContent
			for _, b := range m.Content {
				if t, ok := b.(TextBlock); ok {
					content = append(content, openaiRespContent{Type: "input_text", Text: t.Text})
				}
			}
			if len(content) > 0 {
				wr.Input = append(wr.Input, openaiRespItem{Type: "message", Role: "user", Content: content})
			}
		case RoleAssistant:
			var text strings.Builder
			var pending *openaiRespItem // assistant message item, placed at first text block
			flush := func() {
				if pending != nil {
					pending.Content[0].Text = text.String()
					wr.Input = append(wr.Input, *pending)
					pending = nil
				}
			}
			for _, b := range m.Content {
				switch t := b.(type) {
				case TextBlock:
					if pending == nil {
						pending = &openaiRespItem{
							Type:    "message",
							Role:    "assistant",
							Content: []openaiRespContent{{Type: "output_text"}},
						}
					}
					text.WriteString(t.Text)
				case ToolCallBlock:
					flush()
					wr.Input = append(wr.Input, openaiRespItem{
						Type:      "function_call",
						CallID:    t.ID,
						Name:      t.Name,
						Arguments: emptyJSONObjectArgs(string(t.Arguments)),
					})
				}
			}
			flush()
		case RoleToolResult:
			wr.Input = append(wr.Input, openaiRespItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Text(),
			})
		}
	}
	return json.Marshal(wr)
}

func (p *OpenAIResponsesProvider) headers() map[string]string {
	h := map[string]string{}
	if p.apiKey != "" {
		if key := p.behavior.requestKeyHeader; key != "" {
			h[key] = p.apiKey
		} else {
			h["Authorization"] = "Bearer " + p.apiKey
		}
	}
	for k, v := range p.extraHeaders {
		h[k] = v
	}
	return h
}

// Stream implements Provider.
func (p *OpenAIResponsesProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	return p.streamAt(ctx, req, p.baseURL+"/responses", p.headers())
}

// streamAt is the shared request path of the Responses family: build the body,
// POST it to the variant's URL, and map the SSE stream onto the unified events.
func (p *OpenAIResponsesProvider) streamAt(ctx context.Context, req StreamRequest, url string, headers map[string]string) (<-chan Event, error) {
	body, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	sctx, cancel := context.WithCancel(ctx)
	resp, err := wirePost(sctx, p.httpClient, url, headers, body, p.API())
	if err != nil {
		cancel() // no goroutine will own it on this path
		return nil, err
	}
	ch := make(chan Event, 64)
	start := time.Now()
	go func() {
		defer resp.Body.Close()
		defer cancel()
		defer close(ch)
		p.stream(sctx, resp.Body, model, ch, start)
	}()
	return withWatchdog(ctx, cancel, ch, FirstProgressTimeout, IdleTimeout), nil
}

// openaiRespToolCallState accumulates one function_call by output index.
type openaiRespToolCallState struct {
	id    string // call_id
	name  string
	index int
	args  strings.Builder
}

func (p *OpenAIResponsesProvider) stream(ctx context.Context, r io.Reader, model string, ch chan<- Event, start time.Time) {
	var (
		emitted   bool
		inText    bool
		inThink   bool
		response  string
		usage     *Usage
		text      strings.Builder
		think     strings.Builder
		ttft      int64
		toolCalls = map[int]*openaiRespToolCallState{}
		order     []int
		msg       = Message{Role: RoleAssistant}
	)
	emit := func(ev Event) {
		if !emitted {
			emitted = true
			ch <- Event{Type: EventStart, Provider: p.name, API: p.API(), Model: model}
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
	closeText := func() {
		if inText {
			inText = false
			msg.Content = append(msg.Content, TextBlock{Text: text.String()})
			emit(Event{Type: EventTextEnd})
		}
	}
	closeThink := func() {
		if inThink {
			inThink = false
			msg.Content = append(msg.Content, ThinkingBlock{Thinking: think.String(), ThinkingSignature: "reasoning_content"})
			emit(Event{Type: EventThinkingEnd})
		}
	}
	finishToolCalls := func() error {
		for _, idx := range order {
			st := toolCalls[idx]
			var args json.RawMessage
			if st.args.Len() > 0 {
				if err := json.Unmarshal([]byte(st.args.String()), &args); err != nil {
					return fmt.Errorf("openai-responses: tool call %s arguments: %w", st.id, err)
				}
			} else {
				args = json.RawMessage("{}")
			}
			emit(Event{
				Type:        EventToolcallEnd,
				ToolCallID:  st.id,
				ToolName:    st.name,
				StreamIndex: st.index,
			})
			msg.Content = append(msg.Content, ToolCallBlock{
				ID:          st.id,
				Name:        st.name,
				Arguments:   args,
				PartialArgs: st.args.String(),
				StreamIndex: st.index,
			})
		}
		return nil
	}
	finish := func(reason StopReason) {
		if usage == nil {
			usage = &Usage{}
		}
		if err := finishToolCalls(); err != nil {
			fail(Errorf(err))
			return
		}
		msg.Provider, msg.API, msg.Model = p.name, p.API(), model
		msg.ResponseID, msg.StopReason, msg.Usage = response, reason, usage
		msg.DurationMS, msg.TTFTMS = time.Since(start).Milliseconds(), ttft
		emit(Donef(reason, usage, &msg))
	}

	reader := sse.NewReader(r)
	for {
		frame, err := reader.Next(ctx)
		// A final partial frame may arrive alongside EOF; dispatch it, then
		// treat the end of body as a missing terminal event.
		eof := errors.Is(err, io.EOF)
		if err != nil && !eof {
			if ctx.Err() != nil {
				fail(Errorf(contextErr(ctx)))
				return
			}
			fail(Errorf(fmt.Errorf("openai-responses: read stream: %w", err)))
			return
		}
		if frame.Data == "" {
			fail(Errorf(errors.New("openai-responses: stream ended without response.completed")))
			return
		}
		var data struct {
			Type string `json:"type"`
			Item struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
			Snapshot    string `json:"snapshot"`
			Arguments   string `json:"arguments"`
			Response    struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
				IncompleteDetails *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
				Usage struct {
					InputTokens        int64 `json:"input_tokens"`
					OutputTokens       int64 `json:"output_tokens"`
					InputTokensDetails *struct {
						CachedTokens int64 `json:"cached_tokens"`
					} `json:"input_tokens_details"`
					OutputTokensDetails *struct {
						ReasoningTokens int64 `json:"reasoning_tokens"`
					} `json:"output_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
			Message string `json:"message"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &data); err != nil {
			fail(Errorf(fmt.Errorf("openai-responses: decode event: %w", err)))
			return
		}
		name := frame.Event
		if name == "" {
			name = data.Type
		}
		switch name {
		case "response.output_text.delta":
			if !inText {
				closeThink()
				inText = true
				emit(Event{Type: EventTextStart})
			}
			text.WriteString(data.Delta)
			snap := data.Snapshot
			if snap == "" {
				snap = text.String()
			}
			emit(Event{Type: EventTextDelta, Delta: data.Delta, Snapshot: snap})
		case "response.output_text.done":
			closeText()
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if !inThink {
				closeText()
				inThink = true
				emit(Event{Type: EventThinkingStart})
			}
			think.WriteString(data.Delta)
			emit(Event{Type: EventThinkingDelta, Delta: data.Delta})
		case "response.reasoning_summary_text.done", "response.reasoning_text.done":
			closeThink()
		case "response.output_item.added":
			if data.Item.Type == "function_call" {
				closeText()
				closeThink()
				st := &openaiRespToolCallState{
					id:    data.Item.CallID,
					name:  data.Item.Name,
					index: data.OutputIndex,
				}
				toolCalls[data.OutputIndex] = st
				order = append(order, data.OutputIndex)
				emit(Event{Type: EventToolcallStart, ToolCallID: st.id, ToolName: st.name, StreamIndex: st.index})
			}
		case "response.function_call_arguments.delta":
			if st := toolCalls[data.OutputIndex]; st != nil {
				st.args.WriteString(data.Delta)
				emit(Event{Type: EventToolcallDelta, ToolCallID: st.id, ToolName: st.name, StreamIndex: st.index, PartialJSON: st.args.String()})
			}
		case "response.function_call_arguments.done":
			if st := toolCalls[data.OutputIndex]; st != nil && data.Arguments != "" {
				st.args.Reset()
				st.args.WriteString(data.Arguments)
			}
		case "response.completed", "response.incomplete":
			if data.Response.ID != "" {
				response = data.Response.ID
			}
			closeThink()
			closeText()
			u := &Usage{
				Output: data.Response.Usage.OutputTokens,
			}
			if data.Response.Usage.OutputTokensDetails != nil {
				u.ReasoningTokens = data.Response.Usage.OutputTokensDetails.ReasoningTokens
			}
			// input_tokens includes cached tokens on this wire; the unified
			// Usage keeps Input exclusive of CacheRead (omp invariant:
			// input + output + cacheRead = totalTokens).
			if data.Response.Usage.InputTokensDetails != nil {
				u.CacheRead = data.Response.Usage.InputTokensDetails.CachedTokens
			}
			u.Input = data.Response.Usage.InputTokens - u.CacheRead
			if u.Input < 0 {
				u.Input = 0
			}
			u.TotalTokens = u.Input + u.Output + u.CacheRead
			usage = u
			reason := StopReasonStop
			if data.Response.Status == "incomplete" || name == "response.incomplete" {
				reason = StopReasonLength
			}
			finish(reason)
			return
		case "response.failed", "error":
			errMsg := data.Message
			if errMsg == "" && data.Error != nil {
				errMsg = data.Error.Message
			}
			if errMsg == "" && data.Response.Error != nil {
				errMsg = data.Response.Error.Message
			}
			fail(Errorf(fmt.Errorf("openai-responses: provider error: %s", errMsg)))
			return
		}
	}
}
