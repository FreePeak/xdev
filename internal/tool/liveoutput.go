package tool

import "context"

// OutputObserver is notified as a tool's output is produced, so a UI can paint
// a command live instead of waiting for the whole result. It is an OPTIONAL
// seam: a tool that emits nothing, a caller that sets no observer, and a
// nil observer are all no-ops, so nothing on the model's side of the path
// changes (the model still sees only the finished, sink-windowed text).
//
// Implementations must be cheap and non-blocking: a tool calls OnOutput from
// its copier goroutine, once per chunk, and a slow observer stalls the child.
type OutputObserver interface {
	// OnOutput receives the newest chunk as the tool's stream produced it.
	// Chunks are the tool's own read boundaries: they may split a line, and
	// only the last one of a run is followed by a settled result.
	OnOutput(chunk string)
}

// OutputFunc is the adapter for the common case: a plain function that wants
// the stream. It exists so a caller does not declare a one-method type just
// to receive chunks.
type OutputFunc func(chunk string)

// OnOutput implements OutputObserver.
func (f OutputFunc) OnOutput(chunk string) { f(chunk) }

// outputObserverKey is the private context key for the observer.
type outputObserverKey struct{}

// WithOutputObserver returns a context that carries obs, so every tool running
// under it reports its output. The observer rides the context rather than a
// tool field because one tool instance is shared by every concurrent call
// (a registry hands the same BashTool to six workers), and a per-call target
// has to be per-call data.
func WithOutputObserver(ctx context.Context, obs OutputObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, outputObserverKey{}, obs)
}

// OutputObserverOf returns the observer ctx carries, or nil.
func OutputObserverOf(ctx context.Context) OutputObserver {
	obs, _ := ctx.Value(outputObserverKey{}).(OutputObserver)
	return obs
}
