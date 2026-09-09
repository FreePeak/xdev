package tool

import (
	"fmt"
	"strings"
)

// OutputSink accumulates a byte stream into fixed head+tail windows so the
// in-memory footprint is bounded regardless of how much a tool emits
// (PRD §3.6: fixed head+tail windows, bounded everything; MVP spills no
// artifacts to disk — the truncation marker is the record).
type OutputSink struct {
	headLimit int
	tailLimit int

	head  []byte
	tail  []byte // ring buffer, len(tail) <= tailLimit
	total uint64 // total bytes ever written
}

// Truncation marker format (omp semantics, byte-for-byte).
const truncatedMarkerFormat = "\n[... output truncated: %d bytes total, showing first %d and last %d bytes ...]\n"

// DefaultOutputSink returns a sink with omp's default 32KB head + 32KB tail.
func DefaultOutputSink() *OutputSink {
	return NewOutputSink(32*1024, 32*1024)
}

// NewOutputSink returns a sink with the given head/tail window sizes.
// Both limits must be non-negative; a zero limit disables that window.
func NewOutputSink(headLimit, tailLimit int) *OutputSink {
	if headLimit < 0 {
		headLimit = 0
	}
	if tailLimit < 0 {
		tailLimit = 0
	}
	return &OutputSink{
		headLimit: headLimit,
		tailLimit: tailLimit,
		head:      make([]byte, 0, headLimit),
	}
}

// Write implements io.Writer.
func (s *OutputSink) Write(p []byte) (int, error) {
	n := len(p)
	s.total += uint64(n)

	if s.headLimit > 0 && len(s.head) < s.headLimit {
		take := min(len(p), s.headLimit-len(s.head))
		s.head = append(s.head, p[:take]...)
		p = p[take:]
	}

	if s.tailLimit > 0 && len(p) > 0 {
		// Bounded tail ring: keep the newest tailLimit bytes, discard oldest.
		if len(p) >= s.tailLimit {
			s.tail = append(s.tail[:0], p[len(p)-s.tailLimit:]...)
		} else {
			s.tail = append(s.tail, p...)
			if overflow := len(s.tail) - s.tailLimit; overflow > 0 {
				s.tail = append(s.tail[:0], s.tail[overflow:]...)
			}
		}
	}
	return n, nil
}

// Result returns the windowed text: head, then (when bytes were dropped)
// the truncation marker, then tail. Never exceeds
// headLimit + tailLimit + marker in memory.
func (s *OutputSink) Result() (text string, truncated bool) {
	if s.total == 0 {
		return "", false
	}
	headLen := len(s.head)
	dropped := s.total - uint64(headLen+len(s.tail))
	if dropped == 0 {
		return string(s.head), false
	}
	marker := fmt.Sprintf(truncatedMarkerFormat, s.total, headLen, len(s.tail))
	var b strings.Builder
	b.Grow(headLen + len(marker) + len(s.tail))
	b.Write(s.head)
	b.WriteString(marker)
	b.Write(s.tail)
	return b.String(), true
}

// Total returns the total number of bytes ever written to the sink.
func (s *OutputSink) Total() uint64 {
	return s.total
}
