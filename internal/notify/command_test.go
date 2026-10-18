package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCommandSinkReceivesSanitizedJSON pins the wire format: one Event as JSON
// on stdin, with the model-controlled text already made inert.
func TestCommandSinkReceivesSanitizedJSON(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "event.json")
	s := NewCommandSink("cat > "+out, time.Second)

	if !s.Enabled() {
		t.Fatal("a configured command should be enabled")
	}
	err := s.Emit(Event{
		Kind:  "job_failed",
		Title: "subagent failed",
		Body:  "\x1b]52;c;cGF3bg==\x07\nexit 1",
		JobID: "job-42",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var got Event
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload is not JSON (%q): %v", raw, err)
	}
	if got.Kind != "job_failed" || got.JobID != "job-42" {
		t.Fatalf("payload lost fields: %+v", got)
	}
	for i := 0; i < len(got.Body); i++ {
		if b := got.Body[i]; b < 0x20 || b == 0x7f {
			t.Fatalf("payload body carries control byte %#x: %q", b, got.Body)
		}
	}
}

func TestCommandSinkDisabledWhenEmpty(t *testing.T) {
	s := NewCommandSink("", time.Second)
	if s.Enabled() {
		t.Fatal("an empty command must be disabled")
	}
	if err := s.Emit(Event{Kind: "job_failed"}); err != nil {
		t.Fatalf("disabled sink returned %v, want nil", err)
	}
}

// TestCommandSinkFailsOnNonZeroExit: a misconfigured program is reported, not
// silently swallowed, so the operator can see it in the log.
func TestCommandSinkFailsOnNonZeroExit(t *testing.T) {
	s := NewCommandSink("exit 3", time.Second)
	if err := s.Emit(Event{Kind: "job_failed"}); err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
}

// TestCommandSinkTimesOut: a hung program costs one timeout, never the session.
func TestCommandSinkTimesOut(t *testing.T) {
	s := NewCommandSink("sleep 30", 100*time.Millisecond)
	start := time.Now()
	err := s.Emit(Event{Kind: "job_failed"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Emmit blocked for %s despite a 100ms deadline", elapsed)
	}
}

// TestBoundedBufferCaps keeps a chatty program from growing the heap.
func TestBoundedBufferCaps(t *testing.T) {
	b := &boundedBuffer{limit: 8}
	n, err := b.Write([]byte("0123456789abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("Write = (%d, %v), want (16, nil)", n, err)
	}
	if got := b.String(); got != "01234567" {
		t.Fatalf("buffer kept %q, want %q", got, "01234567")
	}
}
