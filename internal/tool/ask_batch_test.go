package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ask accepts BOTH the flat xdev shape and omp's batch
// ({questions:[{id,question,options,recommended:<index>]}]). A model trained
// on the baseline sends the batch form; rejecting it made the tool look
// broken (#94).

// recSink answers with whatever the request's recommendation is, so the test
// observes how each spelling was resolved.
type recSink struct{ asked []AskRequest }

func (s *recSink) Ask(_ context.Context, req AskRequest) (AskResponse, error) {
	s.asked = append(s.asked, req)
	return AskResponse{Labels: append([]string(nil), req.Recommended...)}, nil
}

func TestAskAcceptsOmpBatchShape(t *testing.T) {
	sink := &recSink{}
	at := &AskTool{Sink: sink, Timeout: time.Second}
	args := json.RawMessage(`{"questions":[
	  {"id":"auth","question":"Which auth?","options":[{"label":"JWT"},{"label":"OAuth2"}],"recommended":1},
	  {"id":"store","question":"Which store?","options":[{"label":"SQLite"},{"label":"Postgres"}],"recommended":[0]}
	]}`)
	res, err := at.Execute(context.Background(), args)
	if err != nil || res.IsError {
		t.Fatalf("batch ask rejected: %+v err=%v", res, err)
	}
	// Index 1 → OAuth2; [0] → SQLite.
	if !strings.Contains(res.Text, `"auth"`) || !strings.Contains(res.Text, "OAuth2") ||
		!strings.Contains(res.Text, `"store"`) || !strings.Contains(res.Text, "SQLite") {
		t.Fatalf("per-question answers missing: %s", res.Text)
	}
	if len(sink.asked) != 2 {
		t.Fatalf("sink saw %d questions, want 2", len(sink.asked))
	}
}

// The flat shape keeps its exact result contract ({"selected":[…]}), so
// existing consumers and the TUI card are unaffected.
func TestAskFlatShapeUnchanged(t *testing.T) {
	sink := &recSink{}
	at := &AskTool{Sink: sink, Timeout: time.Second}
	res, err := at.Execute(context.Background(),
		json.RawMessage(`{"question":"Which?","options":[{"label":"A"},{"label":"B"}],"recommended":"B"}`))
	if err != nil || res.IsError {
		t.Fatalf("flat ask rejected: %+v err=%v", res, err)
	}
	var got struct {
		Selected []string `json:"selected"`
		Answers  any      `json:"answers"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Selected) != 1 || got.Selected[0] != "B" {
		t.Fatalf("flat result shape changed: %s", res.Text)
	}
	if got.Answers != nil {
		t.Fatalf("a single question must not report a batch: %s", res.Text)
	}
}

// A quoted "1" is a LABEL, not an index; an out-of-range index is a loud
// error naming the valid range.
func TestAskRecommendedSpelling(t *testing.T) {
	sink := &recSink{}
	at := &AskTool{Sink: sink, Timeout: time.Second}
	if _, err := at.Execute(context.Background(), json.RawMessage(
		`{"question":"Which?","options":[{"label":"1"},{"label":"2"}],"recommended":"1"}`)); err != nil {
		t.Fatal(err)
	}
	if len(sink.asked) != 1 || len(sink.asked[0].Recommended) != 1 || sink.asked[0].Recommended[0] != "1" {
		t.Fatalf(`quoted "1" must resolve as the label: %+v`, sink.asked[0].Recommended)
	}
	res, _ := at.Execute(context.Background(), json.RawMessage(
		`{"question":"Which?","options":[{"label":"A"}],"recommended":5}`))
	if !res.IsError || !strings.Contains(res.Text, "out of range") {
		t.Fatalf("an out-of-range index must be refused: %+v", res)
	}
}
