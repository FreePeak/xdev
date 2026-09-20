// Package sse provides the shared Server-Sent-Events frame reader used by
// every wire adapter (PRD §3.3: "four wire adapters, one shared SSE reader").
package sse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// MaxLineBytes bounds a single SSE line (1 MiB) — honoring the <100 MB RSS
// budget by never buffering an unbounded line.
const MaxLineBytes = 1 << 20

// Frame is one dispatched SSE event: the optional event name and the
// concatenated data payload.
type Frame struct {
	Event string // "" when the server sent no event: field
	Data  string
}

// IsDone reports whether an OpenAI-family frame is the [DONE] sentinel.
func (f Frame) IsDone() bool { return f.Data == "[DONE]" }

// Reader parses an SSE stream into Frames.
type Reader struct {
	scanner *bufio.Scanner
	frame   Frame
}

// NewReader wraps r. The caller owns closing r.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	return &Reader{scanner: sc}
}

// ErrMalformed marks a structurally invalid SSE stream (not a transport error).
var ErrMalformed = errors.New("sse: malformed stream")

// Next returns the next complete frame. Returns io.EOF when the stream ends.
// Comment lines (":"), BOM, and CRLF are handled per the WHATWG spec.
//
// A frame whose data payload is not valid UTF-8 is rejected with
// ErrMalformed. A vendor that decodes its own SSE with a broken tokenizer
// can splice invalid bytes inside a JSON string; Go's encoding/json would
// silently substitute U+FFFD and the client would frame and persist mojibake
// (the thinking box, for english/mandarin/vietnamese text alike — live
// 2026-09-20). Rejecting here, before any JSON decode, lets the adapter's
// ErrMalformedStream retry ladder treat the bad bytes as a transient stream
// instead of real output.
func (r *Reader) Next(ctx context.Context) (Frame, error) {
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	var ev string
	var data bytes.Buffer
	for r.scanner.Scan() {
		line := r.scanner.Text()
		// Strip UTF-8 BOM on first line.
		line = strings.TrimPrefix(line, "\xEF\xBB\xBF")
		switch {
		case line == "":
			if data.Len() == 0 && ev == "" {
				continue // empty event; dispatch nothing
			}
			if !utf8.ValidString(data.String()) {
				return Frame{}, fmt.Errorf("%w: data payload is not valid UTF-8", ErrMalformed)
			}
			r.frame = Frame{Event: ev, Data: data.String()}
			return r.frame, nil
		case strings.HasPrefix(line, ":"):
			continue // comment
		}
		field, value, _ := cutField(line)
		// A per-line check catches the common case early; the payload
		// re-check catches a multi-byte rune split between two "data:"
		// lines of the same field (each line valid alone, the join not).
		if !utf8.ValidString(value) {
			return Frame{}, fmt.Errorf("%w: data line is not valid UTF-8", ErrMalformed)
		}
		switch field {
		case "event":
			ev = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			if !utf8.ValidString(data.String()) {
				return Frame{}, fmt.Errorf("%w: data payload is not valid UTF-8", ErrMalformed)
			}
		case "id", "retry":
			// Not used by any adapter; ignore.
		default:
			// Unknown field: ignore per spec.
		}
	}
	if err := r.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Frame{}, fmt.Errorf("%w: line exceeds %d bytes", ErrMalformed, MaxLineBytes)
		}
		return Frame{}, err
	}
	// Final dispatch if the stream ended mid-event without a blank line.
	if data.Len() > 0 || ev != "" {
		if !utf8.ValidString(data.String()) {
			return Frame{}, fmt.Errorf("%w: data payload is not valid UTF-8", ErrMalformed)
		}
		return Frame{Event: ev, Data: data.String()}, io.EOF
	}
	return Frame{}, io.EOF
}

func cutField(line string) (field, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return line, "", true
	}
	value = strings.TrimPrefix(line[i+1:], " ") // single leading space stripped per spec
	return line[:i], value, true
}