package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// storeMessages returns the messages a store holds, in order.
func storeMessages(t *testing.T, s *session.Store) []ai.Message {
	t.Helper()
	var out []ai.Message
	for _, e := range s.Entries() {
		if me, ok := e.(*session.MessageEntry); ok {
			out = append(out, me.Message)
		}
	}
	return out
}

// persistHooks mirrors what the hosts do: the assistant message is persisted
// on message_end (cmd/xdev's tuiHooks/printHooks), not by the agent.
func persistHooks(st *session.Store) TurnHooksFunc {
	return TurnHooksFunc{OnMessageEndF: func(m *ai.Message) {
		if m == nil {
			return
		}
		_ = st.Append(&session.MessageEntry{Message: *m})
	}}
}

// alwaysFailingProvider fails every stream with the same error, so the
// record names the failure whatever the recovery ladder does with it.
type alwaysFailingProvider struct{ err error }

func (p *alwaysFailingProvider) Stream(context.Context, ai.StreamRequest) (<-chan ai.Event, error) {
	ch := make(chan ai.Event, 1)
	ch <- ai.Errorf(p.err)
	close(ch)
	return ch, nil
}
func (p *alwaysFailingProvider) Name() string { return "failing" }
func (p *alwaysFailingProvider) API() string  { return "failing-api" }

// blockingProvider never answers: the stream stays open until the context is
// cancelled, which is what a user pressing Esc looks like.
type blockingProvider struct{}

func (p *blockingProvider) Stream(ctx context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	ch := make(chan ai.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}
func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) API() string  { return "blocking-api" }

// TestProviderErrorIsRecordedInTheTranscript: a turn that dies on a provider
// error must leave a record of WHY. Without one the transcript still ends on
// the user prompt, and the session reads "pending" — a turn in flight — for
// a turn that failed (omp rules 36, 37).
func TestProviderErrorIsRecordedInTheTranscript(t *testing.T) {
	st := session.OpenMem("/proj", "t")
	defer st.Close()
	a := &Agent{
		Provider: &alwaysFailingProvider{err: errors.New("upstream is on fire")},
		Tools:    tool.NewRegistry(),
		Store:    st,
		Hooks:    persistHooks(st),
		Retry:    RetryPolicy{MaxRetries: 0, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
	if _, err := a.Run(context.Background(), "sys", userHistory()); err == nil {
		t.Fatal("the run must fail")
	}
	msgs := storeMessages(t, st)
	if len(msgs) == 0 {
		t.Fatal("the failure was not recorded: the store holds no message")
	}
	last := msgs[len(msgs)-1]
	if last.Role != ai.RoleAssistant {
		t.Fatalf("the record's role = %q, want assistant", last.Role)
	}
	if last.StopReason != ai.StopReasonError {
		t.Fatalf("the record's stop reason = %q, want %q", last.StopReason, ai.StopReasonError)
	}
	if !strings.Contains(last.Text(), "upstream is on fire") {
		t.Fatalf("the record does not name the failure: %q", last.Text())
	}
}

// TestUserAbortIsRecordedAsAborted: a turn the user stopped is NOT a
// provider error. The record says "aborted", so the badge can tell the two
// apart.
func TestUserAbortIsRecordedAsAborted(t *testing.T) {
	st := session.OpenMem("/proj", "t")
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &Agent{
		Provider: &blockingProvider{},
		Tools:    tool.NewRegistry(),
		Store:    st,
		Hooks:    persistHooks(st),
		Retry:    RetryPolicy{MaxRetries: 0, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if _, err := a.Run(ctx, "sys", userHistory()); err == nil {
		t.Fatal("the run must fail")
	}
	msgs := storeMessages(t, st)
	if len(msgs) == 0 {
		t.Fatal("the abort was not recorded: the store holds no message")
	}
	last := msgs[len(msgs)-1]
	if last.StopReason != ai.StopReasonAborted {
		t.Fatalf("the record's stop reason = %q, want %q", last.StopReason, ai.StopReasonAborted)
	}
}

// TestTurnFailureRecordSaysNothingAboutAnAnswer: the record must not claim
// the turn produced one.
func TestTurnFailureRecordSaysNothingAboutAnAnswer(t *testing.T) {
	st := session.OpenMem("/proj", "t")
	defer st.Close()
	a := &Agent{
		Provider: &alwaysFailingProvider{err: errors.New("boom")},
		Tools:    tool.NewRegistry(),
		Store:    st,
		Hooks:    persistHooks(st),
		Retry:    RetryPolicy{MaxRetries: 0, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
	_, _ = a.Run(context.Background(), "sys", userHistory())
	msgs := storeMessages(t, st)
	if len(msgs) == 0 {
		t.Fatal("no record")
	}
	text := msgs[len(msgs)-1].Text()
	if !strings.Contains(text, "before it produced an answer") {
		t.Fatalf("the record does not say the turn produced no answer: %q", text)
	}
}
