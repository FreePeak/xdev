package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/protocol"
)

// fakeHandler serves canned responses and records what arrived, so the
// server loop is exercised without an agent.
type fakeHandler struct {
	mu       sync.Mutex
	prompts  []string
	steered  []string
	aborted  int
	sessions int
	models   []string

	srv *Server
}

func (f *fakeHandler) Prompt(id, text string) {
	f.mu.Lock()
	f.prompts = append(f.prompts, text)
	f.mu.Unlock()
	go func() {
		f.srv.SendEvent(id, protocol.EventFromAI(ai.Event{Type: ai.EventTextDelta, Delta: "he"}))
		f.srv.SendEvent(id, protocol.EventFromAI(ai.Event{Type: ai.EventTextDelta, Delta: "llo"}))
		f.srv.Respond(id, protocol.Response{Ok: true, Text: "hello", StopReason: "stop"})
	}()
}

func (f *fakeHandler) Steer(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steered = append(f.steered, text)
	return nil
}

func (f *fakeHandler) FollowUp(text string) error { return nil }

func (f *fakeHandler) Abort() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted++
	return nil
}

func (f *fakeHandler) NewSession() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions++
	return nil
}

func (f *fakeHandler) State() protocol.State {
	return protocol.State{SessionID: "sess-1", Running: false}
}

func (f *fakeHandler) SetModel(model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append(f.models, model)
	return nil
}

// frame is the decoded envelope: type + id + payload object.
type frame struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Frame json.RawMessage `json:"frame"`
}

// ioPipe returns a synchronous in-memory pipe pair.
func ioPipe() (io.ReadCloser, io.WriteCloser) {
	pr, pw := io.Pipe()
	return pr, pw
}

// sinkWriter discards writes (the ready frame + error path noise).
type sinkWriter struct{}

func (sinkWriter) Write(p []byte) (int, error) { return len(p), nil }

func mustFrame(t *testing.T, f frame) []byte {
	t.Helper()
	if f.Frame == nil {
		t.Fatalf("frame %s has no payload", f.Type)
	}
	return f.Frame
}

func TestServerEndToEnd(t *testing.T) {
	inR, inW := ioPipe()
	outR, outW := ioPipe()
	h := &fakeHandler{}
	srv := New(inR, outW, h)
	h.srv = srv

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	out := bufio.NewScanner(outR)
	out.Buffer(make([]byte, protocol.FrameLimit), protocol.FrameLimit)
	readFrame := func() frame {
		if !out.Scan() {
			t.Fatalf("output closed early: %v", out.Err())
		}
		var f frame
		if err := json.Unmarshal(out.Bytes(), &f); err != nil {
			t.Fatalf("bad frame %q: %v", out.Text(), err)
		}
		return f
	}

	// Ready frame first.
	var ready protocol.Ready
	if f := readFrame(); f.Type != protocol.TypeReady {
		t.Fatalf("first frame = %s, want ready", f.Type)
	} else if err := json.Unmarshal(mustFrame(t, f), &ready); err != nil {
		t.Fatal(err)
	}
	if ready.Protocol != protocol.ProtocolVersion || ready.FrameLimit != protocol.FrameLimit {
		t.Fatalf("ready = %+v", ready)
	}

	// Prompt: events correlate, response terminates.
	fmt.Fprintln(inW, `{"type":"prompt","id":"p1","text":"say hi"}`)
	var evText string
	for {
		f := readFrame()
		if f.Type == protocol.TypeEvent && f.ID == "p1" {
			var ev protocol.Event
			if err := json.Unmarshal(mustFrame(t, f), &ev); err != nil {
				t.Fatal(err)
			}
			evText += ev.Delta
		}
		if f.Type == protocol.TypeResponse && f.ID == "p1" {
			var resp protocol.Response
			if err := json.Unmarshal(mustFrame(t, f), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.Ok || resp.Text != "hello" || resp.StopReason != "stop" {
				t.Fatalf("prompt response = %+v", resp)
			}
			break
		}
	}
	if evText != "hello" {
		t.Fatalf("event deltas = %q", evText)
	}

	// Steer / state / set_model / new_session round-trips.
	fmt.Fprintln(inW, `{"type":"steer","id":"s1","text":"go left"}`)
	fmt.Fprintln(inW, `{"type":"state","id":"s2"}`)
	fmt.Fprintln(inW, `{"type":"set_model","id":"s3","model":"onegw/dev"}`)
	fmt.Fprintln(inW, `{"type":"new_session","id":"s4"}`)
	resp := map[string]protocol.Response{}
	for len(resp) < 4 {
		f := readFrame()
		if f.Type != protocol.TypeResponse {
			continue
		}
		var r protocol.Response
		if err := json.Unmarshal(mustFrame(t, f), &r); err != nil {
			t.Fatal(err)
		}
		resp[f.ID] = r
	}
	if !resp["s1"].Ok || !resp["s3"].Ok || !resp["s4"].Ok {
		t.Fatalf("responses = %+v", resp)
	}
	if resp["s2"].State == nil || resp["s2"].State.SessionID != "sess-1" {
		t.Fatalf("state response = %+v", resp["s2"])
	}

	// Unknown command errors by id.
	fmt.Fprintln(inW, `{"type":"bogus","id":"x1"}`)
	for {
		f := readFrame()
		if f.Type != protocol.TypeResponse {
			continue
		}
		var r protocol.Response
		if err := json.Unmarshal(mustFrame(t, f), &r); err != nil {
			t.Fatal(err)
		}
		if r.Ok || !strings.Contains(r.Err, "unknown frame type") {
			t.Fatalf("bogus response = %+v", r)
		}
		break
	}

	// Close input: Serve returns.
	inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not exit on input close")
	}

	if len(h.prompts) != 1 || h.prompts[0] != "say hi" {
		t.Fatalf("prompts = %v", h.prompts)
	}
	if len(h.steered) != 1 || h.steered[0] != "go left" {
		t.Fatalf("steered = %v", h.steered)
	}
	if len(h.models) != 1 || h.models[0] != "onegw/dev" {
		t.Fatalf("models = %v", h.models)
	}
	if h.sessions != 1 {
		t.Fatalf("sessions = %d", h.sessions)
	}
}

func TestServerRejectsOversizedFrame(t *testing.T) {
	// The oversized frame is fed from a plain buffer: a pipe writer would
	// block forever once the server stops reading at the frame limit.
	big := fmt.Sprintf(`{"type":"prompt","id":"p","text":"%s"}`, strings.Repeat("x", protocol.FrameLimit+10))
	in := struct{ io.Reader }{strings.NewReader(big + "\n")}
	done := make(chan error, 1)
	srv := New(in, sinkWriter{}, &fakeHandler{})
	go func() { done <- srv.Serve(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "frame exceeds") {
			t.Fatalf("Serve err = %v, want frame-limit error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return on oversized frame")
	}
}
