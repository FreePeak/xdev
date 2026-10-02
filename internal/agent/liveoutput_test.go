package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// streamSpy reports the output the CALL CONTEXT carries, the way a streaming
// tool (bash) does, and can be held alive so two calls overlap.
type streamSpy struct {
	name string
	held time.Duration
	mu   *sync.Mutex
	ran  *[]string
}

func (s streamSpy) Name() string        { return s.name }
func (s streamSpy) Description() string { return "streams output" }
func (s streamSpy) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (s streamSpy) Execute(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	if obs := tool.OutputObserverOf(ctx); obs != nil {
		obs.OnOutput("head-" + s.name)
	}
	s.mu.Lock()
	*s.ran = append(*s.ran, s.name)
	s.mu.Unlock()
	if s.held > 0 {
		time.Sleep(s.held)
	}
	return tool.Result{Text: "done " + s.name}, nil
}

// observerSpy records whether the context it ran under carried an observer.
// It is the plain-path probe: with Agent.OnOutput nil nothing may reach it.
type observerSpy struct {
	mu       *sync.Mutex
	ran      *[]string
	observed *[]string
}

func (o observerSpy) Name() string        { return "bash" }
func (o observerSpy) Description() string { return "reports its observer" }
func (o observerSpy) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (o observerSpy) Execute(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	*o.ran = append(*o.ran, "bash")
	if obs := tool.OutputObserverOf(ctx); obs != nil {
		*o.observed = append(*o.observed, "observed")
	}
	return tool.Result{Text: "done"}, nil
}

// The live-output seam must carry the id and name of the call the bytes came
// from. A batch's calls run CONCURRENTLY (MaxToolWorkers), so a callback
// without them cannot route: two `bash` calls' output would paint under each
// other, which is exactly the mis-pairing FinishTool matches by id to avoid.
func TestOnOutputCarriesTheCallIdentity(t *testing.T) {
	var mu sync.Mutex
	var ran, got []string
	reg := tool.NewRegistry()
	// One registered tool, two concurrent calls: the real shape (a registry
	// hands the same BashTool to every worker).
	reg.Register(streamSpy{name: "bash", held: 100 * time.Millisecond, mu: &mu, ran: &ran})
	a := &Agent{
		Tools: reg, Model: "m", Hooks: &hookLog{},
		OnOutput: func(callID, name, chunk string) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, callID+"|"+name+"|"+chunk)
		},
	}
	var wg sync.WaitGroup
	for _, id := range []string{"call-a", "call-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			a.runOneTool(context.Background(), ai.ToolCallBlock{
				ID: id, Name: "bash", Arguments: json.RawMessage(`{}`),
			})
		}(id)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 {
		t.Fatalf("both calls must run: %v", ran)
	}
	// Each call's stream is stamped with ITS OWN id: a shared observer would
	// stamp both with whichever call wired it.
	ids := map[string]bool{}
	for _, s := range got {
		id, rest, ok := strings.Cut(s, "|")
		if !ok {
			t.Fatalf("callback carried no call identity: %q", s)
		}
		if rest != "bash|head-bash" {
			t.Fatalf("callback did not carry the tool's own bytes: %q", s)
		}
		ids[id] = true
	}
	if len(ids) != 2 || !ids["call-a"] || !ids["call-b"] {
		t.Fatalf("output was not routed per call: %v", got)
	}
}

// An agent with no OnOutput must hand tools the untouched context: the plain
// path (print mode, RPC, every other test) is byte-for-byte the old path.
func TestNoOnOutputLeavesToolsUnobserved(t *testing.T) {
	var mu sync.Mutex
	var ran, observed []string
	reg := tool.NewRegistry()
	reg.Register(observerSpy{mu: &mu, ran: &ran, observed: &observed})
	a := &Agent{Tools: reg, Model: "m", Hooks: &hookLog{}}
	if msg := a.runOneTool(context.Background(), ai.ToolCallBlock{
		Name: "bash", Arguments: json.RawMessage(`{}`),
	}); msg.IsError {
		t.Fatalf("call failed: %s", msg.Text())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran[0] != "bash" {
		t.Fatalf("the tool never ran: %v", ran)
	}
	if len(observed) != 0 {
		t.Fatalf("a tool saw an observer nobody wired: %v", observed)
	}
}
