package tool

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBashOutputObserverSeesChunksBeforeResult is the live-output contract: a
// tool's stream is handed to the observer WHILE the command runs, not after
// Execute returns, so a UI can paint a command that is still going. Both
// streams tap, and the tap does not change the text the model is shown.
func TestBashOutputObserverSeesChunksBeforeResult(t *testing.T) {
	var mu sync.Mutex
	var chunks []string
	obs := OutputFunc(func(chunk string) {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, chunk)
	})
	ctx := WithOutputObserver(context.Background(), obs)

	bt := NewBashTool(t.TempDir())
	res, err := bt.Execute(ctx, args(t, map[string]any{
		"command": "printf 'out-1\\n'; sleep 0.2; printf 'out-2\\n'; printf 'err-1\\n' >&2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", res.Text)
	}
	mu.Lock()
	got := strings.Join(chunks, "")
	mu.Unlock()
	for _, want := range []string{"out-1", "out-2", "err-1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("observer never saw %q; got %q", want, got)
		}
	}
	// The tap observes; it does not perturb what the model is shown.
	if !strings.Contains(res.Text, "out-1") || !strings.Contains(res.Text, "out-2") {
		t.Fatalf("result lost output: %q", res.Text)
	}
}

// TestBashOutputObserverSeesStreamWhileRunning pins the timing half of the
// contract: the callback fires from the copier goroutine, so a chunk exists
// before Execute has returned. Without this the feature still passes a
// "chunks match the result" test while painting nothing until the end.
func TestBashOutputObserverSeesStreamWhileRunning(t *testing.T) {
	seen := make(chan string, 1)
	obs := OutputFunc(func(chunk string) {
		if strings.Contains(chunk, "first") {
			select {
			case seen <- chunk:
			default:
			}
		}
	})
	ctx := WithOutputObserver(context.Background(), obs)

	bt := NewBashTool(t.TempDir())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := bt.Execute(ctx, args(t, map[string]any{
			"command": "printf 'first\\n'; sleep 1",
		})); err != nil {
			t.Errorf("execute: %v", err)
		}
	}()
	select {
	case <-seen:
		// The chunk arrived while the command was still sleeping.
	case <-time.After(5 * time.Second):
		t.Fatal("no live chunk: the observer only ever saw the finished result")
	}
	<-done
}

// TestOutputObserverOfAbsent keeps the no-observer path the plain path: a
// caller that wires nothing (print mode, RPC, every test in this package)
// must run with no observer and no error.
func TestOutputObserverOfAbsent(t *testing.T) {
	if got := OutputObserverOf(context.Background()); got != nil {
		t.Fatalf("bare context carried an observer: %v", got)
	}
	if got := OutputObserverOf(WithOutputObserver(context.Background(), nil)); got != nil {
		t.Fatalf("nil observer was stored: %v", got)
	}
}
