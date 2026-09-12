package tool

import (
	"context"
	"encoding/json"
	"strings"
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
