package tool

// AskTool: structured mid-task clarification (M11 #36, omp `ask`). The
// model asks a question with labelled options; a host-provided AskSink
// answers it (the TUI overlay card is #46's scope). Without a sink —
// headless print runs — nobody answers, so the default policy waits out
// ask.timeout and lets the recommended option(s) proceed.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

var _ Tool = (*AskTool)(nil)

// DefaultAskTimeout is the headless wait when ask.timeout is unset.
const DefaultAskTimeout = 2 * time.Minute

// AskToolName is the registry name of the ask tool.
const AskToolName = "ask"

// AskOption is one answer choice.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskRequest is one clarification.
type AskRequest struct {
	Question    string      `json:"question"`
	Options     []AskOption `json:"options"`
	Multi       bool        `json:"multi,omitempty"`
	Recommended []string    `json:"recommended,omitempty"` // labels used when nobody answers
}

// AskResponse carries the chosen labels (one unless Multi).
type AskResponse struct {
	Labels []string `json:"selected"`
}

// AskSink answers a question. Sinks should respect ctx cancellation (Esc
// aborts the turn); returning an error or zero labels means "no answer",
// and the tool then falls through to the headless guidance text.
type AskSink interface {
	Ask(ctx context.Context, req AskRequest) (AskResponse, error)
}

// AskTool asks the model's question through the configured sink.
type AskTool struct {
	// Sink answers the question; nil selects the headless default
	// (wait out Timeout, then the recommended options).
	Sink AskSink
	// Timeout caps the headless wait; <=0 means DefaultAskTimeout.
	// The ask.timeout setting feeds this from cmd.
	Timeout time.Duration
}

// NewAskTool returns an ask tool with the headless default policy.
func NewAskTool(timeout time.Duration) *AskTool { return &AskTool{Timeout: timeout} }

func (t *AskTool) Name() string { return AskToolName }

func (t *AskTool) Description() string {
	return "ask the user a clarifying question with labelled options; unattended runs use the recommended option after a timeout"
}

func (t *AskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "the clarifying question, phrased so one of the options answers it"},
    "options": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "label": {"type": "string", "description": "short answer choice"},
          "description": {"type": "string", "description": "what choosing it means"}
        },
        "required": ["label"]
      }
    },
    "multi": {"type": "boolean", "description": "true = several options may be selected"},
    "recommended": {"description": "option label(s) to use when nobody answers within the timeout; string or array of labels"}
  },
  "required": ["question", "options"]
}`)
}

func (t *AskTool) timeout() time.Duration {
	if t == nil || t.Timeout <= 0 {
		return DefaultAskTimeout
	}
	return t.Timeout
}

func (t *AskTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Question    string          `json:"question"`
		Options     []AskOption     `json:"options"`
		Multi       bool            `json:"multi"`
		Recommended json.RawMessage `json:"recommended"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("ask: invalid arguments: %w", err)
	}
	req := AskRequest{Question: strings.TrimSpace(a.Question), Options: a.Options, Multi: a.Multi}
	if req.Question == "" {
		return Result{IsError: true, Text: "ask: question is required"}, nil
	}
	if len(req.Options) == 0 {
		return Result{IsError: true, Text: "ask: at least one option is required"}, nil
	}
	labels := map[string]bool{}
	for _, o := range req.Options {
		if strings.TrimSpace(o.Label) == "" {
			return Result{IsError: true, Text: "ask: every option needs a label"}, nil
		}
		if labels[o.Label] {
			return Result{IsError: true, Text: "ask: duplicate option label " + o.Label}, nil
		}
		labels[o.Label] = true
	}
	if len(a.Recommended) > 0 {
		var one string
		if err := json.Unmarshal(a.Recommended, &one); err == nil {
			req.Recommended = []string{one}
		} else if err := json.Unmarshal(a.Recommended, &req.Recommended); err != nil {
			return Result{IsError: true, Text: "ask: recommended must be a label or a list of labels"}, nil
		}
	}
	for _, l := range req.Recommended {
		if !labels[l] {
			return Result{IsError: true, Text: "ask: recommended option " + l + " is not in options"}, nil
		}
	}

	// Headless default: nobody answers, so wait out the timeout and let
	// the recommended option(s) proceed.
	sink := t.Sink
	if sink == nil {
		sink = headlessAskSink{timeout: t.timeout()}
	}
	resp, err := sink.Ask(ctx, req)
	if ctx.Err() != nil {
		return Result{IsError: true, Text: "ask: canceled: " + ctx.Err().Error()}, nil
	}
	if err != nil || len(resp.Labels) == 0 {
		return Result{Text: fmt.Sprintf("no answer within %s — proceed with your best judgment and state the assumption", t.timeout().Round(time.Second))}, nil
	}
	for _, l := range resp.Labels {
		if !labels[l] {
			return Result{IsError: true, Text: "ask: answer " + l + " is not one of the options"}, nil
		}
	}
	if !req.Multi && len(resp.Labels) > 1 {
		return Result{IsError: true, Text: fmt.Sprintf("ask: got %d answers for a single-select question", len(resp.Labels))}, nil
	}
	out, _ := json.Marshal(map[string]any{"selected": resp.Labels})
	return Result{Text: string(out), Details: map[string]any{"question": req.Question, "selected": resp.Labels}}, nil
}

// headlessAskSink is the default policy: an unattended run has nobody to
// answer, so it waits out the timeout and returns the recommended
// option(s) — or nothing, when none were recommended.
type headlessAskSink struct{ timeout time.Duration }

// NewHeadlessAskSink returns the timeout→recommended policy sink; the
// TUI fallback sink delegates to it after surfacing the question.
func NewHeadlessAskSink(timeout time.Duration) AskSink {
	return headlessAskSink{timeout: timeout}
}

func (s headlessAskSink) Ask(ctx context.Context, req AskRequest) (AskResponse, error) {
	t := s.timeout
	if t <= 0 {
		t = DefaultAskTimeout
	}
	timer := time.NewTimer(t)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return AskResponse{}, ctx.Err()
	case <-timer.C:
	}
	return AskResponse{Labels: append([]string(nil), req.Recommended...)}, nil
}
