package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/tool"
)

// silentProvider is a provider whose request never gets its response
// headers: the live 2026-10-20 shape, where the turn sat in the wait
// with nothing on the screen and nothing in the session file to say so.
type silentProvider struct {
	release chan struct{}
	calls   int
}

func (s *silentProvider) Stream(ctx context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	s.calls++
	<-s.release // the headers never come
	return nil, errors.New("openai-completions: net/http: timeout awaiting response headers")
}

func (s *silentProvider) Name() string { return "silent" }

func (s *silentProvider) API() string { return ai.APIOpenAICompletions }

// TestSilentProviderIsAnnounced: a request the provider has not answered
// must SAY so — on the event stream (the screen) and in the session file
// (the record that survives the turn).
//
// A turn that waits here writes no assistant message, so without the
// announcement the session file ends at the user's question and the screen
// shows an unexplained spinner: the 4h50m silence the user reported.
func TestSilentProviderIsAnnounced(t *testing.T) {
	old := slowRequestBudget
	slowRequestBudget = 20 * time.Millisecond
	defer func() { slowRequestBudget = old }()

	silent := &silentProvider{release: make(chan struct{})}
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	store := session.OpenMem("test", "t")
	events := make(chan ai.Event, 8)
	hooks := TurnHooksFunc{
		OnMessageEndF:    func(m *ai.Message) { _ = store.Append(&session.MessageEntry{Message: *m}) },
		OnToolResultMsgF: func(m *ai.Message) { _ = store.Append(&session.MessageEntry{Message: *m}) },
		OnEventF: func(e ai.Event) {
			select {
			case events <- e:
			default:
			}
		},
	}
	a := &Agent{Provider: silent, Tools: reg, Hooks: hooks, Store: store}
	a.Retry = fastRetry()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Run(context.Background(), "sys", []ai.Message{
			{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "brief me"}}},
		})
	}()

	select {
	case ev := <-events:
		var se *SilentProviderError
		if !errors.As(ev.Err, &se) {
			t.Fatalf("event = %v, want a SilentProviderError", ev)
		}
		if se.Messages != 1 {
			t.Fatalf("messages = %d, want 1", se.Messages)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no announcement for a provider that never answered")
	}
	close(silent.release)
	<-done

	// And the record: the session file must say what happened.
	found := false
	for _, e := range store.Entries() {
		if ce, ok := e.(*session.CustomEntry); ok && ce.CustomType == "recovery_silent_provider" {
			found = true
		}
	}
	if !found {
		t.Fatal("no recovery_silent_provider notice in the session file")
	}
}
