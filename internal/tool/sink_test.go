package tool

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSinkHeadOnlyFit(t *testing.T) {
	s := NewOutputSink(16, 16)
	if _, err := s.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	text, truncated := s.Result()
	if truncated {
		t.Fatalf("expected no truncation, got marker in %q", text)
	}
	if text != "hello world" {
		t.Fatalf("got %q, want %q", text, "hello world")
	}
	if s.Total() != 11 {
		t.Fatalf("total = %d, want 11", s.Total())
	}
}

func TestSinkEmpty(t *testing.T) {
	s := DefaultOutputSink()
	text, truncated := s.Result()
	if text != "" || truncated {
		t.Fatalf("empty sink produced %q truncated=%v", text, truncated)
	}
}

func TestSinkOverflowByteMath(t *testing.T) {
	const head, tail = 10, 10
	s := NewOutputSink(head, tail)
	// 36 bytes: head keeps the first 10, tail the last 10, the 16 middle
	// bytes are dropped — exactly the window math the marker must report.
	data := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	if _, err := s.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	text, truncated := s.Result()
	if !truncated {
		t.Fatal("expected truncated=true for overflow")
	}
	want := "0123456789" +
		"\n[... output truncated: 36 bytes total, showing first 10 and last 10 bytes ...]\n" +
		"QRSTUVWXYZ"
	if text != want {
		t.Fatalf("unexpected result %q", text)
	}
}

func TestSinkRingDiscardKeepsNewest(t *testing.T) {
	s := NewOutputSink(4, 4)
	// Six writes of 2 bytes each: head takes 0-3, tail ring holds newest 4.
	for _, chunk := range []string{"aa", "bb", "cc", "dd", "ee", "ff"} {
		if _, err := s.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	text, truncated := s.Result()
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if !strings.HasPrefix(text, "aabb") || !strings.HasSuffix(text, "eeff") {
		t.Fatalf("tail did not keep newest bytes: %q", text)
	}
	if !strings.Contains(text, "12 bytes total, showing first 4 and last 4 bytes") {
		t.Fatalf("marker byte math wrong: %q", text)
	}
}

func TestSinkLargeTailWriteReplacesRing(t *testing.T) {
	s := NewOutputSink(2, 4)
	if _, err := s.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	text, truncated := s.Result()
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if text != "01"+"\n[... output truncated: 10 bytes total, showing first 2 and last 4 bytes ...]\n"+"6789" {
		t.Fatalf("unexpected result %q", text)
	}
}

func TestSinkBoundedMemory(t *testing.T) {
	s := NewOutputSink(1024, 1024)
	big := bytes.Repeat([]byte("x"), 1<<20)
	if _, err := s.Write(big); err != nil {
		t.Fatal(err)
	}
	if got := len(s.head) + len(s.tail); got > 2048 {
		t.Fatalf("sink holds %d bytes, want <= headLimit+tailLimit", got)
	}
	text, truncated := s.Result()
	if !truncated || len(text) > 1024+1024+200 {
		t.Fatalf("result not bounded: len=%d truncated=%v", len(text), truncated)
	}
	if s.Total() != 1<<20 {
		t.Fatalf("total = %d", s.Total())
	}
}

func TestSinkConcurrentWritesStayBounded(t *testing.T) {
	// Sinks are used from a single reader goroutine in bash; this only
	// checks that the ring arithmetic never grows unbounded under chunks
	// of mixed sizes.
	s := NewOutputSink(8, 8)
	chunks := [][]byte{bytes.Repeat([]byte("a"), 5), bytes.Repeat([]byte("b"), 7), bytes.Repeat([]byte("c"), 9)}
	for _, c := range chunks {
		if _, err := s.Write(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.head) + len(s.tail); got > 16 {
		t.Fatalf("sink holds %d bytes", got)
	}
}

// TestCappedWriterKillsOnOverflow unit-tests the 8MB backpressure logic
// against a synthetic cap (feeding 9MB through a small cap instead of
// through the real 8MB constant).
func TestCappedWriterKillsOnOverflow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cap      int64
		write    int
		chunk    int
		wantKill int // how many kill calls are expected (0 or 1)
	}{
		{"under cap", 1000, 999, 100, 0},
		{"exactly at cap", 1000, 1000, 250, 0},
		{"one byte over", 1000, 1001, 100, 1},
		{"big single write", 1000, 5000, 5000, 1},
		{"many small over", 1000, 3000, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := NewOutputSink(64, 64)
			var combined atomic.Int64
			var killOnce sync.Once
			kills := 0
			w := &cappedWriter{sink: sink, combined: &combined, cap: tc.cap, kill: func() { killOnce.Do(func() { kills++ }) }}
			payload := bytes.Repeat([]byte("x"), tc.chunk)
			for written := 0; written < tc.write; written += tc.chunk {
				n := min(tc.chunk, tc.write-written)
				if _, err := w.Write(payload[:n]); err != nil {
					t.Fatal(err)
				}
			}
			if kills != tc.wantKill {
				t.Fatalf("kills = %d, want %d (combined=%d cap=%d)", kills, tc.wantKill, combined.Load(), tc.cap)
			}
			// The kill must fire exactly when the cap is crossed, and
			// only once even if writes keep coming.
			if tc.wantKill == 1 && combined.Load() <= tc.cap {
				t.Fatalf("kill fired under cap: combined=%d cap=%d", combined.Load(), tc.cap)
			}
		})
	}
}
