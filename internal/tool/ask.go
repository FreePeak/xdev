package tool

// AskTool: structured mid-task clarification (M11 #36, omp `ask`). The
// model asks a question with labelled options; a host-provided AskSink
// answers it (the TUI overlay card is #46's scope). Without a sink —
// headless print runs — nobody answers, so the default policy waits out
// ask.timeout and lets the recommended option(s) proceed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var _ Tool = (*AskTool)(nil)

// DefaultAskTimeout is the headless wait when ask.timeout is unset. Thirty
// seconds, not minutes: a one-shot run that hits an ask must not stall CI
// for two minutes per question — the wait exists so a barely-late human can
// still answer, and ask.timeout raises it deliberately.
const DefaultAskTimeout = 30 * time.Second

// AskToolName is the registry name of the ask tool.
const AskToolName = "ask"

// AskOption is one answer choice.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskRequest is one clarification.
type AskRequest struct {
	// ID names the question in a batch answer (omp's questions[].id); a
	// single question gets a positional id so the report stays addressable.
	ID          string      `json:"id,omitempty"`
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
    "recommended": {"description": "option(s) to use when nobody answers within the headless wait: a label, a 0-based option index, or a list of either"},
    "questions": {
      "type": "array",
      "description": "batch form (omp): ask several questions in one call; each entry carries id/question/options/multi/recommended",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "description": "names this question in the answer"},
          "question": {"type": "string"},
          "options": {"type": "array", "items": {"type": "object", "properties": {"label": {"type": "string"}, "description": {"type": "string"}, "preview": {"type": "string"}}, "required": ["label"]}},
          "multi": {"type": "boolean"},
          "recommended": {"description": "0-based option index or label"}
        },
        "required": ["question", "options"]
      }
    },
    "_note": {"description": "unattended (print/rpc) runs wait ask.timeout (default 30s) then take the recommended option — in a one-shot run, prefer deciding over asking"}
  },
  "anyOf": [
    {"required": ["question", "options"]},
    {"required": ["questions"]}
  ]
}`)
}

func (t *AskTool) timeout() time.Duration {
	if t == nil || t.Timeout <= 0 {
		return DefaultAskTimeout
	}
	return t.Timeout
}

// askQuestion is one question in either accepted shape: xdev's flat single
// form and omp's batch form (`{questions:[{id,question,options,multi,
// recommended}]}`, where `recommended` is a zero-based INDEX). A model
// following the baseline's tool docs sends the batch shape, and rejecting it
// with "question is required" made the tool look broken (#94).
type askQuestion struct {
	ID          string          `json:"id"`
	Question    string          `json:"question"`
	Options     []AskOption     `json:"options"`
	Multi       bool            `json:"multi"`
	Recommended json.RawMessage `json:"recommended"`
}

func (t *AskTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		askQuestion
		Questions []askQuestion `json:"questions"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("ask: invalid arguments: %w", err)
	}
	items := a.Questions
	if len(items) == 0 {
		items = []askQuestion{a.askQuestion}
	}
	// One sink for the whole call: the headless wait and the TUI card are the
	// same machinery per question.
	sink := t.Sink
	if sink == nil {
		sink = headlessAskSink{timeout: t.timeout()}
	}
	type answered struct {
		ID       string   `json:"id,omitempty"`
		Selected []string `json:"selected"`
	}
	results := make([]answered, 0, len(items))
	for i, q := range items {
		req, fail := normalizeAsk(q, i)
		if fail != nil {
			return *fail, nil
		}
		resp, err := sink.Ask(ctx, req)
		if ctx.Err() != nil {
			return Result{IsError: true, Text: "ask: canceled: " + ctx.Err().Error()}, nil
		}
		if err != nil || len(resp.Labels) == 0 {
			return Result{Text: fmt.Sprintf("no answer within %s — proceed with your best judgment and state the assumption", t.timeout().Round(time.Second))}, nil
		}
		legal := map[string]bool{}
		for _, o := range req.Options {
			legal[o.Label] = true
		}
		for _, l := range resp.Labels {
			if !legal[l] {
				return Result{IsError: true, Text: "ask: answer " + l + " is not one of the options"}, nil
			}
		}
		if !req.Multi && len(resp.Labels) > 1 {
			return Result{IsError: true, Text: fmt.Sprintf("ask: got %d answers for a single-select question", len(resp.Labels))}, nil
		}
		results = append(results, answered{ID: req.ID, Selected: resp.Labels})
	}
	// A single question keeps the flat result shape it always had; a batch
	// answers per id.
	out, _ := json.Marshal(map[string]any{"selected": results[0].Selected})
	if len(results) > 1 {
		out, _ = json.Marshal(map[string]any{"answers": results})
	}
	return Result{Text: string(out), Details: map[string]any{"questions": len(results)}}, nil
}

// normalizeAsk validates one question and resolves its recommendation, which
// arrives as a label (xdev) or a zero-based option index (omp).
func normalizeAsk(q askQuestion, i int) (AskRequest, *Result) {
	bad := func(msg string) (AskRequest, *Result) {
		r := Result{IsError: true, Text: "ask: " + msg}
		return AskRequest{}, &r
	}
	req := AskRequest{ID: strings.TrimSpace(q.ID), Question: strings.TrimSpace(q.Question), Options: q.Options, Multi: q.Multi}
	if req.ID == "" {
		req.ID = strconv.Itoa(i + 1)
	}
	if req.Question == "" {
		return bad("question is required")
	}
	if len(req.Options) == 0 {
		return bad("at least one option is required")
	}
	labels := map[string]bool{}
	for _, o := range req.Options {
		if strings.TrimSpace(o.Label) == "" {
			return bad("every option needs a label")
		}
		if labels[o.Label] {
			return bad("duplicate option label " + o.Label)
		}
		labels[o.Label] = true
	}
	if len(q.Recommended) == 0 {
		return req, nil
	}
	rec, err := resolveRecommended(q.Recommended, req.Options)
	if err != nil {
		return bad(err.Error())
	}
	req.Recommended = rec

	for _, l := range req.Recommended {
		if !labels[l] {
			return bad("recommended option " + l + " is not in options")
		}
	}
	return req, nil
}

// resolveRecommended turns a `recommended` value into option labels. It
// accepts every spelling either baseline uses: a bare label ("B"), a bare
// 0-based index (1), a list of labels (["A","B"]) or a list of indices
// ([0,1]). A quoted "1" is a label, never an index — which is why the JSON
// token's first byte decides, not a decode attempt.
func resolveRecommended(raw json.RawMessage, options []AskOption) ([]string, error) {
	rec := bytes.TrimSpace(raw)
	if len(rec) == 0 {
		return nil, nil
	}
	labelAt := func(n int) (string, error) {
		if n < 0 || n >= len(options) {
			return "", fmt.Errorf("recommended index %d is out of range (0..%d)", n, len(options)-1)
		}
		return options[n].Label, nil
	}
	switch first := rec[0]; {
	case first == '"':
		var one string
		if err := json.Unmarshal(rec, &one); err != nil {
			return nil, errors.New("recommended must be a label, an index, or a list of either")
		}
		return []string{one}, nil
	case first >= '0' && first <= '9':
		label, err := labelAt(intFromJSON(rec))
		if err != nil {
			return nil, err
		}
		return []string{label}, nil
	case first == '[':
		var idx []int
		if err := json.Unmarshal(rec, &idx); err == nil {
			out := make([]string, 0, len(idx))
			for _, n := range idx {
				label, err := labelAt(n)
				if err != nil {
					return nil, err
				}
				out = append(out, label)
			}
			return out, nil
		}
		var list []string
		if err := json.Unmarshal(rec, &list); err == nil {
			return list, nil
		}
		return nil, errors.New("recommended must be a label, an index, or a list of either")
	default:
		return nil, errors.New("recommended must be a label, an index, or a list of either")
	}
}

// intFromJSON decodes a JSON number, returning -1 when it is not one (a
// float index is not a thing, and -1 is out of range for any real list).
func intFromJSON(raw json.RawMessage) int {
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return -1
	}
	return n
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
