package notify

import (
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/logx"
)

const (
	// defaultBuffer is how many events may be queued before Send starts
	// dropping. Failures arrive in bursts (a cancelled fan-out settles every
	// child at once); a queue this size absorbs that without unbounded growth.
	defaultBuffer = 64
	// defaultEmitTimeout bounds one transport call. A transport slower than
	// this is dropped: the alternative is a background job's settling goroutine
	// waiting on a user's shell script.
	defaultEmitTimeout = 2 * time.Second
	// defaultCloseTimeout bounds Close, so shutdown cannot hang on a wedged
	// transport.
	defaultCloseTimeout = 3 * time.Second
)

// Event is one thing worth telling the human about.
type Event struct {
	Kind      string    `json:"kind"` // e.g. "job_failed"
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	JobID     string    `json:"job_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	At        time.Time `json:"at"`
}

// Sanitized returns a copy of the event safe to emit: its human-readable text
// is stripped of terminal control bytes and capped, while the identifiers that
// address the job are left alone so a command sink can still act on them.
//
// Every sink sanitizes rather than trusting its caller, because the text
// originates in a model or a failing child process and the sink is the last
// place before it reaches a terminal.
func (e Event) Sanitized() Event {
	e.Title = Sanitize(e.Title, TitleLimit)
	e.Body = Sanitize(e.Body, BodyLimit)
	return e
}

// Sink delivers one event somewhere. Implementations must be safe to call from
// the dispatcher goroutine and should return quickly; a sink that blocks is
// dropped after the dispatcher's timeout, and a sink that panics is recovered
// and logged rather than allowed to end the session.
type Sink interface {
	Emit(Event) error
}

// SinkFunc adapts a function to Sink. It is how the in-app toast is wired in
// without internal/notify importing internal/tui, which would invert the
// dependency.
type SinkFunc func(Event) error

// Emit calls f.
func (f SinkFunc) Emit(ev Event) error { return f(ev) }

// Dispatcher fans one event out to every sink, off the caller's goroutine.
//
// It exists so that no notification can delay or crash the code that produced
// it: the hub fires settle listeners on the settling job's own goroutine and a
// panic there takes the process down, so the listener's only job is to hand an
// event to Send and return.
type Dispatcher struct {
	ch      chan Event
	sinks   []Sink
	timeout time.Duration
	wg      sync.WaitGroup
	once    sync.Once
}

// New starts a dispatcher over sinks. A buffer of zero or less uses
// defaultBuffer; a timeout of zero or less uses defaultEmitTimeout.
func New(buffer int, timeout time.Duration, sinks ...Sink) *Dispatcher {
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	if timeout <= 0 {
		timeout = defaultEmitTimeout
	}
	d := &Dispatcher{
		ch:      make(chan Event, buffer),
		sinks:   sinks,
		timeout: timeout,
	}
	d.wg.Add(1)
	go d.run()
	return d
}

// Send queues an event and returns immediately, dropping it if the queue is
// full. Dropping is deliberate: a notification is a courtesy, and blocking the
// caller -- a job settling goroutine -- to deliver one would trade a cosmetic
// loss for a real one.
func (d *Dispatcher) Send(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case d.ch <- ev:
	default:
		logx.Warnf("notify: dropped %s notification (queue full)", ev.Kind)
	}
}

// Close stops the dispatcher once the queue drains, waiting at most
// defaultCloseTimeout for in-flight transports. Calling it twice is safe.
func (d *Dispatcher) Close() {
	d.once.Do(func() { close(d.ch) })
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(defaultCloseTimeout):
		logx.Warnf("notify: dispatcher did not drain within %s", defaultCloseTimeout)
	}
}

func (d *Dispatcher) run() {
	defer d.wg.Done()
	for ev := range d.ch {
		for _, s := range d.sinks {
			d.emit(s, ev)
		}
	}
}

// emit calls one sink under a recover and a deadline.
func (d *Dispatcher) emit(s Sink, ev Event) {
	// ponytail: a sink that ignores the deadline leaks its goroutine until its
	// own call returns. Ceiling: one leaked goroutine per hung transport.
	// Upgrade path: give Sink a context so a sink can be cancelled properly.
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("notify: sink panicked on %s: %v", ev.Kind, r)
				done <- nil
			}
		}()
		done <- s.Emit(ev)
	}()
	select {
	case err := <-done:
		if err != nil {
			logx.Warnf("notify: sink failed on %s: %v", ev.Kind, err)
		}
	case <-time.After(d.timeout):
		logx.Warnf("notify: sink timed out after %s on %s", d.timeout, ev.Kind)
	}
}
