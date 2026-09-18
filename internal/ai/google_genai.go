package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai/sse"
)

// GoogleGenAIProvider speaks the google-generative-ai wire protocol:
// POST {baseURL}/models/{model}:streamGenerateContent?alt=sse with
// candidates[].content.parts[] chunks (text, thought, functionCall).
// It completes the v1 transport catalog (M9 #10).
type GoogleGenAIProvider struct {
	httpClient   *http.Client
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	model        string // default model when a request omits Model
	name         string
	// apiLabel overrides the wire name reported by API() (google-vertex and
	// gemini-cli reuse this adapter's request/SSE mapping).
	apiLabel string
	// normalizeTools runs NormalizeSchemaForGoogle over the tool schemas.
	normalizeTools bool
	// unwrapResponse reads the GenerateContentResponse from the Code Assist
	// "response" envelope instead of the bare chunk.
	unwrapResponse bool
}

// HealthCheck implements ai.HealthChecker.
func (p *GoogleGenAIProvider) HealthCheck(ctx context.Context) error {
	return healthCheckOneGet(ctx, p.httpClient, p.baseURL+"/v1/models", nil, APIGoogleGenerativeAI)
}

// NewGoogleGenAIProvider builds a provider. A nil hc uses the shared
// transport.
func NewGoogleGenAIProvider(name, baseURL, apiKey string, headers map[string]string, hc *http.Client) *GoogleGenAIProvider {
	if hc == nil {
		hc = wireHTTPClient
	}
	return &GoogleGenAIProvider{
		httpClient: hc, baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey: apiKey, extraHeaders: headers, name: name,
	}
}

func (p *GoogleGenAIProvider) Name() string { return p.name }
func (p *GoogleGenAIProvider) API() string {
	if p.apiLabel != "" {
		return p.apiLabel
	}
	return APIGoogleGenerativeAI
}

// --- request wire shapes ---

type googlePart struct {
	Text           string                 `json:"text,omitempty"`
	Thought        bool                   `json:"thought,omitempty"`
	ThoughtSig     json.RawMessage        `json:"thoughtSignature,omitempty"`
	FunctionCall   *googleCall            `json:"functionCall,omitempty"`
	FunctionResult *googleFunctionRequest `json:"functionResponse,omitempty"`
}

type googleCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
	// ThoughtSig must be replayed with the call once thinking is enabled;
	// dropping it makes the next request invalid.
	ThoughtSig json.RawMessage `json:"thoughtSignature,omitempty"`
}

// googleFunctionRequest carries a tool result back to the model: Gemini has
// no tool role, so the result rides a user turn keyed by the call's name.
type googleFunctionRequest struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type googleContent struct {
	Role  string       `json:"role,omitempty"` // user | model (tool → user)
	Parts []googlePart `json:"parts"`
}

type googleSystemInstruction struct {
	Parts []googlePart `json:"parts"`
}

type googleToolDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type googleToolConfig struct {
	Mode string `json:"functionCallingMode,omitempty"` // AUTO
}

type googleRequest struct {
	Contents          []googleContent          `json:"contents"`
	SystemInstruction *googleSystemInstruction `json:"systemInstruction,omitempty"`
	Tools             []googleToolDeclaration  `json:"tools,omitempty"`
	ToolConfig        *googleToolConfig        `json:"toolConfig,omitempty"`
	GenerationConfig  *googleGeneration        `json:"generationConfig,omitempty"`
}

type googleGeneration struct {
	MaxOutputTokens int            `json:"maxOutputTokens,omitempty"`
	Temperature     float64        `json:"temperature,omitempty"`
	ThinkingConfig  map[string]any `json:"thinkingConfig,omitempty"`
}

// --- response wire shapes ---

type googleResponse struct {
	Candidates []struct {
		Content      *googleContent `json:"content"`
		FinishReason string         `json:"finishReason,omitempty"`
		Index        int            `json:"index,omitempty"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
		ThoughtsTokenCount   int64 `json:"thoughtsTokenCount,omitempty"`
	} `json:"usageMetadata,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// buildRequest converts a unified request to the Gemini shape. Roles map:
// assistant→model, toolResult→user with a functionResponse part (the API
// has no tool role), and system text → systemInstruction.
func (p *GoogleGenAIProvider) buildRequest(req StreamRequest) ([]byte, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return nil, fmt.Errorf("%s: model is required", p.API())
	}
	g := googleRequest{}
	if req.System != "" {
		g.SystemInstruction = &googleSystemInstruction{Parts: []googlePart{{Text: req.System}}}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			parts := make([]googlePart, 0, len(m.Content))
			for _, b := range m.Content {
				if tb, ok := b.(TextBlock); ok {
					parts = append(parts, googlePart{Text: tb.Text})
				}
			}
			if len(parts) == 0 {
				continue
			}
			g.Contents = append(g.Contents, googleContent{Role: "user", Parts: parts})
		case RoleToolResult:
			g.Contents = append(g.Contents, googleContent{Role: "user", Parts: []googlePart{{
				FunctionResult: &googleFunctionRequest{
					Name:     m.ToolName,
					Response: map[string]any{"output": toolResultText(m)},
				},
			}}})
		case RoleAssistant:
			parts := make([]googlePart, 0, len(m.Content))
			for _, b := range m.Content {
				switch blk := b.(type) {
				case TextBlock:
					if blk.Text != "" {
						parts = append(parts, googlePart{Text: blk.Text})
					}
				case ThinkingBlock:
					if blk.Thinking != "" {
						parts = append(parts, googlePart{Text: blk.Thinking, Thought: true})
					}
				case ToolCallBlock:
					// Same guard as the other three encoders: an unparseable stored
					// blob becomes {} rather than an encoder error that would take the
					// whole request (and every tool in it) down with it.
					args := map[string]any{}
					if obj, aerr := decodeArgsObject(parseToolArgs(p.API(), blk.ID, string(blk.Arguments))); aerr == nil {
						args = obj
					}
					parts = append(parts, googlePart{
						ThoughtSig:   blk.Signature,
						FunctionCall: &googleCall{ID: blk.ID, Name: blk.Name, Args: args, ThoughtSig: blk.Signature},
					})
				}
			}
			if len(parts) == 0 {
				continue
			}
			g.Contents = append(g.Contents, googleContent{Role: "model", Parts: parts})
		}
	}
	tools := req.Tools
	if p.normalizeTools {
		tools = NormalizeToolsForAPI(p.API(), tools)
	}
	if len(tools) > 0 {
		decls := make([]googleToolDeclaration, 0, len(tools))
		for _, t := range tools {
			decls = append(decls, googleToolDeclaration{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
		}
		g.Tools = decls
		g.ToolConfig = &googleToolConfig{Mode: "AUTO"}
	}
	if req.MaxTokens > 0 || req.Thinking != nil {
		gen := &googleGeneration{}
		if req.MaxTokens > 0 {
			gen.MaxOutputTokens = req.MaxTokens
		}
		if req.Thinking != nil && req.Thinking.Tokens > 0 {
			gen.ThinkingConfig = map[string]any{"thinkingBudget": req.Thinking.Tokens}
		}
		g.GenerationConfig = gen
	}
	return json.Marshal(g)
}

// toolResultText flattens a toolResult message into the string the
// functionResponse part carries.
func toolResultText(m Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if tb, ok := blk.(TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

func (p *GoogleGenAIProvider) headers() map[string]string {
	h := map[string]string{"Content-Type": "application/json"}
	// Gemini's HTTP API authenticates by query param; x-api-key-style
	// headers still pass through for gateways that expect them.
	for k, v := range p.extraHeaders {
		h[k] = v
	}
	return h
}

// Stream implements Provider.
func (p *GoogleGenAIProvider) Stream(ctx context.Context, req StreamRequest) (<-chan Event, error) {
	model, err := p.resolveModel(req)
	if err != nil {
		return nil, err
	}
	body, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s", p.baseURL, model, p.apiKey)
	return p.streamAt(ctx, req, url, p.headers(), body)
}

// streamAt is the shared request path of the Google family: POST the body to
// the variant's URL and map the SSE chunks onto unified events. The body is a
// parameter because the Code Assist variant wraps it in its own envelope.
func (p *GoogleGenAIProvider) streamAt(ctx context.Context, req StreamRequest, url string, headers map[string]string, body []byte) (<-chan Event, error) {
	model, err := p.resolveModel(req)
	if err != nil {
		return nil, err
	}
	// Fetch-origin timing: ttft/duration must include connect +
	// gateway queue + prefill — the wait the user feels (#283; see the
	// full comment in anthropic.go).
	start := time.Now()
	sctx, cancel := context.WithCancel(ctx)
	resp, err := wirePost(sctx, p.httpClient, url, headers, body, p.API())
	if err != nil {
		cancel()
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

// resolveModel is the shared "request model or provider default" resolution.
func (p *GoogleGenAIProvider) resolveModel(req StreamRequest) (string, error) {
	if req.Model != "" {
		return req.Model, nil
	}
	if p.model != "" {
		return p.model, nil
	}
	return "", fmt.Errorf("%s: model is required", p.API())
}

// stream maps the SSE chunk stream onto the unified event contract:
// start → (thinking|text|toolcall triplets)* → exactly one terminal event.
func (p *GoogleGenAIProvider) stream(ctx context.Context, body io.Reader, model string, ch chan<- Event, start time.Time) {
	var (
		text       strings.Builder
		toolCalls  = map[int]*ToolCallBlock{}
		order      []int
		inText     bool
		inThinking bool
		stop       = StopReasonStop
		usage      *Usage
		emitted    bool
		ttft       int64
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
	fail := func(ev Event) {
		ch <- ev
	}
	closeThinking := func() {
		if inThinking {
			inThinking = false
			emit(Event{Type: EventThinkingEnd})
		}
	}
	closeText := func() {
		if inText {
			inText = false
			emit(Event{Type: EventTextEnd})
		}
	}
	done := func() {
		closeThinking()
		closeText()
		content := make([]Block, 0, len(order)+1)
		if text.Len() > 0 {
			content = append(content, TextBlock{Text: text.String()})
		}
		for _, idx := range order {
			content = append(content, *toolCalls[idx])
		}
		msg := &Message{
			Role: RoleAssistant, Content: content, StopReason: stop, Usage: usage,
			Provider: p.name, API: p.API(), Model: model, TTFTMS: ttft,
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
			DurationMS:  time.Since(start).Milliseconds(),
		}
		emit(Event{Type: EventDone, StopReason: stop, Usage: usage, Message: msg})
	}

	rd := sse.NewReader(body)
	for {
		frame, err := rd.Next(ctx)
		if err == io.EOF {
			done()
			return
		}
		if err != nil {
			fail(Event{Type: EventError, Err: fmt.Errorf("google-generative-ai: %w", err)})
			return
		}
		data := strings.TrimSpace(frame.Data)
		if data == "" || data == "[]" {
			continue
		}
		if p.unwrapResponse {
			data = unwrapCodeAssistChunk(data)
		}
		var gr googleResponse
		if err := json.Unmarshal([]byte(data), &gr); err != nil {
			fail(Event{Type: EventError, Err: malformedStream(p.API(), "bad chunk", err)})
			return
		}
		if gr.Error != nil && gr.Error.Message != "" {
			fail(Event{Type: EventError, Err: &HTTPError{API: p.API(), Status: http.StatusBadRequest, Body: gr.Error.Message}})
			return
		}
		if gr.UsageMetadata != nil {
			usage = &Usage{
				Input:           gr.UsageMetadata.PromptTokenCount,
				Output:          gr.UsageMetadata.CandidatesTokenCount,
				TotalTokens:     gr.UsageMetadata.TotalTokenCount,
				ReasoningTokens: gr.UsageMetadata.ThoughtsTokenCount,
			}
		}
		if len(gr.Candidates) == 0 {
			continue
		}
		cand := gr.Candidates[0]
		if cand.Content != nil {
			for _, part := range cand.Content.Parts {
				switch {
				case part.FunctionCall != nil:
					closeThinking()
					closeText()
					args, _ := json.Marshal(part.FunctionCall.Args)
					idx := len(order)
					// An id links a multi-chunk call to its first index.
					if part.FunctionCall.ID != "" {
						for _, existing := range order {
							if toolCalls[existing].ID == part.FunctionCall.ID {
								idx = existing
								break
							}
						}
					}
					if _, seen := toolCalls[idx]; seen {
						continue
					}
					toolCalls[idx] = &ToolCallBlock{
						ID: part.FunctionCall.ID, Name: part.FunctionCall.Name,
						Arguments: args, StreamIndex: idx, Signature: part.ThoughtSig,
					}
					order = append(order, idx)
					emit(Event{Type: EventToolcallStart, ToolCallID: part.FunctionCall.ID, ToolName: part.FunctionCall.Name, StreamIndex: idx})
					emit(Event{Type: EventToolcallDelta, ToolCallID: part.FunctionCall.ID, StreamIndex: idx, PartialJSON: string(args)})
					emit(Event{Type: EventToolcallEnd, ToolCallID: part.FunctionCall.ID, StreamIndex: idx, PartialJSON: string(args)})

				case part.Text != "":
					if part.Thought {
						closeText()
						if !inThinking {
							inThinking = true
							emit(Event{Type: EventThinkingStart})
						}
						emit(Event{Type: EventThinkingDelta, Delta: part.Text})
						continue
					}
					closeThinking()
					if !inText {
						inText = true
						emit(Event{Type: EventTextStart})
					}
					text.WriteString(part.Text)
					emit(Event{Type: EventTextDelta, Delta: part.Text, Snapshot: text.String()})
				}
			}
		}
		switch strings.ToUpper(cand.FinishReason) {
		case "MAX_TOKENS":
			stop = StopReasonLength
		case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
			stop = StopReasonAborted
		}
	}
}
