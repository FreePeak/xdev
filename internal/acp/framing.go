package acp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxFrame caps one decoded frame: a runaway peer line must not make xdev
// allocate without bound.
const maxFrame = 16 << 20

// errFrameTooLong ends the read loop when a single line exceeds maxFrame.
var errFrameTooLong = errors.New("acp: frame exceeds the size cap")

// writeFrame writes one newline-delimited JSON frame.
//
// ACP frames messages with a single '\n' after the JSON body — it is NOT the
// LSP/DAP `Content-Length: <n>\r\n\r\n` header block. xdev spoke LSP framing
// here (the comment said "LSP-style" and meant it), so an editor reading the
// session stream line-by-line saw a header line, then a bare JSON body with no
// delimiter it trusted: nothing parsed (parity finding T5; omp writes
// `JSON.stringify(msg) + "\n"`).
func writeFrame(w io.Writer, body []byte) error {
	if _, err := w.Write(body); err != nil {
		return err
	}
	_, err := w.Write([]byte{'\n'})
	return err
}

// newReader builds the frame reader with the size cap baked into the buffer,
// so an over-long line fails fast instead of growing the buffer forever.
func newReader(r io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(r, 64<<10)
}

// readFrame reads one newline-delimited JSON frame. Blank lines are skipped (a
// peer may emit them between messages); the stream's end is reported as io.EOF
// and an over-long line as errFrameTooLong.
//
// The line is assembled with ReadSlice, NOT ReadString: ReadString grows its
// buffer to fit any line, so a peer that sends 16 MB of JSON with no newline
// would be buffered in full before anything could reject it. ReadSlice returns
// bufio.ErrBufferFull at the buffer boundary, which is what makes maxFrame
// real.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var (
		frame []byte
		seen  int
	)
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			seen += len(chunk)
			if seen > maxFrame {
				return nil, fmt.Errorf("%w (cap %d bytes)", errFrameTooLong, maxFrame)
			}
			// ReadSlice returns a slice valid only until the next read, so the
			// bytes are copied into the growing frame.
			frame = append(frame, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // long line so far: keep accumulating under the cap
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(strings.TrimSpace(string(frame))) > 0 {
				// A final line without a trailing newline is still a frame.
				return trimFrame(frame), nil
			}
			return nil, err
		}
		trimmed := trimFrame(frame)
		if len(trimmed) == 0 {
			frame, seen = nil, 0
			continue // blank separator line between messages
		}
		return trimmed, nil
	}
}

// trimFrame strips the line terminator and surrounding space from one frame.
func trimFrame(b []byte) []byte {
	return []byte(strings.TrimSpace(strings.TrimRight(string(b), "\r\n")))
}
