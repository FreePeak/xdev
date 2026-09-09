// Package memlimit sets the process-wide Go memory limit (PRD §3.7 hard
// backstop: a runaway turn degrades into "compact now", never an OOM kill).
package memlimit

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// DefaultLimitBytes is the hard worst-case ceiling: 100 MB.
const DefaultLimitBytes = 100 << 20

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
