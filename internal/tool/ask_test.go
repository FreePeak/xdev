package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Headless default policy: after ask.timeout the recommended options
// answer, and without a recommendation the model is told to proceed.
func TestAskHeadlessTimeoutFallsBackToRecommended(t *testing.T) {
	at := NewAskTool(5 * time.Millisecond)
	args := `{"question":"which db?","options":[{"label":"sqlite"},{"label":"postgres"}],"recommended":"sqlite"}`
	res, err := at.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("ask errored: %q", res.Text)
	}
	var got struct {
		Selected []string `json:"selected"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("answer is not the selected-shape JSON: %q", res.Text)
	}
	if len(got.Selected) != 1 || got.Selected[0] != "sqlite" {
		t.Fatalf("selected = %v, want [sqlite]", got.Selected)
	}

	// No recommendation: no fabricated answer, an explicit proceed note.
	res, _ = at.Execute(context.Background(), json.RawMessage(`{"question":"which db?","options":[{"label":"sqlite"},{"label":"postgres"}]}`))
	if res.IsError || !strings.Contains(res.Text, "no answer") {
		t.Fatalf("no-recommendation result = %q (IsError=%v)", res.Text, res.IsError)
	}
}

// An unattended batch waits ONE timeout, not one per question: a one-shot run
// that asks three questions must not stall CI for three minutes.
func TestAskHeadlessBatchWaitsOnce(t *testing.T) {
	const wait = 100 * time.Millisecond
	at := NewAskTool(wait)
	args := `{"questions":[{"question":"q1","options":[{"label":"a"}],"recommended":"a"},{"question":"q2","options":[{"label":"b"}],"recommended":"b"}]}`
	start := time.Now()
	res, err := at.Execute(context.Background(), json.RawMessage(args))
	elapsed := time.Since(start)
	if err != nil || res.IsError {
		t.Fatalf("headless batch: %q %v", res.Text, err)
	}
	// Sequential waits would need 2xwait; the extra 80% is CI slack.
	if elapsed > wait*3/2 {
		t.Fatalf("batch waited %v for a %v timeout — it waited per question", elapsed, wait)
	}
	if !strings.Contains(res.Text, `"a"`) || !strings.Contains(res.Text, `"b"`) {
		t.Fatalf("both recommendations must answer: %q", res.Text)
	}
}

// A wired sink answers immediately; the multi-select shape keeps every
// chosen label, and a single-select must not accept several.
func TestAskSinkMultiSelectResponseShape(t *testing.T) {
	sink := stubSink{answer: []string{"a", "c"}}
	at := &AskTool{Sink: sink}
	base := `{"question":"pick","multi":true,"options":[{"label":"a"},{"label":"b"},{"label":"c"}]}`

	res, err := at.Execute(context.Background(), json.RawMessage(base))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("multi ask errored: %q", res.Text)
	}
	if res.Text != `{"selected":["a","c"]}` {
		t.Fatalf("multi shape = %q", res.Text)
	}

	// Single-select: two answers from the sink is a contract violation.
	sink.answer = []string{"a", "c"}
	at2 := &AskTool{Sink: sink}
	res, _ = at2.Execute(context.Background(), json.RawMessage(`{"question":"pick","options":[{"label":"a"},{"label":"b"},{"label":"c"}]}`))
	if !res.IsError || !strings.Contains(res.Text, "single-select") {
		t.Fatalf("single-select with 2 answers = %q (IsError=%v)", res.Text, res.IsError)
	}

	// Sink answers outside the option set: rejected, never relayed.
	sink.answer = []string{"zzz"}
	at3 := &AskTool{Sink: sink}
	res, _ = at3.Execute(context.Background(), json.RawMessage(base))
	if !res.IsError || !strings.Contains(res.Text, "zzz") {
		t.Fatalf("unknown label = %q (IsError=%v)", res.Text, res.IsError)
	}
}

// Input validation at the trust boundary: bad recommended refs and
// malformed shapes are rejected before any sink runs.
func TestAskValidatesArguments(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"no question", `{"options":[{"label":"a"}]}`, "question is required"},
		{"no options", `{"question":"q"}`, "at least one option"},
		{"recommended off-list", `{"question":"q","options":[{"label":"a"}],"recommended":"zzz"}`, "not in options"},
		{"recommended bad shape", `{"question":"q","options":[{"label":"a"}],"recommended":42}`, "recommended"},
		{"duplicate labels", `{"question":"q","options":[{"label":"a"},{"label":"a"}]}`, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := NewAskTool(time.Millisecond).Execute(context.Background(), json.RawMessage(tc.args))
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("got %q (IsError=%v), want %q", res.Text, res.IsError, tc.want)
			}
		})
	}
	// A single string recommended is accepted and normalized to a list.
	res, err := NewAskTool(time.Millisecond).Execute(context.Background(),
		json.RawMessage(`{"question":"q","options":[{"label":"a"}],"recommended":"a"}`))
	if err != nil || res.IsError {
		t.Fatalf("string recommended rejected: %q %v", res.Text, err)
	}
}

// A canceled context aborts the headless wait instead of answering.
func TestAskHeadlessRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	at := NewAskTool(time.Hour)
	res, _ := at.Execute(ctx, json.RawMessage(`{"question":"q","options":[{"label":"a"}],"recommended":"a"}`))
	if !res.IsError || !strings.Contains(res.Text, "canceled") {
		t.Fatalf("canceled ask = %q (IsError=%v)", res.Text, res.IsError)
	}
}

type stubSink struct{ answer []string }

func (s stubSink) Ask(context.Context, AskRequest) (AskResponse, error) {
	return AskResponse{Labels: append([]string(nil), s.answer...)}, nil
}

// countingSink is a stubSink without a batch seam: a batch call must still get
// every question answered, one Ask at a time.
type countingSink struct {
	mu    sync.Mutex
	asks  int
	label string
}

func (s *countingSink) Ask(context.Context, AskRequest) (AskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks++
	return AskResponse{Labels: []string{s.label}}, nil
}

// batchSink answers a whole batch in one call and counts the calls.
type batchSink struct {
	mu       sync.Mutex
	asks     int
	batches  int
	calls    int
	noteFor  int  // which question of the batch answers with prose (-1: none)
	skip     bool // the flagged question goes unanswered
	chatFlag bool
}

func (s *batchSink) Ask(ctx context.Context, req AskRequest) (AskResponse, error) {
	resp, err := s.AskBatch(ctx, []AskRequest{req})
	if len(resp) == 0 {
		return AskResponse{}, err
	}
	return resp[0], err
}

func (s *batchSink) AskBatch(_ context.Context, reqs []AskRequest) ([]AskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches++
	s.calls += len(reqs)
	out := make([]AskResponse, 0, len(reqs))
	for i := range reqs {
		switch {
		case s.skip && i == s.noteFor:
			out = append(out, AskResponse{})
		case s.chatFlag && i == s.noteFor:
			out = append(out, AskResponse{Note: AskChatNote})
		case i == s.noteFor:
			out = append(out, AskResponse{Note: "typed answer"})
		default:
			out = append(out, AskResponse{Labels: []string{reqs[i].Options[0].Label}})
		}
	}
	return out, nil
}

// A batch of questions costs one interruption when the sink can take one, and
// the result answers per id.
func TestAskBatchUsesTheBatchSinkOnce(t *testing.T) {
	sink := &batchSink{noteFor: -1} // no prose, no skips
	at := &AskTool{Sink: sink}
	args := `{"questions":[{"id":"db","question":"which db?","options":[{"label":"sqlite"},{"label":"pg"}]},{"id":"cache","question":"cache?","options":[{"label":"yes"},{"label":"no"}]}]}`
	res, err := at.Execute(context.Background(), json.RawMessage(args))
	if err != nil || res.IsError {
		t.Fatalf("batch ask: %q %v", res.Text, err)
	}
	if sink.batches != 1 || sink.calls != 2 {
		t.Fatalf("sink saw %d batches / %d questions; want one batch of two", sink.batches, sink.calls)
	}
	var got struct {
		Answers []struct {
			ID       string   `json:"id"`
			Selected []string `json:"selected"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("batch shape: %v (%q)", err, res.Text)
	}
	if len(got.Answers) != 2 || got.Answers[0].ID != "db" || got.Answers[0].Selected[0] != "sqlite" || got.Answers[1].ID != "cache" {
		t.Fatalf("batch answers = %s", res.Text)
	}
}

// In a batch, one skipped question must not erase the answers to the others:
// the empty slot carries the guidance text, the rest stay real answers.
func TestAskBatchKeepsAnswersAroundASkip(t *testing.T) {
	sink := &batchSink{noteFor: 0, skip: true}
	at := &AskTool{Sink: sink}
	args := `{"questions":[{"question":"q1","options":[{"label":"a"}]},{"question":"q2","options":[{"label":"a"}]}]}`
	res, err := at.Execute(context.Background(), json.RawMessage(args))
	if err != nil || res.IsError {
		t.Fatalf("partial batch: %q %v", res.Text, err)
	}
	var got struct {
		Answers []struct {
			ID       string   `json:"id"`
			Selected []string `json:"selected"`
			Note     string   `json:"note"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("partial batch shape: %v (%q)", err, res.Text)
	}
	if len(got.Answers) != 2 || !strings.Contains(got.Answers[0].Note, "no answer") {
		t.Fatalf("skipped question lost its guidance: %s", res.Text)
	}
	if got.Answers[1].Selected[0] != "a" {
		t.Fatalf("answered question lost: %s", res.Text)
	}
}

// A sink that cannot batch still answers every question, in sequence.
func TestAskBatchFallsBackToSequentialAsks(t *testing.T) {
	sink := &countingSink{label: "a"}
	at := &AskTool{Sink: sink}
	args := `{"questions":[{"question":"q1","options":[{"label":"a"}]},{"question":"q2","options":[{"label":"a"}]}]}`
	res, err := at.Execute(context.Background(), json.RawMessage(args))
	if err != nil || res.IsError {
		t.Fatalf("sequential batch: %q %v", res.Text, err)
	}
	if sink.asks != 2 {
		t.Fatalf("asks = %d, want 2", sink.asks)
	}
}

// A typed answer is an answer: it reaches the model as note text, and no option
// validation rejects it.
func TestAskTypedNoteAnswers(t *testing.T) {
	sink := &batchSink{noteFor: 0}
	res, err := (&AskTool{Sink: sink}).Execute(context.Background(), json.RawMessage(`{"question":"which?","options":[{"label":"a"},{"label":"b"}]}`))
	if err != nil || res.IsError {
		t.Fatalf("typed ask: %q %v", res.Text, err)
	}
	if !strings.Contains(res.Text, `"note":"typed answer"`) || strings.Contains(res.Text, "selected") {
		t.Fatalf("typed answer shape = %q", res.Text)
	}
}

// The card's escape hatch must stop the model, not invite it to pick for
// itself: the result says to end the turn.
func TestAskChatNoteEndsTheTurn(t *testing.T) {
	sink := &batchSink{noteFor: 0, chatFlag: true}
	res, err := (&AskTool{Sink: sink}).Execute(context.Background(), json.RawMessage(`{"question":"which?","options":[{"label":"a"}],"recommended":"a"}`))
	if err != nil || res.IsError {
		t.Fatalf("chat ask: %q %v", res.Text, err)
	}
	if !strings.Contains(res.Text, AskChatNote) || !strings.Contains(res.Text, "end this turn") {
		t.Fatalf("chat answer must tell the model to stop: %q", res.Text)
	}
	if strings.Contains(res.Text, "recommended") {
		t.Fatalf("a chat answer must not smuggle in the recommendation: %q", res.Text)
	}
}

// TestAskParametersRootUnionIsObjectTyped pins the schema the provider
// sees: a root anyOf with untyped branches is the 400
// "ask: tool parameter root must be an object type". Both shapes the
// tool accepts (flat question+options, batch questions) stay required.
func TestAskParametersRootUnionIsObjectTyped(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal((&AskTool{}).Parameters(), &schema); err != nil {
		t.Fatalf("parameters: %v", err)
	}
	branches, _ := schema["anyOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("anyOf = %v, want both call shapes", schema["anyOf"])
	}
	var sawQuestion, sawQuestions bool
	for i, b := range branches {
		obj, _ := b.(map[string]any)
		if obj["type"] != "object" {
			t.Fatalf("branch %d = %v, want type object", i, b)
		}
		req, _ := obj["required"].([]any)
		joined := fmt.Sprint(req)
		if strings.Contains(joined, "question") && strings.Contains(joined, "options") {
			sawQuestion = true
		}
		if strings.Contains(joined, "questions") {
			sawQuestions = true
		}
	}
	if !sawQuestion || !sawQuestions {
		t.Fatalf("required-alternatives lost: %v", branches)
	}
}
