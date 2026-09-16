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

// OpenAICompletionsProvider speaks the openai-completions wire protocol:
// POST {baseURL}/chat/completions with data:-framed SSE and a [DONE] sentinel.
type OpenAICompletionsProvider struct {
	httpClient   *http.Client
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	model        string // default model when a request omits Model
	name         string
}

// NewOpenAICompletionsProvider builds a provider. A nil hc uses the shared
// transport.
func NewOpenAICompletionsProvider(name, baseURL, apiKey string, headers map[string]string, hc *http.Client) *OpenAICompletionsProvider {
	if hc == nil {
		hc = wireHTTPClient
	}
	return &OpenAICompletionsProvider{
		httpClient:   hc,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		apiKey:       apiKey,
		extraHeaders: headers,
		name:         name,
	}
}

func (p *OpenAICompletionsProvider) Name() string { return p.name }
func (p *OpenAICompletionsProvider) API() string  { return APIOpenAICompletions }

// Wire shapes for the request body.

type openaiWireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiWireToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiWireFunction `json:"function"`
}

type openaiWireMessage struct {
	Role string `json:"role"`
	// Content is a string for every text-only message and a []part array for
	// one carrying images — the two shapes the endpoint accepts. any is not
	// sloppiness here: the array form is a different JSON type, so no single
	// Go type can name both.
	Content    any                  `json:"content"`
	ToolCalls  []openaiWireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
}

// openaiWirePart is one element of a multimodal content array. Only image_url
// uses the nested shape; text parts leave URL nil, which omits the object.
type openaiWirePart struct {
	Type     string              `json:"type"`
	Text     string              `json:"text,omitempty"`
	ImageURL *openaiWireImageURL `json:"image_url,omitempty"`
}

type openaiWireImageURL struct {
	URL string `json:"url"`
}

type openaiWireTool struct {
	Type     string             `json:"type"`
	Function openaiWireToolSpec `json:"function"`
}

type openaiWireToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openaiWireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiWireRequest struct {
	Model           string                   `json:"model"`
	Messages        []openaiWireMessage      `json:"messages"`
	Stream          bool                     `json:"stream"`
	StreamOptions   *openaiWireStreamOptions `json:"stream_options,omitempty"`
	Tools           []openaiWireTool         `json:"tools,omitempty"`
	ReasoningEffort string                   `json:"reasoning_effort,omitempty"`
	// PromptCacheKey is this wire's cache-affinity field: a stable value routes
	// a session's turns to one cached prefix. It rides only on a first-party
	// endpoint (see promptCacheKey).
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

// userContent maps a user message onto this wire: a plain string when it is
// text, the part array when it carries images. An image is sent as a data URL,
// which is the only form the chat endpoint accepts inline.
func userContent(m Message) any {
	imgs := 0
	for _, b := range m.Content {
		if _, ok := b.(ImageBlock); ok {
			imgs++
		}
	}
	if imgs == 0 {
		return m.Text()
	}
	parts := []openaiWirePart{}
	if t := m.Text(); t != "" {
		parts = append(parts, openaiWirePart{Type: "text", Text: t})
	}
	for _, b := range m.Content {
		ib, ok := b.(ImageBlock)
		if !ok {
			continue
		}
		if s := ib.Source; s.Type == "base64" {
			parts = append(parts, openaiWirePart{Type: "image_url", ImageURL: &openaiWireImageURL{
				URL: "data:" + s.MediaType + ";base64," + s.Data,
			}})
		} else {
			parts = append(parts, openaiWirePart{Type: "image_url", ImageURL: &openaiWireImageURL{URL: s.Data}})
		}
	}
	return parts
}

// buildRequest maps the unified conversation onto the chat/completions shape.
// Content is a plain string on this wire; thinking blocks are dropped (the
// OpenAI API rejects replaying reasoning); assistant tool calls ride in
// tool_calls with content null.
func (p *OpenAICompletionsProvider) buildRequest(req StreamRequest) ([]byte, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return nil, errors.New("openai-completions: no model configured")
	}
	wr := openaiWireRequest{
		Model:         model,
		Stream:        true,
		StreamOptions: &openaiWireStreamOptions{IncludeUsage: true},
	}
	if k := promptCacheKey(p.baseURL, req.Cache.Key); k != "" {
		wr.PromptCacheKey = k
	}
	if req.System != "" {
		wr.Messages = append(wr.Messages, openaiWireMessage{Role: "system", Content: req.System})
	}
	if req.Thinking != nil {
		wr.ReasoningEffort = reasoningEffort(req.Thinking.Tokens)
	}
	for _, t := range req.Tools {
		wr.Tools = append(wr.Tools, openaiWireTool{
			Type: "function",
			Function: openaiWireToolSpec{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			wr.Messages = append(wr.Messages, openaiWireMessage{Role: "user", Content: userContent(m)})
		case RoleAssistant:
			var toolCalls []openaiWireToolCall
			var text strings.Builder
			for _, b := range m.Content {
				switch t := b.(type) {
				case TextBlock:
					text.WriteString(t.Text)
				case ToolCallBlock:
					toolCalls = append(toolCalls, openaiWireToolCall{
						ID:   t.ID,
						Type: "function",
						Function: openaiWireFunction{
							Name:      t.Name,
							Arguments: string(parseToolArgs(APIOpenAICompletions, t.ID, string(t.Arguments))),
						},
					})
				}
			}
			wm := openaiWireMessage{Role: "assistant", Content: nil, ToolCalls: toolCalls}
			if toolCalls == nil {
				wm.Content = text.String()
			}
			wr.Messages = append(wr.Messages, wm)
		case RoleToolResult:
			wr.Messages = append(wr.Messages, openaiWireMessage{
				Role:       "tool",
				Content:    m.Text(),
				ToolCallID: m.ToolCallID,
			})
		}
	}
	return json.Marshal(wr)
}

func (p *OpenAICompletionsProvider) headers() map[string]string {
	h := map[string]string{}
	if p.apiKey != "" {
		h["Authorization"] = "Bearer " + p.apiKey
	}
	for k, v := range p.extraHeaders {
		h[k] = v
	}
	return h
}

// Stream implements Provider.
func (p *OpenAICompletionsProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	body, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	// Fetch-origin timing: ttft/duration must include connect +
	// gateway queue + prefill — the wait the user feels (#283; see the
	// full comment in anthropic.go).
	start := time.Now()
	sctx, cancel := context.WithCancel(ctx)
	resp, err := wirePost(sctx, p.httpClient, p.baseURL+"/chat/completions", p.headers(), body, APIOpenAICompletions)
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

// openaiToolCallState accumulates one streamed tool call by delta index.
type openaiToolCallState struct {
	id    string
	name  string
	index int
	args  strings.Builder
}

func (p *OpenAICompletionsProvider) stream(ctx context.Context, r io.Reader, model string, ch chan<- Event, start time.Time) {
	var (
		emitted   bool
		inText    bool
		inThink   bool
		response  string
		stop      StopReason
		sawStop   bool
		usage     *Usage
		text      strings.Builder
		thinkBuf  strings.Builder
		ttft      int64
		toolCalls = map[int]*openaiToolCallState{}
		order     []int
		msg       = Message{Role: RoleAssistant}
	)
	emit := func(ev Event) {
		if !emitted {
			emitted = true
			ch <- Event{Type: EventStart, Provider: p.name, API: APIOpenAICompletions, Model: model}
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
			msg.Content = append(msg.Content, ThinkingBlock{Thinking: thinkBuf.String(), ThinkingSignature: "reasoning_content"})
			emit(Event{Type: EventThinkingEnd})
		}
	}
	finish := func() {
		if !sawStop {
			fail(Errorf(errors.New("openai-completions: stream ended without finish_reason")))
			return
		}
		if usage == nil {
			usage = &Usage{}
		}
		closeThink()
		closeText()
		for _, idx := range order {
			st := toolCalls[idx]
			args := parseToolArgs(APIOpenAICompletions, st.id, st.args.String())
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
		msg.Provider, msg.API, msg.Model = p.name, APIOpenAICompletions, model
		msg.ResponseID, msg.StopReason, msg.Usage = response, stop, usage
		msg.DurationMS, msg.TTFTMS = time.Since(start).Milliseconds(), ttft
		emit(Donef(stop, usage, &msg))
	}
	reader := sse.NewReader(r)
	for {
		frame, err := reader.Next(ctx)
		eof := err != nil && errors.Is(err, io.EOF)
		if err != nil && !eof {
			if ctx.Err() != nil {
				fail(Errorf(contextErr(ctx)))
				return
			}
			fail(Errorf(fmt.Errorf("openai-completions: read stream: %w", err)))
			return
		}
		// [DONE] ends the stream. A final partial frame may arrive alongside
		// EOF; with no payload there is nothing left to dispatch.
		if frame.IsDone() || eof {
			finish()
			return
		}
		if frame.Data == "" {
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					Message          *struct {
						ReasoningContent string `json:"reasoning_content"`
						Reasoning        string `json:"reasoning"`
					} `json:"message"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int64 `json:"prompt_tokens"`
				CompletionTokens    int64 `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CompletionTokensDetails *struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &chunk); err != nil {
			fail(Errorf(malformedStream("openai-completions", "decode chunk", err)))
			return
		}
		if response == "" {
			response = chunk.ID
		}
		for _, choice := range chunk.Choices {
			d := choice.Delta
			think := d.ReasoningContent
			if think == "" {
				think = d.Reasoning
			}
			if think == "" && d.Message != nil {
				// Some providers put reasoning inside delta.message.
				think = d.Message.ReasoningContent
				if think == "" {
					think = d.Message.Reasoning
				}
			}
			if think != "" {
				if !inThink {
					closeText()
					inThink = true
					emit(Event{Type: EventThinkingStart})
				}
				thinkBuf.WriteString(think)
				emit(Event{Type: EventThinkingDelta, Delta: think})
			}
			if d.Content != "" {
				if !inText {
					closeThink()
					inText = true
					emit(Event{Type: EventTextStart})
				}
				text.WriteString(d.Content)
				emit(Event{Type: EventTextDelta, Delta: d.Content, Snapshot: text.String()})
			}
			for _, tc := range d.ToolCalls {
				st := toolCalls[tc.Index]
				if st == nil {
					closeThink()
					closeText()
					st = &openaiToolCallState{index: tc.Index, id: tc.ID, name: tc.Function.Name}
					toolCalls[tc.Index] = st
					order = append(order, tc.Index)
					emit(Event{Type: EventToolcallStart, ToolCallID: st.id, ToolName: st.name, StreamIndex: st.index})
				} else if tc.ID != "" {
					st.id = tc.ID
				}
				if tc.Function.Name != "" && st.name == "" {
					st.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					st.args.WriteString(tc.Function.Arguments)
					emit(Event{Type: EventToolcallDelta, ToolCallID: st.id, ToolName: st.name, StreamIndex: st.index, PartialJSON: st.args.String()})
				}
			}
			if choice.FinishReason != "" {
				sawStop = true
				closeThink()
				closeText()
				switch choice.FinishReason {
				case "length":
					stop = StopReasonLength
				default:
					// stop, tool_calls, and anything unknown continue the turn.
					stop = StopReasonStop
				}
			}
		}
		if chunk.Usage != nil {
			u := &Usage{
				Output: chunk.Usage.CompletionTokens,
			}
			if chunk.Usage.CompletionTokensDetails != nil {
				u.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
			}
			// The wire's prompt_tokens includes cached tokens; the unified
			// Usage keeps Input exclusive of CacheRead (omp invariant:
			// input + output + cacheRead = totalTokens).
			if chunk.Usage.PromptTokensDetails != nil {
				u.CacheRead = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			u.Input = chunk.Usage.PromptTokens - u.CacheRead
			if u.Input < 0 {
				u.Input = 0
			}
			u.TotalTokens = u.Input + u.Output + u.CacheRead
			usage = u
		}
	}
}
