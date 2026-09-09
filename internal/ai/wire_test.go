package ai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// captureBody is a channel-based request-body capture: the handler sends the
// raw body before writing any SSE frames, so the test can read it after the
// stream completes without data races.
type captureBody chan []byte

func (c captureBody) get(t *testing.T) []byte {
	t.Helper()
	select {
	case b := <-c:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for request body")
		return nil
	}
}

// newStreamServer serves the given SSE frames as [event, data] pairs on every
// request. An empty event name omits the event: line.
func newStreamServer(t *testing.T, frames ...[2]string) (*httptest.Server, captureBody) {
	t.Helper()
	body := make(captureBody, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		body <- buf
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, f := range frames {
			if f[0] != "" {
				fmt.Fprintf(w, "event: %s\n", f[0])
			}
			fmt.Fprintf(w, "data: %s\n\n", f[1])
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, body
}

// collectEvents drains ch with a hard timeout, returning all events.
func collectEvents(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Type == EventDone || ev.Type == EventError {
				for ev := range ch {
					out = append(out, ev)
				}
				return out
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout collecting events; got %d events", len(out))
		}
	}
}

// eventTypes projects events to their type names for order assertions.
func eventTypes(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, string(ev.Type))
	}
	return out
}

func assertOrder(t *testing.T, got []Event, want ...EventType) {
	t.Helper()
	gotTypes := eventTypes(got)
	wantTypes := make([]string, 0, len(want))
	for _, w := range want {
		wantTypes = append(wantTypes, string(w))
	}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event order = %v, want %v", gotTypes, wantTypes)
	}
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Fatalf("event order = %v, want %v (mismatch at %d)", gotTypes, wantTypes, i)
		}
	}
}

// jpath walks a decoded JSON document; string path elements index objects,
// int elements index arrays. Returns nil when any step is missing.
func jpath(m map[string]any, path ...any) any {
	var cur any = m
	for _, p := range path {
		switch key := p.(type) {
		case string:
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur, ok = obj[key]
			if !ok {
				return nil
			}
		case int:
			arr, ok := cur.([]any)
			if !ok || key >= len(arr) {
				return nil
			}
			cur = arr[key]
		}
	}
	return cur
}

func jstr(t *testing.T, m map[string]any, path ...any) string {
	t.Helper()
	v := jpath(m, path...)
	s, ok := v.(string)
	if !ok {
		t.Fatalf("path %v = %v (%T), want string", path, v, v)
	}
	return s
}

func jnum(t *testing.T, m map[string]any, path ...any) float64 {
	t.Helper()
	v := jpath(m, path...)
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("path %v = %v (%T), want number", path, v, v)
	}
	return f
}

func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode request body: %v (%s)", err, b)
	}
	return m
}
