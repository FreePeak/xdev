package main

import (
	"context"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
	"github.com/FreePeak/xdev/internal/tui"
)

// The ask card wiring (#106): the overlay existed with zero callers, so every
// interactive ask ran the headless timeout and answered itself. The sink must
// consult the card FIRST and only fall back when it is skipped.

type recordingOps struct {
	calls int
	req   tui.AskRequest
	ans   tui.AskAnswer
	ok    bool
}

func (r *recordingOps) show(_ context.Context, req tui.AskRequest, _ time.Duration) (tui.AskAnswer, bool) {
	r.calls++
	r.req = req
	return tui.AskAnswer{Labels: r.ans.Labels}, r.ok
}

type staticSink struct{ labels []string }

func (s staticSink) Ask(context.Context, tool.AskRequest) (tool.AskResponse, error) {
	return tool.AskResponse{Labels: s.labels}, nil
}

func TestAskCardSinkPrefersTheCard(t *testing.T) {
	ops := &recordingOps{ans: tui.AskAnswer{Labels: []string{"red"}}, ok: true}
	sink := &askCardSink{ops: &tui.AskOps{Show: ops.show}, fallback: staticSink{labels: []string{"blue"}}}
	got, err := sink.Ask(context.Background(), tool.AskRequest{
		Question: "Which color?", Options: []tool.AskOption{{Label: "red"}, {Label: "blue", Description: "cool"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "red" {
		t.Fatalf("the user's pick was not returned: %+v", got.Labels)
	}
	// The card saw the question and both options with descriptions.
	if ops.req.Question != "Which color?" || len(ops.req.Options) != 2 || ops.req.Options[1].Description != "cool" {
		t.Fatalf("card request mangled: %+v", ops.req)
	}
}

// Skip (or a narrow terminal, which the overlay reports as not-ok) falls back
// to the timeout→recommended policy instead of losing the question.
func TestAskCardSinkFallsBackOnSkip(t *testing.T) {
	ops := &recordingOps{ok: false}
	sink := &askCardSink{ops: &tui.AskOps{Show: ops.show}, fallback: staticSink{labels: []string{"recommended-label"}}}
	got, err := sink.Ask(context.Background(), tool.AskRequest{
		Question: "Q", Options: []tool.AskOption{{Label: "recommended-label"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ops.calls != 1 {
		t.Fatalf("card consulted %d times", ops.calls)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "recommended-label" {
		t.Fatalf("fallback did not answer: %+v", got.Labels)
	}
}

// An unwired card (no ops) keeps the headless path — the tool must never hang
// or vanish because a host did not provide a UI seam.
func TestAskCardSinkWithoutOps(t *testing.T) {
	sink := &askCardSink{fallback: staticSink{labels: []string{"x"}}}
	got, err := sink.Ask(context.Background(), tool.AskRequest{Question: "Q", Options: []tool.AskOption{{Label: "x"}}})
	if err != nil || len(got.Labels) != 1 {
		t.Fatalf("headless fallback broken: %+v %v", got, err)
	}
}
