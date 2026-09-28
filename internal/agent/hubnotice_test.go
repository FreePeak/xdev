package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// hubNoticeFixture starts one background job that yields a fixed result and
// waits for it to settle, returning the hub, the job id and a channel carrying
// one (info, result) pair per settle.
func hubNoticeFixture(t *testing.T, resultJSON string) (*Hub, string, chan [2]any) {
	t.Helper()
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(resultJSON)}}}
	h := NewHub()
	settled := make(chan [2]any, 8)
	h.AddSettleListener(func(info JobInfo, res *SubagentResult) {
		settled <- [2]any{info, res}
	})
	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "bg", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 3,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return h, id, settled
}

func waitNotice(t *testing.T, ch chan [2]any) (JobInfo, *SubagentResult) {
	t.Helper()
	select {
	case v := <-ch:
		info, _ := v[0].(JobInfo)
		res, _ := v[1].(*SubagentResult)
		return info, res
	case <-time.After(10 * time.Second):
		t.Fatal("settle listener never fired")
		return JobInfo{}, nil
	}
}

// TestAddSettleListenerSeesTheTerminalStatus is the reason listeners get the
// snapshot handed to them: statusLocked only reports a terminal status once
// `done` is closed, so a listener that went to fetch it itself could easily
// read "running" for a job that had already finished.
func TestAddSettleListenerSeesTheTerminalStatus(t *testing.T) {
	_, _, settled := hubNoticeFixture(t, `{"result":"bg-done"}`)
	info, res := waitNotice(t, settled)

	if info.ID == "" {
		t.Fatal("listener got no job id")
	}
	if info.Status != "yielded" {
		t.Fatalf("listener status = %q, want yielded (the job had settled)", info.Status)
	}
	if res == nil || !strings.Contains(res.Text, "bg-done") {
		t.Fatalf("listener result = %+v, want the yielded text", res)
	}
}

// TestSettleListenersAreAdditiveWithSetNotify pins that a delivery listener can
// coexist with the dock's repaint. SetNotify is a slot, so a host that needs
// both must not have to multiplex them by hand.
func TestSettleListenersAreAdditiveWithSetNotify(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"x"}`)}}}
	h := NewHub()

	var mu sync.Mutex
	var bumps, notices int
	h.SetNotify(func() {
		mu.Lock()
		bumps++
		mu.Unlock()
	})
	h.AddSettleListener(func(JobInfo, *SubagentResult) {
		mu.Lock()
		notices++
		mu.Unlock()
	})
	h.AddSettleListener(nil) // must be ignored, not panic

	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "bg", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Wait(context.Background(), []string{id}, 10*time.Second); len(got) != 1 {
		t.Fatalf("job did not settle: %v", h.Jobs())
	}
	// The listener runs after close(done), so give it the same edge the
	// Wait above already crossed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		b, n := bumps, notices
		mu.Unlock()
		if b == 1 && n == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("bumps=%d notices=%d, want 1 and 1 — the two seams must both fire", bumps, notices)
}

// TestStartHubNoticeDeliveryDeliversOnce is the behavior change: one settled
// job produces exactly one persisted notice and one dispatch.
func TestStartHubNoticeDeliveryDeliversOnce(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"findings here"}`)}}}
	h := NewHub()

	var mu sync.Mutex
	var persisted, dispatched int
	admitted := 0
	StartHubNoticeDelivery(context.Background(), h,
		func() bool { admitted++; return true },
		func(notice string) error {
			mu.Lock()
			defer mu.Unlock()
			persisted++
			if !strings.Contains(notice, "findings here") {
				t.Errorf("notice lost the result text: %q", notice)
			}
			return nil
		},
		func(error) {
			mu.Lock()
			dispatched++
			mu.Unlock()
		})

	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "bg", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Wait(context.Background(), []string{id}, 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := persisted
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if persisted != 1 || dispatched != 1 {
		t.Fatalf("persisted=%d dispatched=%d, want 1 and 1", persisted, dispatched)
	}
	if admitted != 1 {
		t.Fatalf("admit called %d times, want 1", admitted)
	}
}

// TestStartHubNoticeDeliverySkipsWhenBusy pins the idle gate. A turn already
// running means the model will see the roster on its next step anyway, and
// starting a second turn out from under it is exactly the race the gate exists
// to prevent.
func TestStartHubNoticeDeliverySkipsWhenBusy(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{{events: yieldEvents(`{"result":"x"}`)}}}
	h := NewHub()

	var mu sync.Mutex
	persisted := 0
	StartHubNoticeDelivery(context.Background(), h,
		func() bool { return false }, // session busy
		func(string) error { mu.Lock(); persisted++; mu.Unlock(); return nil },
		func(error) {})

	id, _ := h.Start(context.Background(), SubagentSpec{
		Name: "bg", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 3,
	})
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	time.Sleep(100 * time.Millisecond) // let any stray delivery land

	mu.Lock()
	defer mu.Unlock()
	if persisted != 0 {
		t.Fatalf("persisted=%d while the admit gate was closed, want 0", persisted)
	}
}

// TestStartHubNoticeDeliveryDedupesRevive guards the double-notice. Revive and
// Send-to-parked re-enter launchLocked, so the same id settles again; without
// the seen-set a single revive would make the model believe it had two results.
func TestStartHubNoticeDeliveryDedupesRevive(t *testing.T) {
	p := &fakeProvider{calls: []fakeScript{
		{events: yieldEvents(`{"result":"first"}`)},
		{events: yieldEvents(`{"result":"second"}`)},
	}}
	h := NewHub()

	var mu sync.Mutex
	var notices []string
	StartHubNoticeDelivery(context.Background(), h,
		func() bool { return true },
		func(notice string) error {
			mu.Lock()
			notices = append(notices, notice)
			mu.Unlock()
			return nil
		}, func(error) {})

	id, err := h.Start(context.Background(), SubagentSpec{
		Name: "bg", Prompt: "work", Provider: p, Model: "m",
		Tools: []tool.Tool{}, MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Wait(context.Background(), []string{id}, 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(notices)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Park then revive the settled job: Revive re-enters launchLocked, so the
	// same id settles a second time and must not re-announce.
	if !h.Park(id) {
		t.Fatalf("park: job %q would not park", id)
	}
	if err := h.Revive(id, "again"); err != nil {
		t.Fatalf("revive: %v", err)
	}
	h.Wait(context.Background(), []string{id}, 10*time.Second)
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(notices) != 1 {
		t.Fatalf("got %d notices (%v), want 1 — a revive must not re-announce the same id+status", len(notices), notices)
	}
}

func TestHubNoticePrompt(t *testing.T) {
	t.Run("yielded carries the result", func(t *testing.T) {
		got := HubNoticePrompt(JobInfo{ID: "hub-1", Label: "scout", Status: "yielded"},
			&SubagentResult{Status: "yielded", Text: "the answer"})
		for _, want := range []string{"scout", "hub-1", "yielded", "the answer"} {
			if !strings.Contains(got, want) {
				t.Errorf("notice missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("failure names the error", func(t *testing.T) {
		got := HubNoticePrompt(JobInfo{ID: "hub-2", Label: "scout", Status: "failed"},
			&SubagentResult{Status: "failed", Err: "boom"})
		if !strings.Contains(got, "failed") || !strings.Contains(got, "boom") {
			t.Errorf("failure notice hides the cause:\n%s", got)
		}
	})

	t.Run("nil result is still a notice", func(t *testing.T) {
		got := HubNoticePrompt(JobInfo{ID: "hub-3", Status: "completed"}, nil)
		if !strings.Contains(got, "hub-3") {
			t.Errorf("nil-result notice drops the id:\n%s", got)
		}
	})

	t.Run("yield payload is used when there is no text", func(t *testing.T) {
		got := HubNoticePrompt(JobInfo{ID: "hub-4", Status: "yielded"},
			&SubagentResult{Status: "yielded", Yield: []byte(`{"k":"v"}`)})
		if !strings.Contains(got, `"k":"v"`) {
			t.Errorf("yield payload not surfaced:\n%s", got)
		}
	})

	t.Run("unlabelled job falls back to its id", func(t *testing.T) {
		got := HubNoticePrompt(JobInfo{ID: "hub-5", Status: "yielded"},
			&SubagentResult{Status: "yielded", Text: "t"})
		if !strings.Contains(got, "hub-5") {
			t.Errorf("no label and no id in the notice:\n%s", got)
		}
	})

	t.Run("a large result is truncated", func(t *testing.T) {
		big := strings.Repeat("z", maxHubNoticeChars*2)
		got := HubNoticePrompt(JobInfo{ID: "hub-6", Status: "yielded"},
			&SubagentResult{Status: "yielded", Text: big})
		if len(got) > maxHubNoticeChars+200 {
			t.Errorf("notice is %d chars; a large artifact must not ride this channel whole", len(got))
		}
		if !strings.Contains(got, "truncated") {
			t.Error("truncation is silent — the model cannot tell it is looking at a prefix")
		}
	})
}
