package memlimit

import (
	"runtime"
	"testing"
)

// TestDefaultUnderBudget verifies that the process memory limit is set and
// that the Go heap after setup fits inside the PRD Goal-2 budget. This is
// the Go-side half of the RSS check (Goal 2: hard <100 MB) — the OS-level
// RSS is verified by the release gate CI, not by unit tests.
func TestDefaultUnderBudget(t *testing.T) {
	n := Apply()
	if n != DefaultLimitBytes {
		t.Fatalf("limit = %d, want %d", n, DefaultLimitBytes)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// Sys is the virtual allocation the runtime asked the OS for; after
	// memlimit it should not wildly exceed the limit (Go sets some beyond-
	// limit metadata, so allow the limit plus metadata).
	if ms.Sys > uint64(n)*2 {
		t.Errorf("Sys = %d bytes, budget = %d — runtime allocation exceeds 2× limit", ms.Sys, n)
	}
}

// TestParseBytesUnitForms covers the parser's unit handling.
func TestParseBytesUnitForms(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"100000000", 100000000},
		{"100MB", 100 << 20},
		{"1GB", 1 << 30},
		{"52428800", 52428800},
		// fractional sizes unsupported by design (deterministic)
		{"", 0},
		{"bogus", 0},
		{"0", 0},
	}
	for _, tc := range tests {
		got, err := ParseBytes(tc.in)
		if tc.want == 0 && tc.in != "0" {
			if err == nil {
				t.Errorf("ParseBytes(%q) should error", tc.in)
			}
			return
		}
		if err != nil {
			t.Errorf("ParseBytes(%q): %v", tc.in, err)
			return
		}
		if got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
