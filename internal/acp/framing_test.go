package acp

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// ACP frames are newline-delimited JSON (parity finding T5). These tests pin
// the transport, not just the codec: an editor must be able to read xdev's
// stdout line by line, and xdev must tolerate what editors send.

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	bodies := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`),
		[]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"a":"é✓"}}`),
	}
	for _, body := range bodies {
		if err := writeFrame(&buf, body); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
	}
	// The wire form is body + "\n" and nothing else — no header block.
	if n := strings.Count(buf.String(), "\n"); n != len(bodies) {
		t.Fatalf("%d newlines for %d frames: %q", n, len(bodies), buf.String())
	}
	if strings.Contains(buf.String(), "Content-Length") {
		t.Fatalf("LSP header leaked into the ACP transport: %q", buf.String())
	}
	r := newReader(&buf)
	for i, want := range bodies {
		got, err := readFrame(r)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %q, want %q", i, got, want)
		}
	}
	if _, err := readFrame(r); err != io.EOF {
		t.Fatalf("end of stream = %v, want EOF", err)
	}
}

// A peer that separates messages with blank lines, or ends the stream without
// a trailing newline, is still read correctly.
func TestReadFrameToleratesPeerHabits(t *testing.T) {
	raw := "\r\n{\"a\":1}\n\n{\"b\":2}"
	r := newReader(strings.NewReader(raw))
	first, err := readFrame(r)
	if err != nil || string(first) != `{"a":1}` {
		t.Fatalf("first = %q %v", first, err)
	}
	second, err := readFrame(r)
	if err != nil || string(second) != `{"b":2}` {
		t.Fatalf("last (no trailing newline) = %q %v", second, err)
	}
}

// An over-long line is refused before it is fully buffered: the reader must
// stop at maxFrame rather than assembling an arbitrarily large message (the
// old ReadString-based loop grew its own buffer for any line a peer sent).
func TestReadFrameSizeCap(t *testing.T) {
	huge := strings.Repeat("{", maxFrame+64) // no newline: one endless frame
	_, err := readFrame(newReader(strings.NewReader(huge)))
	if err == nil {
		t.Fatal("an over-long frame must error")
	}
	if !errors.Is(err, errFrameTooLong) {
		t.Fatalf("err = %v, want the size-cap error", err)
	}
}

// A frame just under the cap still reads, so the guard is not a size limit in
// disguise for legitimate large tool outputs.
func TestReadFrameUnderCap(t *testing.T) {
	body := `{"x":"` + strings.Repeat("y", 200<<10) + `"}`
	got, err := readFrame(newReader(strings.NewReader(body + "\n")))
	if err != nil {
		t.Fatalf("a %d KB frame must read: %v", len(body)>>10, err)
	}
	if string(got) != body {
		t.Fatalf("frame mangled: %d bytes", len(got))
	}
}
