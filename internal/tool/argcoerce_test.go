package tool

// Regression tests for the in-band tool-argument bug batch (2026-10-02). The
// seven live failures were all one root cause: a model reading a schema it is
// not constrained by sends a flattened array or a quoted scalar, and the
// tools' plain decode refuses. These pin both shapes, and — just as
// important — pin that nothing else is rewritten.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoerceArgsFlattensSingleKeyArrayWrapper(t *testing.T) {
	// The exact live failure: {"ids":{"item":["hub-1",...]}} against a
	// declared {"type":"array"} and, in the same call, "600" for a number.
	schema := json.RawMessage(`{"type":"object","properties":{
		"op":{"type":"string"},
		"ids":{"type":"array","items":{"type":"string"}},
		"timeout":{"type":"number"}}}`)
	in := json.RawMessage(`{"op":"wait","ids":{"item":["hub-1","hub-2"]},"timeout":"600"}`)
	got := string(CoerceArgs(schema, in))
	want := `{"ids":["hub-1","hub-2"],"op":"wait","timeout":600}`
	if got != want {
		t.Fatalf("coerced = %s, want %s", got, want)
	}
	var back struct {
		Op      string   `json:"op"`
		IDs     []string `json:"ids"`
		Timeout float64  `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("coerced args do not decode: %v", err)
	}
	if len(back.IDs) != 2 || back.IDs[0] != "hub-1" || back.Timeout != 600 {
		t.Fatalf("decode = %+v", back)
	}
}

func TestCoerceArgsIntegerStaysInteger(t *testing.T) {
	// A quoted integer must not come back as 20.0: the tool reads it into an
	// int field, and a float there is a different value on the wire.
	schema := json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}}}`)
	if got := string(CoerceArgs(schema, json.RawMessage(`{"limit":"20"}`))); got != `{"limit":20}` {
		t.Fatalf("coerced = %s, want {limit:20}", got)
	}
}

func TestCoerceArgsRepairsNestedItems(t *testing.T) {
	// task's batch shape: an array of objects whose own numeric field is
	// quoted. One rule, applied at depth.
	schema := json.RawMessage(`{"type":"object","properties":{"tasks":{"type":"array","items":{
		"type":"object","properties":{"max_turns":{"type":"integer"},"name":{"type":"string"}}}}}}`)
	in := json.RawMessage(`{"tasks":[{"name":"a","max_turns":"25"},{"name":"b","max_turns":"30"}]}`)
	got := string(CoerceArgs(schema, in))
	if strings.Contains(got, `"25"`) || strings.Contains(got, `"30"`) {
		t.Fatalf("nested quoted integer survived: %s", got)
	}
	var back struct {
		Tasks []struct {
			Name     string `json:"name"`
			MaxTurns int    `json:"max_turns"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(back.Tasks) != 2 || back.Tasks[0].MaxTurns != 25 || back.Tasks[1].MaxTurns != 30 {
		t.Fatalf("nested coercion = %+v", back.Tasks)
	}
}

func TestCoerceArgsLeavesCorrectArgsByteIdentical(t *testing.T) {
	// No rewrite means no re-marshal: a value the tool reads as a string
	// must not acquire float formatting or key reordering on the way past.
	schema := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
	in := json.RawMessage(`{"command":"printf 'v=1.0'"}`)
	if got := CoerceArgs(schema, in); string(got) != string(in) {
		t.Fatalf("correct args were rewritten: %s -> %s", in, got)
	}
}

func TestCoerceArgsRefusesAmbiguousShapes(t *testing.T) {
	// The repair must never invent. Each of these stays as sent so the tool
	// answers with its own honest error.
	cases := []struct{ name, schema, in string }{
		{"two-key wrapper", `{"type":"object","properties":{"ids":{"type":"array"}}}`, `{"ids":{"item":["a"],"other":"x"}}`},
		{"non-numeric string", `{"type":"object","properties":{"timeout":{"type":"number"}}}`, `{"timeout":"soon"}`},
		{"wrapper key is not an array", `{"type":"object","properties":{"ids":{"type":"array"}}}`, `{"ids":{"item":"hub-1"}}`},
		{"wrapper holding an object", `{"type":"object","properties":{"task":{"type":"array"}}}`, `{"task":{"item":{"name":"a"}}}`},
		{"unknown key", `{"type":"object","properties":{"path":{"type":"string"}}}`, `{"offset":"3"}`},
		{"empty string for number", `{"type":"object","properties":{"n":{"type":"number"}}}`, `{"n":"  "}`},
		{"number for an array", `{"type":"object","properties":{"ids":{"type":"array"}}}`, `{"ids":"hub-1"}`},
		{"non-integer quoted integer", `{"type":"object","properties":{"n":{"type":"integer"}}}`, `{"n":"3.5"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := json.RawMessage(tc.in)
			if got := CoerceArgs(json.RawMessage(tc.schema), in); string(got) != string(in) {
				t.Fatalf("ambiguous args were rewritten: %s -> %s", tc.in, got)
			}
		})
	}
}

func TestCoerceArgsFlattensArrayOfObjects(t *testing.T) {
	// task's own batch shape with undeclared items: the wrapper is still
	// exactly one key holding one array, so it is the same flattened
	// spelling — the element walk is simply skipped when no items schema is
	// declared.
	schema := json.RawMessage(`{"type":"object","properties":{"tasks":{"type":"array"}}}`)
	in := json.RawMessage(`{"tasks":{"item":[{"name":"a"},{"name":"b"}]}}`)
	want := `{"tasks":[{"name":"a"},{"name":"b"}]}`
	if got := string(CoerceArgs(schema, in)); got != want {
		t.Fatalf("coerced = %s, want %s", got, want)
	}
}

func TestCoerceArgsLeavesNonObjectsAndJunkAlone(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	for _, in := range []string{`"nope"`, `null`, `{not json`, `{}`} {
		if got := CoerceArgs(schema, json.RawMessage(in)); string(got) != in {
			t.Fatalf("%s was rewritten to %s", in, got)
		}
	}
}
