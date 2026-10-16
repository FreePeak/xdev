package memlimit

import (
	"testing"
)

// TestDefaultLeavesWorkingSetHeadroom: the shipped backstop must sit ABOVE a
// real session's live heap, because a Go soft limit below the working set does
// not cap anything — it only makes the collector run continuously (assist and
// background marking every few milliseconds, ~5% of a core at a measured
// 100 MB limit against a 222 MB RSS / 98 MB live session).
func TestDefaultLeavesWorkingSetHeadroom(t *testing.T) {
	n := Apply()
	if n != DefaultLimitBytes {
		t.Fatalf("limit = %d, want %d", n, DefaultLimitBytes)
	}
	// Where a working session actually sits: 100 MB is the tail of the range
	// measured on 3 MB answers, so the floor is set well clear of it.
	const workingSet = 512 << 20
	if DefaultLimitBytes < workingSet {
		t.Fatalf("DefaultLimitBytes = %d, below the measured working set %d — the collector would run continuously", DefaultLimitBytes, workingSet)
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

// TestPressureRatio covers the memory-pressure trigger input: heap over
// limit, and 0 when no limit is set (so the trigger stays inert). The seams
// are stubbed so the test never allocates toward the real limit.
func TestPressureRatio(t *testing.T) {
	oldHeap, oldLimit := liveHeap, memLimit
	t.Cleanup(func() { liveHeap, memLimit = oldHeap, oldLimit })

	liveHeap = func() uint64 { return 85 }
	memLimit = func() int64 { return 100 }
	if got, want := Pressure(), 85.0/100.0; got != want {
		t.Fatalf("Pressure = %v, want %v", got, want)
	}
	if Pressure() < HighPressure {
		t.Fatalf("85%% of the limit must reach HighPressure (%v)", HighPressure)
	}

	// Just below the threshold must not fire.
	liveHeap = func() uint64 { return 84 }
	if Pressure() >= HighPressure {
		t.Fatalf("84%% of the limit must not reach HighPressure (%v)", HighPressure)
	}

	// No limit: division is meaningless, so the trigger reads 0.
	memLimit = func() int64 { return 0 }
	if got := Pressure(); got != 0 {
		t.Fatalf("Pressure without a limit = %v, want 0", got)
	}
}
