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

// AskResponse carries the chosen labels (one unless Multi). Note is prose the
// human typed instead of picking — or AskChatNote, the card's escape hatch
// meaning "let me answer in the next turn" — and a response with neither labels
// nor note is "no answer".
type AskResponse struct {
	Labels []string `json:"selected"`
	Note   string   `json:"note,omitempty"`
}

// AskChatNote is the TUI card's escape hatch arriving as a note rather than an
// option: the human declined the options and will speak in chat, so the model
// must end its turn instead of picking for itself.
const AskChatNote = "Chat about this"

// AskSink answers a question. Sinks should respect ctx cancellation (Esc
// aborts the turn); returning an error or zero labels means "no answer",
// and the tool then falls through to the headless guidance text.
type AskSink interface {
	Ask(ctx context.Context, req AskRequest) (AskResponse, error)
}

// AskBatchSink is the optional seam a host implements when it can ask several
// questions as ONE interruption (the TUI's tabbed card). A sink that only
// implements AskSink is still correct — the tool then asks in sequence — but a
// batch costs one wait instead of one per question, which is the whole point of
// asking them together.
type AskBatchSink interface {
	AskBatch(ctx context.Context, reqs []AskRequest) ([]AskResponse, error)
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
	return "ask the user a clarifying question with labelled options; unattended runs use the recommended option after a timeout. Several questions in one call are ONE interruption (a tabbed card), not a card each — ask them together"
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
	// Validate the whole call before interrupting anyone: a bad question must
	// not cost the human an answer to a good one.
	reqs := make([]AskRequest, 0, len(items))
	for i, q := range items {
		req, fail := normalizeAsk(q, i)
		if fail != nil {
			return *fail, nil
		}
		reqs = append(reqs, req)
	}
	// One sink for the whole call, and one interruption when the sink can take
	// a batch: the wait behind a question is the expensive part, not the card.
	sink := t.Sink
	if sink == nil {
		sink = headlessAskSink{timeout: t.timeout()}
	}
	resps, err := askAll(ctx, sink, reqs)
	if ctx.Err() != nil {
		return Result{IsError: true, Text: "ask: canceled: " + ctx.Err().Error()}, nil
	}
	type answered struct {
		ID       string   `json:"id,omitempty"`
		Selected []string `json:"selected,omitempty"`
		Note     string   `json:"note,omitempty"`
	}
	guidance := fmt.Sprintf("no answer within %s — proceed with your best judgment and state the assumption", t.timeout().Round(time.Second))
	if err != nil || len(resps) != len(reqs) {
		// A sink that broke the one-answer-per-question contract gets reported
		// as no answer at all rather than half an answer mapped to the wrong id.
		return Result{Text: guidance}, nil
	}
	results := make([]answered, 0, len(reqs))
	answeredAny := false // did at least one question get an answer?
	for i, req := range reqs {
		resp := resps[i]
		note := strings.TrimSpace(resp.Note)
		if len(resp.Labels) == 0 && note == "" {
			// This one went unanswered (its recommendation, if any, already
			// arrived through the sink); the others keep their real answers.
			results = append(results, answered{ID: req.ID, Note: guidance})
			continue
		}
		answeredAny = true
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
		results = append(results, answered{ID: req.ID, Selected: resp.Labels, Note: note})
	}
	// Nothing answered at all: the flat guidance text is the whole result, as
	// it has always been for a single question.
	if !answeredAny {
		return Result{Text: guidance}, nil
	}
	// A single question keeps the flat result shape it always had; a batch
	// answers per id.
	single := map[string]any{}
	if len(results[0].Selected) > 0 {
		single["selected"] = results[0].Selected
	}
	if results[0].Note != "" {
		single["note"] = results[0].Note // typed answer; AskChatNote = "they'll say it in chat"
	}
	out, _ := json.Marshal(single)
	if len(results) > 1 {
		out, _ = json.Marshal(map[string]any{"answers": results})
	}
	text := string(out)
	for _, r := range results {
		if r.Note == AskChatNote {
			// The escape hatch only works if the model stops talking, so say so
			// in the result it reads instead of trusting the card's UI hint.
			text += "\nthe human will answer in chat: end this turn now and wait for their next message"
			break
		}
	}
	return Result{Text: text, Details: map[string]any{"questions": len(results)}}, nil
}

// askAll puts one or several questions to the sink, in as few interruptions as
// the sink's shape allows: a batch sink gets the whole list, otherwise each
// question goes through Ask in turn.
func askAll(ctx context.Context, sink AskSink, reqs []AskRequest) ([]AskResponse, error) {
	if len(reqs) == 1 {
		resp, err := sink.Ask(ctx, reqs[0])
		return []AskResponse{resp}, err
	}
	if batch, ok := sink.(AskBatchSink); ok {
		resps, err := batch.AskBatch(ctx, reqs)
		if err == nil && len(resps) != len(reqs) {
			err = fmt.Errorf("ask: batch sink answered %d of %d questions", len(resps), len(reqs))
		}
		return resps, err
	}
	out := make([]AskResponse, 0, len(reqs))
	for _, req := range reqs {
		resp, err := sink.Ask(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, resp)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return out, nil
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

// AskBatch waits ONCE for the whole batch: nobody is answering any of these
// questions, so N sequential waits would only make a one-shot run slower.
func (s headlessAskSink) AskBatch(ctx context.Context, reqs []AskRequest) ([]AskResponse, error) {
	t := s.timeout
	if t <= 0 {
		t = DefaultAskTimeout
	}
	timer := time.NewTimer(t)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	out := make([]AskResponse, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, AskResponse{Labels: append([]string(nil), req.Recommended...)})
	}
	return out, nil
}
