package dap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	msg := &message{Seq: 1, Type: typeRequest, Command: "initialize", Arguments: json.RawMessage(`{"adapterID":"dlv"}`)}
	var buf bytes.Buffer
	if err := writeFrame(&buf, msg); err != nil {
		t.Fatal(err)
	}
	wire := buf.String()
	if !strings.HasPrefix(wire, "Content-Length: ") {
		t.Fatalf("frame has no Content-Length header: %q", wire)
	}
	var declared int
	if _, err := fmt.Sscanf(strings.SplitN(wire, "\r\n", 2)[0], "Content-Length: %d", &declared); err != nil {
		t.Fatalf("header: %v", err)
	}
	body, err := readFrame(bufio.NewReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	if declared != len(body) {
		t.Fatalf("declared %d bytes, body has %d", declared, len(body))
	}
	var got message
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != typeRequest || got.Command != "initialize" || got.Seq != 1 {
		t.Fatalf("round trip changed the message: %+v", got)
	}
	if string(got.Arguments) != `{"adapterID":"dlv"}` {
		t.Fatalf("arguments: %s", got.Arguments)
	}
}

func TestReadFrameRejectsBadFrames(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"missing content-length", "\r\n{}", "missing Content-Length"},
		{"unparsable length", "Content-Length: nope\r\n\r\n{}", "bad Content-Length"},
		{"oversized", fmt.Sprintf("Content-Length: %d\r\n\r\n", maxFrame+1), "exceeds the"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readFrame(bufio.NewReader(strings.NewReader(tc.wire)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// The DAP framing is case-insensitive on header names and must skip the extra
// headers some adapters add.
func TestReadFrameSkipsUnknownHeaders(t *testing.T) {
	body := `{"seq":1}`
	wire := fmt.Sprintf("Content-Type: application/json\r\nCONTENT-LENGTH: %d\r\n\r\n%s", len(body), body)
	got, err := readFrame(bufio.NewReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestCallReturnsBodyAndDispatchesEvents(t *testing.T) {
	f := newFakeAdapter(t, nil)
	f.setRespond(func(command string, _ json.RawMessage) (json.RawMessage, error) {
		if command != "stackTrace" {
			return nil, fmt.Errorf("unexpected request %s", command)
		}
		f.emit("output", OutputEvent{Category: "stdout", Output: "hello\n"})
		return json.RawMessage(`{"totalFrames":1,"stackFrames":[{"id":3,"name":"main.main","line":7}]}`), nil
	})
	body, err := f.client.Call(context.Background(), "stackTrace", map[string]any{"threadId": 1})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := decodeList[StackFrame](body, "stackFrames")
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Name != "main.main" || frames[0].Line != 7 {
		t.Fatalf("frames = %+v", frames)
	}
	if total := totalFrames(body); total != 1 {
		t.Fatalf("totalFrames = %d", total)
	}
	ev, err := f.client.NextEvent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "output" {
		t.Fatalf("event = %q", ev.Name)
	}
	var out OutputEvent
	if err := json.Unmarshal(ev.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Output != "hello\n" {
		t.Fatalf("output = %q", out.Output)
	}
}

func TestCallReportsAdapterFailure(t *testing.T) {
	f := newFakeAdapter(t, func(string, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("no variable named x")
	})
	_, err := f.client.Call(context.Background(), "evaluate", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no variable named x") {
		t.Fatalf("err = %v, want the adapter's message", err)
	}
	if !strings.Contains(err.Error(), "evaluate") {
		t.Fatalf("err = %v, want the failing command named", err)
	}
}

func TestCallTimesOut(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	f := newFakeAdapter(t, func(string, json.RawMessage) (json.RawMessage, error) {
		<-block
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := f.client.Call(ctx, "threads", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestClientDetectsAdapterExit(t *testing.T) {
	f := newFakeAdapter(t, nil)
	_, _ = f.client.stderr.Write([]byte("boom: cannot debug\n"))
	f.exit()
	select {
	case <-f.client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the client never noticed the adapter exit")
	}
	if f.client.alive() {
		t.Fatal("client still reports alive after the adapter exited")
	}
	if _, err := f.client.NextEvent(context.Background()); err == nil {
		t.Fatal("NextEvent on a dead client should fail")
	}
	if _, err := f.client.Call(context.Background(), "threads", nil); err == nil {
		t.Fatal("Call on a dead client should fail")
	}
	if got := f.client.Dead().Error(); !strings.Contains(got, "boom") {
		t.Fatalf("Dead() = %q, want the adapter's stderr in it", got)
	}
}

// A debuggee that floods output must not grow the client: events arriving
// while the buffer is full are dropped and counted, and the client stays
// usable (a drop is not a protocol failure).
func TestEventBufferDropsExcess(t *testing.T) {
	const sent = eventBuffer + 10
	f := newFakeAdapter(t, nil)
	for i := 0; i < sent; i++ {
		f.emit("output", OutputEvent{Output: "x"})
	}
	waitFor(t, "dropped events", func() bool { return f.client.Dropped() > 0 })

	got := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := f.client.NextEvent(ctx)
		cancel()
		if err != nil {
			break
		}
		got++
	}
	if got == 0 {
		t.Fatal("no event was delivered")
	}
	if got+f.client.Dropped() > sent {
		t.Fatalf("delivered %d + dropped %d > %d events sent", got, f.client.Dropped(), sent)
	}
	if f.client.Dropped() == 0 {
		t.Fatalf("%d events never exceeded the %d event buffer", sent, eventBuffer)
	}
}

// WaitEvent must ignore unrelated events and give up when the context does.
func TestWaitEvent(t *testing.T) {
	f := newFakeAdapter(t, nil)
	f.emit("output", OutputEvent{Output: "noise"})
	f.emit("initialized", nil)
	ev, err := f.client.WaitEvent(context.Background(), time.Second, "initialized")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "initialized" {
		t.Fatalf("event = %q", ev.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.client.WaitEvent(ctx, time.Second, "stopped"); err == nil {
		t.Fatal("WaitEvent should fail when the event never arrives")
	}
}

// Close is idempotent and ends every pending request.
func TestCloseIsIdempotent(t *testing.T) {
	f := newDefaultAdapter(t)
	if _, err := f.client.Call(context.Background(), "threads", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Close(); err != nil {
		t.Fatal(err)
	}
	if f.client.alive() {
		t.Fatal("client alive after Close")
	}
}
