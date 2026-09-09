package sse

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestReaderBasicDispatch(t *testing.T) {
	stream := "data: {\"a\":1}\n\n" +
		"event: delta\ndata: \"x\"\ndata: \"y\"\n\n" +
		": keep-alive\n\n" +
		"data: [DONE]\n\n"
	r := NewReader(strings.NewReader(stream))
	ctx := context.Background()

	f, err := r.Next(ctx)
	if err != nil || f.Data != `{"a":1}` || f.Event != "" {
		t.Fatalf("frame 1 = %+v, %v", f, err)
	}
	f, err = r.Next(ctx)
	if err != nil || f.Event != "delta" || f.Data != "\"x\"\n\"y\"" {
		t.Fatalf("frame 2 = %+v, %v", f, err)
	}
	f, err = r.Next(ctx)
	if err != nil || !f.IsDone() {
		t.Fatalf("frame 3 = %+v, %v", f, err)
	}
	if _, err := r.Next(ctx); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

func TestReaderEOFMidEvent(t *testing.T) {
	r := NewReader(strings.NewReader("data: tail"))
	f, err := r.Next(context.Background())
	if err != io.EOF || f.Data != "tail" {
		t.Fatalf("got %+v, %v", f, err)
	}
}

func TestReaderCRLFAndBOM(t *testing.T) {
	stream := "\xEF\xBB\xBFdata: one\r\n\r\ndata: two\r\n\r\n"
	r := NewReader(strings.NewReader(stream))
	f, err := r.Next(context.Background())
	if err != nil || f.Data != "one" {
		t.Fatalf("frame 1 = %+v, %v", f, err)
	}
}

func TestParsePartialTruncations(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // JSON of the repaired output
	}{
		{"empty object", `{`, `{}`},
		{"half key", `{"pat`, `{}`},
		{"key no value", `{"path":`, `{}`},
		{"string value cut", `{"path":"/tm`, `{"path":"/tm"}`},
		{"nested cut", `{"a":{"b":[1,2`, `{"a":{"b":[1,2]}}`},
		{"dangling comma", `{"a":1,`, `{"a":1}`},
		{"dangling escape", `{"s":"a\`, `{"s":"a"}`},
		{"array of objects cut", `{"files":[{"path":"x"},{"path":"y"`, `{"files":[{"path":"x"},{"path":"y"}]}`},
		{"complete", `{"n":42}`, `{"n":42}`},
		{"true cut", `{"f":tru`, `{"f":null}`},
		{"number cut exp", `{"x":1.5e`, `{"x":1.5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePartial(tc.in)
			if err != nil {
				t.Fatalf("ParsePartial(%q): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("ParsePartial(%q) = %s, want %s", tc.in, got, tc.want)
			}
			// The repaired value must be strictly valid JSON.
			var v any
			if err := json.Unmarshal(got, &v); err != nil {
				t.Fatalf("repaired output invalid: %v", err)
			}
		})
	}
}

func TestParsePartialRealToolArgs(t *testing.T) {
	// Streaming deltas of a realistic read call, each must repair.
	full := `{"i":"Reading config","path":"/etc/app/config.toml"}`
	for i := range len(full) {
		frag := full[:i]
		if strings.TrimSpace(frag) == "" {
			continue
		}
		got, err := ParsePartial(frag)
		if err != nil {
			t.Fatalf("prefix %d %q: %v", i, frag, err)
		}
		if !json.Valid(got) {
			t.Fatalf("prefix %d produced invalid JSON: %s", i, got)
		}
	}
	// The complete prefix parses strictly and matches.
	got, err := ParsePartial(full)
	if err != nil || string(got) != full {
		t.Fatalf("full parse = %s, %v", got, err)
	}
}
