package acp

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	bodies := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`),
		[]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"a":"é✓"}}`),
		{},
	}
	for _, body := range bodies {
		if err := writeFrame(&buf, body); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
	}
	r := bufio.NewReader(&buf)
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

func TestReadFrameHeaders(t *testing.T) {
	// Case-insensitive name, unknown headers skipped, LF-only line endings.
	body := `{"ok":true}`
	raw := "x-note: ignored\ncontent-length: " + strconv.Itoa(len(body)) + "\n\n" + body
	got, err := readFrame(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestReadFrameRejectsBadLength(t *testing.T) {
	for name, raw := range map[string]string{
		"missing": "x: 1\r\n\r\n{}",
		"garbage": "Content-Length: abc\r\n\r\n{}",
		"huge":    "Content-Length: " + strconv.Itoa(maxFrame+1) + "\r\n\r\n{}",
	} {
		if _, err := readFrame(bufio.NewReader(strings.NewReader(raw))); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
