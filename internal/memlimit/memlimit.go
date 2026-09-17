// Package memlimit sets the process-wide Go memory limit (PRD §3.7 hard
// backstop: a runaway turn degrades into "compact now", never an OOM kill).
package memlimit

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// DefaultLimitBytes is the hard worst-case ceiling: 100 MB.
const DefaultLimitBytes = 100 << 20

// HighPressure is the live-heap fraction of the memory limit at which the
// agent forces compaction instead of waiting for the token threshold
// (PRD §3.7). Slightly below 1 so a compaction starts while there is still
// room to summarize, and above the steady-state heap of a normal session so
// the trigger does not fire on startup.
const HighPressure = 0.85

// ApplyFrom sets the process memory limit from an already-resolved value
// (layered settings), letting XDEV_MEMLIMIT still override it. n<=0 keeps
// the default.
func ApplyFrom(n int64) int64 {
	if v := strings.TrimSpace(os.Getenv("XDEV_MEMLIMIT")); v != "" {
		if parsed, err := ParseBytes(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n <= 0 {
		n = DefaultLimitBytes
	}
	debug.SetMemoryLimit(n)
	return n
}

// Apply sets debug.SetMemoryLimit from XDEV_MEMLIMIT ("100MB", "512KB",
// bare bytes) or falls back to DefaultLimitBytes. Returns the applied limit.
func Apply() int64 {
	v := strings.TrimSpace(os.Getenv("XDEV_MEMLIMIT"))
	n := int64(DefaultLimitBytes)
	if v != "" {
		if parsed, err := ParseBytes(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	debug.SetMemoryLimit(n)
	return n
}

// ParseBytes parses "12", "12B", "12KB", "12MB", "12GB" (case-insensitive).
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(strings.ToLower(s), "gb"):
		mult, s = 1<<30, s[:len(s)-2]
	case strings.HasSuffix(strings.ToLower(s), "mb"):
		mult, s = 1<<20, s[:len(s)-2]
	case strings.HasSuffix(strings.ToLower(s), "kb"):
		mult, s = 1<<10, s[:len(s)-2]
	case strings.HasSuffix(strings.ToLower(s), "b"):
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

// liveHeap and memLimit are seams over the runtime: tests pin the ratio
// without allocating for real or disturbing the process limit.
var (
	liveHeap = func() uint64 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	// A negative value asks SetMemoryLimit to report the current limit
	// without changing it.
	memLimit = func() int64 { return debug.SetMemoryLimit(-1) }
)

// Pressure reports live heap as a fraction of the process memory limit
// (0 when no limit is set, so the trigger stays inert).
func Pressure() float64 {
	limit := memLimit()
	if limit <= 0 {
		return 0
	}
	return float64(liveHeap()) / float64(limit)
}
