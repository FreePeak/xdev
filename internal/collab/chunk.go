package collab

import (
	"bytes"
	"fmt"
)

// DefaultChunkSize is the snapshot chunk payload (bytes of JSONL per frame).
// Each arrival resets the guest's progress timeout, so a large transcript
// streams instead of arriving as one oversized frame.
const DefaultChunkSize = 64 << 10

// MaxSnapshot bounds a reassembled snapshot: a hostile or broken host cannot
// make a guest allocate without limit.
const MaxSnapshot = 32 << 20

// ChunkBytes splits data into <=size chunks (size <= 0 → DefaultChunkSize).
// Empty input yields no chunks.
func ChunkBytes(data []byte, size int) [][]byte {
	if size <= 0 {
		size = DefaultChunkSize
	}
	if len(data) == 0 {
		return nil
	}
	var out [][]byte
	for off := 0; off < len(data); off += size {
		end := off + size
		if end > len(data) {
			end = len(data)
		}
		out = append(out, data[off:end])
	}
	return out
}

// Reassembler rebuilds a snapshot from snapshot-chunk frames. It tolerates
// out-of-order arrival and rejects duplicates that disagree, gaps that never
// fill, and totals beyond MaxSnapshot.
type Reassembler struct {
	total int
	parts [][]byte
	got   int
	bytes int
}

// Add feeds one chunk. It returns the full snapshot once the last chunk
// arrives (done=true); until then data is nil.
func (r *Reassembler) Add(index, total int, chunk []byte) (data []byte, done bool, err error) {
	if total <= 0 || index < 0 || index >= total {
		return nil, false, fmt.Errorf("collab: bad snapshot chunk %d/%d", index, total)
	}
	if total*DefaultChunkSize > MaxSnapshot {
		return nil, false, fmt.Errorf("collab: snapshot of %d chunks exceeds the %d-byte limit", total, MaxSnapshot)
	}
	if r.total == 0 {
		r.total, r.parts = total, make([][]byte, total)
	} else if r.total != total {
		return nil, false, fmt.Errorf("collab: snapshot chunk total changed from %d to %d", r.total, total)
	}
	if r.parts[index] != nil {
		if !bytes.Equal(r.parts[index], chunk) {
			return nil, false, fmt.Errorf("collab: snapshot chunk %d arrived twice with different content", index)
		}
		return nil, false, nil
	}
	if r.bytes+len(chunk) > MaxSnapshot {
		return nil, false, fmt.Errorf("collab: snapshot exceeds the %d-byte limit", MaxSnapshot)
	}
	r.parts[index] = append([]byte(nil), chunk...)
	r.bytes += len(chunk)
	r.got++
	if r.got != r.total {
		return nil, false, nil
	}
	var buf bytes.Buffer
	buf.Grow(r.bytes)
	for _, p := range r.parts {
		buf.Write(p)
	}
	out := buf.Bytes()
	r.Reset()
	return out, true, nil
}

// Reset drops all buffered chunks (a new snapshot starts from scratch).
func (r *Reassembler) Reset() {
	r.total, r.parts, r.got, r.bytes = 0, nil, 0, 0
}

// Pending reports how many chunks are still missing.
func (r *Reassembler) Pending() int {
	if r.total == 0 {
		return 0
	}
	return r.total - r.got
}
