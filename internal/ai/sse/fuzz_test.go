package sse

import (
	"encoding/json"
	"testing"
)

// FuzzParsePartial feeds the relaxed JSON repair parser (M8): it consumes
// truncated model output, so arbitrary prefixes of any JSON value must
// either repair into valid JSON or return an error — never panic or emit
// a payload that is not syntactically valid JSON.
func FuzzParsePartial(f *testing.F) {
	f.Add(`{"a":1}`)
	f.Add(`{"a":`)
	f.Add(`{"tool":"write","args":{"path":"x`)
	f.Add(`[1,2,3`)
	f.Add(`"unterminated`)
	f.Add(`tru`)
	f.Add(`{"nested":{"deep":[{"x":`)
	f.Add(`\`)

	f.Fuzz(func(t *testing.T, data string) {
		out, err := ParsePartial(data)
		if err != nil {
			return // an unrepairable prefix is a fine outcome
		}
		if len(out) == 0 {
			return
		}
		// Syntactic validity is the contract. Decoding into `any` is the
		// wrong check: a huge numeric literal is valid JSON that Go
		// refuses to narrow to float64 (the fuzzer found exactly that).
		if !json.Valid(out) {
			t.Fatalf("repaired output is not valid JSON: %q -> %q", data, out)
		}
	})
}
