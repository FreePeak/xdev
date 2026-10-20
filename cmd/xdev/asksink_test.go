package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
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
	return tui.AskAnswer{Labels: r.ans.Labels, Note: r.ans.Note}, r.ok
}

// recordingBatchOps stands for the tabbed card: one ShowBatch call answers a
// whole batch, and the sink must not spend a wait per question.
type recordingBatchOps struct {
	batchCalls int
	showCalls  int
	reqs       []tui.AskRequest
	ans        []tui.AskAnswer
	ok         bool
}

func (r *recordingBatchOps) showBatch(_ context.Context, reqs []tui.AskRequest, _ time.Duration) ([]tui.AskAnswer, bool) {
	r.batchCalls++
	r.reqs = reqs
	return r.ans, r.ok
}

func (r *recordingBatchOps) show(_ context.Context, _ tui.AskRequest, _ time.Duration) (tui.AskAnswer, bool) {
	r.showCalls++
	return tui.AskAnswer{}, false
}

func (r *recordingBatchOps) ops() *tui.AskOps {
	return &tui.AskOps{Show: r.show, ShowBatch: r.showBatch}
}

type staticSink struct{ labels []string }

func (s staticSink) Ask(context.Context, tool.AskRequest) (tool.AskResponse, error) {
	return tool.AskResponse{Labels: s.labels}, nil
}

// onTrue is the ask.autoAnswer policy being on, and sinkOf the headless
// fallback as a factory — the sink reads both per call now, so a test that
// flips the policy can hand a closure instead of a captured bool.
func onTrue() bool                              { return true }
func sinkOf(s tool.AskSink) func() tool.AskSink { return func() tool.AskSink { return s } }

func TestAskCardSinkPrefersTheCard(t *testing.T) {
	ops := &recordingOps{ans: tui.AskAnswer{Labels: []string{"red"}}, ok: true}
	sink := &askCardSink{ops: &tui.AskOps{Show: ops.show}, fallback: sinkOf(staticSink{labels: []string{"blue"}})}
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

// With ask.autoAnswer ON, a skip (or a terminal that cannot fit the card)
// must still answer from the recommendation, and it must answer NOW: the card
// already waited out ask.timeout, so falling through to the headless sink
// would wait it a second time and then report "no answer within" a wait
// nobody saw. A fallback that blocks forever proves the sink never reaches it.
func TestAskCardSinkSkipDoesNotWaitAgain(t *testing.T) {
	ops := &recordingOps{ok: false}
	sink := &askCardSink{ops: &tui.AskOps{Show: ops.show}, auto: onTrue, fallback: sinkOf(blockingSink{})}
	done := make(chan tool.AskResponse, 1)
	go func() {
		got, err := sink.Ask(context.Background(), tool.AskRequest{
			Question: "Q", Options: []tool.AskOption{{Label: "recommended-label"}},
			Recommended: []string{"recommended-label"},
		})
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	select {
	case got := <-done:
		if len(got.Labels) != 1 || got.Labels[0] != "recommended-label" {
			t.Fatalf("recommended path did not answer: %+v", got.Labels)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a skipped card must not wait the headless timeout a second time")
	}
	if ops.calls != 1 {
		t.Fatalf("card consulted %d times", ops.calls)
	}
}

// A mode with NO user — a print/rpc/acp one-shot — must not leave the ask
// tool waiting for a human who does not exist. The registry is where the mode
// declares it, so the wait cannot come back by accident: the old path blocked
// until the 10-minute tool deadline and then reported "its result is
// unknown", which reads like a lost side effect, not like "nobody answered".
func TestRegistryWithoutAUserAnswersNobody(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, &agent.PlanMode{}, false)
	got, ok := reg.Get(tool.AskToolName)
	if !ok {
		t.Fatal("ask tool missing from the registry")
	}
	at, isAsk := got.(*tool.AskTool)
	if !isAsk {
		t.Fatalf("ask registry entry is %T", got)
	}
	args := `{"question":"which db?","options":[{"label":"sqlite"}],"recommended":"sqlite"}`
	done := make(chan tool.Result, 1)
	go func() {
		res, err := at.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.IsError {
			t.Fatalf("ask errored: %q", res.Text)
		}
		if !strings.Contains(res.Text, "no answer") {
			t.Fatalf("result = %q, want the honest no-answer", res.Text)
		}
		if strings.Contains(res.Text, "sqlite") {
			t.Fatalf("result = %q, must not decide the question for the user", res.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a host with no user must not wait for an answer nobody can give")
	}
}

// The TUI has a user, so its registry keeps the waiting policy: the card owns
// the wait, and when the card has no room to draw the human answers in chat.
// The no-user path must not leak into the interactive host.
func TestRegistryWithAUserKeepsWaiting(t *testing.T) {
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, &agent.PlanMode{}, true)
	got, ok := reg.Get(tool.AskToolName)
	if !ok {
		t.Fatal("ask tool missing from the registry")
	}
	at, isAsk := got.(*tool.AskTool)
	if !isAsk {
		t.Fatalf("ask registry entry is %T", got)
	}
	args := `{"question":"which db?","options":[{"label":"sqlite"}],"recommended":"sqlite"}`
	done := make(chan tool.Result, 1)
	go func() {
		res, err := at.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		t.Fatalf("an interactive host answered nobody: %q", res.Text)
	case <-time.After(200 * time.Millisecond):
	}
}

// blockingSink answers never — it stands for the headless timeout wait.
type blockingSink struct{}

func (blockingSink) Ask(context.Context, tool.AskRequest) (tool.AskResponse, error) {
	select {}
}

func TestAskCardSinkBatchIsOneWait(t *testing.T) {
	ops := &recordingBatchOps{ok: true, ans: []tui.AskAnswer{
		{Labels: []string{"sqlite"}},
		{Note: "only if it caches"},
		{}, // left empty: the recommendation answers it, with no second wait
	}}
	sink := &askCardSink{ops: ops.ops(), auto: onTrue, fallback: sinkOf(blockingSink{})}
	reqs := []tool.AskRequest{
		{ID: "db", Question: "Which db?", Options: []tool.AskOption{{Label: "sqlite"}}},
		{ID: "cache", Question: "Cache?", Options: []tool.AskOption{{Label: "yes"}, {Label: "no"}}},
		{ID: "mode", Question: "Mode?", Options: []tool.AskOption{{Label: "fast"}, {Label: "safe"}}, Recommended: []string{"safe"}},
	}
	done := make(chan []tool.AskResponse, 1)
	go func() {
		got, err := sink.AskBatch(context.Background(), reqs)
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	var got []tool.AskResponse
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a batch card must not wait per question")
	}
	if ops.batchCalls != 1 || ops.showCalls != 0 {
		t.Fatalf("card calls: batch=%d show=%d", ops.batchCalls, ops.showCalls)
	}
	if len(got) != 3 || got[0].Labels[0] != "sqlite" || got[1].Note != "only if it caches" || got[2].Labels[0] != "safe" {
		t.Fatalf("batch answers = %+v", got)
	}
	// The card saw the ids, so its tabs can name the questions.
	if ops.reqs[0].ID != "db" || ops.reqs[2].Recommended[0] != "safe" {
		t.Fatalf("card requests mangled: %+v", ops.reqs)
	}
}

// TestAskCardSinkChatEscapeIsAnAnswer: "Chat about this" carries no labels. If
// the sink reads that as "no answer" it waits the headless timeout a second
// time and then answers for the human — the opposite of what they asked for.
func TestAskCardSinkChatEscapeIsAnAnswer(t *testing.T) {
	ops := &recordingOps{ans: tui.AskAnswer{Note: tui.AskChatLabel}, ok: true}
	sink := &askCardSink{ops: &tui.AskOps{Show: ops.show}, auto: onTrue, fallback: sinkOf(blockingSink{})}
	done := make(chan tool.AskResponse, 1)
	go func() {
		got, _ := sink.Ask(context.Background(), tool.AskRequest{
			Question: "Q", Options: []tool.AskOption{{Label: "x"}}, Recommended: []string{"x"},
		})
		done <- got
	}()
	select {
	case got := <-done:
		if got.Note != tui.AskChatLabel || len(got.Labels) != 0 {
			t.Fatalf("escape hatch = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the chat escape must answer at once, not wait again")
	}
}

// An unwired card (no ops) keeps the headless path — the tool must never hang
// or vanish because a host did not provide a UI seam.
func TestAskCardSinkWithoutOps(t *testing.T) {
	sink := &askCardSink{fallback: sinkOf(staticSink{labels: []string{"x"}})}
	got, err := sink.Ask(context.Background(), tool.AskRequest{Question: "Q", Options: []tool.AskOption{{Label: "x"}}})
	if err != nil || len(got.Labels) != 1 {
		t.Fatalf("headless fallback broken: %+v %v", got, err)
	}
}

// The default (ask.autoAnswer off): a skipped card is the human saying
// "not this one", not permission to decide for them. The sink must return
// nothing, and must still answer NOW — the card already waited out the
// policy's one wait, so falling to the headless sink would wait again.
func TestAskCardSinkSkipDoesNotAnswerWithoutAutoAnswer(t *testing.T) {
	ops := &recordingOps{ok: false}
	sink := &askCardSink{ops: &tui.AskOps{Show: ops.show}, fallback: sinkOf(blockingSink{})}
	done := make(chan tool.AskResponse, 1)
	go func() {
		got, err := sink.Ask(context.Background(), tool.AskRequest{
			Question: "Q", Options: []tool.AskOption{{Label: "recommended-label"}},
			Recommended: []string{"recommended-label"},
		})
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	select {
	case got := <-done:
		if len(got.Labels) != 0 || got.Note != "" {
			t.Fatalf("a skip must not answer: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a skipped card must not wait the headless policy again")
	}
	if ops.calls != 1 {
		t.Fatalf("card consulted %d times", ops.calls)
	}
}

// /auto-answer flips the policy MID-SESSION, so the sink must read it per
// call: one question answered from the recommendation, the flip, then the
// same skip must answer nothing. A captured bool would answer both — the
// human is told their card no longer decides, and it decides anyway.
func TestAskCardSinkReadsAutoAnswerPerCall(t *testing.T) {
	ops := &recordingOps{ok: false}
	auto := false
	sink := &askCardSink{
		ops:      &tui.AskOps{Show: ops.show},
		auto:     func() bool { return auto },
		fallback: sinkOf(blockingSink{}),
	}
	req := tool.AskRequest{Question: "Q", Options: []tool.AskOption{{Label: "rec"}}, Recommended: []string{"rec"}}
	ask := func() tool.AskResponse {
		got, err := sink.Ask(context.Background(), req)
		if err != nil {
			t.Error(err)
		}
		return got
	}
	if got := ask(); len(got.Labels) != 0 {
		t.Fatalf("auto-answer off: a skip must not answer, got %+v", got)
	}
	auto = true
	if got := ask(); len(got.Labels) != 1 || got.Labels[0] != "rec" {
		t.Fatalf("auto-answer on: the recommendation must answer, got %+v", got)
	}
}
