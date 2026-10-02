package tool

// Schema-driven argument coercion (M14 follow-up to the in-band tool-arg bug
// batch). A model that reads a JSON Schema but is not constrained by it (every
// non-strict wire: openai-completions, Azure, and every gateway that proxies
// them) sends two shapes the schema forbids and that the tools' plain
// json.Unmarshal then rejects outright:
//
//   - a flattened array: {"ids":{"item":["a","b"]}} instead of
//     {"ids":["a","b"]}. OpenAI structured outputs serialize a bare
//     {"type":"array"} as an object with a single "item" key, and a model
//     trained on that shape emits it even with no strict enforcement.
//   - a quoted scalar: {"timeout":"600"} for a declared number.
//
// Both were observed live on the hub tool (2026-10-02): seven retries of the
// same wait call, every one refused, because ids was an object and timeout was
// a string. The github tool's limit field failed the same way the day before.
//
// Coerce runs at the one chokepoint every tool call passes through, so a
// direct call, a tool_call bridge, and an eval-kernel cell are all covered by
// the same rules. It reshapes only what is unambiguous — never invents,
// never reorders, never guesses a value the model did not send — so a call
// that is genuinely wrong still reaches the tool and still gets the tool's own
// honest required-argument error.
//
// ponytail: no per-tool opt-in and no second schema walk. If a provider shape
// shows up that this cannot repair unambiguously, the upgrade path is one more
// case in coerceScalar, not a new layer.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// maxCoerceDepth bounds the recursion. Tool schemas are shallow by
// construction (task nests two levels); the cap only exists so a
// self-referential $ref cannot spin.
const maxCoerceDepth = 8

// CoerceArgs repairs raw tool-call arguments against the tool's declared
// JSON Schema, returning args unchanged when nothing needs repair. A
// non-object argument, an unparseable schema, or an unparseable argument blob
// is returned as-is: the tool's own decoder owns those errors and its message
// is more useful than anything guessed here.
func CoerceArgs(schema, args json.RawMessage) json.RawMessage {
	if len(schema) == 0 || len(args) == 0 {
		return args
	}
	spec, ok := specAsObject(schema)
	if !ok {
		return args
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber() // an integer must not round-trip through float64
	if dec.Decode(&v) != nil {
		return args
	}
	out, changed := coerceNode(spec, v, 0)
	if !changed {
		return args
	}
	// json.Number marshals verbatim, so an integer stays an integer rather
	// than becoming 600.0 on the way back out.
	enc, err := json.Marshal(out)
	if err != nil {
		return args
	}
	return enc
}

// coerceNode rewrites v against the schema node spec. changed reports whether
// anything was rewritten, so an untouched payload keeps its original bytes (no
// re-marshal, no key reordering, no float formatting drift on a value the tool
// reads as a string).
func coerceNode(spec map[string]json.RawMessage, v any, depth int) (any, bool) {
	if depth > maxCoerceDepth {
		return v, false
	}
	// The declared type decides first, not the value's Go type: a node
	// declared "array" whose value arrived as an object IS the flattened-
	// wrapper bug, so it has to reach the scalar repair rather than the object
	// walk. Symmetrically a node declared "object" is never a scalar to mend.
	switch specType(spec) {
	case "array", "number", "integer", "boolean", "string":
		return coerceScalar(spec, v, depth)
	}
	switch node := v.(type) {
	case map[string]any:
		props, ok := specObject(spec, "properties")
		if !ok {
			return v, false
		}
		changed := false
		for key, val := range node {
			child, ok := specAsObject(props[key])
			if !ok {
				continue // undeclared key: additionalProperties, the tool decides
			}
			fixed, ch := coerceNode(child, val, depth+1)
			if ch {
				node[key] = fixed
				changed = true
			}
		}
		return v, changed
	case []any:
		child, ok := specAsObject(specMember(spec, "items"))
		if !ok {
			return v, false
		}
		changed := false
		for i, val := range node {
			fixed, ch := coerceNode(child, val, depth+1)
			if ch {
				node[i] = fixed
				changed = true
			}
		}
		return v, changed
	}
	return v, false
}

// coerceScalar repairs a single value whose declared type disagrees with the
// one the model sent. Every case is a total function of what arrived: a string
// that is exactly a number is that number, and an object that is exactly one
// array under a known flattening key is that array. Anything else is left for
// the tool to reject.
func coerceScalar(spec map[string]json.RawMessage, v any, depth int) (any, bool) {
	switch specType(spec) {
	case "array":
		obj, ok := v.(map[string]any)
		if !ok || len(obj) != 1 {
			// An array that already arrived as an array needs only its items
			// walked; anything else (two-key object, a number, a string) is
			// not an unambiguous unwrap.
			arr, isArr := v.([]any)
			if !isArr {
				return v, false
			}
			return coerceItems(spec, arr, depth)
		}
		for _, key := range flattenKeys {
			arr, ok := obj[key].([]any)
			if !ok {
				continue
			}
			if fixed, ch := coerceItems(spec, arr, depth); ch {
				return fixed, true
			}
			return arr, true
		}
		return v, false
	case "number", "integer":
		s, ok := v.(string)
		if !ok {
			return v, false
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return v, false
		}
		if specType(spec) == "integer" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return v, false
			}
			return json.Number(strconv.FormatInt(n, 10)), true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return v, false
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), true
	case "boolean":
		s, ok := v.(string)
		if !ok {
			return v, false
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return v, false
	}
	return v, false
}

// coerceItems applies the node's item schema to every element.
func coerceItems(spec map[string]json.RawMessage, arr []any, depth int) (any, bool) {
	child, ok := specAsObject(specMember(spec, "items"))
	if !ok {
		return arr, false
	}
	changed := false
	for i, el := range arr {
		if fixed, ch := coerceNode(child, el, depth+1); ch {
			arr[i] = fixed
			changed = true
		}
	}
	return arr, changed
}

// flattenKeys are the single-key object wrappers a model uses for a bare
// array. "item" is the OpenAI structured-output spelling and the one observed
// in the field; the rest are cheap synonyms, tried only when the object has
// exactly one key so an ordinary argument object can never be unwrapped.
var flattenKeys = []string{"item", "items", "value", "values"}

// specMember returns a named schema member, or nil when absent.
func specMember(spec map[string]json.RawMessage, key string) json.RawMessage {
	return spec[key]
}

// specObject returns the named schema member decoded as an object node. A
// missing member, a boolean subschema, or any other shape is not a node this
// walker can descend into, so it reports false and the value is left for the
// tool to reject on its own terms.
func specObject(spec map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	return specAsObject(specMember(spec, key))
}

// specAsObject decodes a raw schema member as an object node.
func specAsObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, false
	}
	return out, true
}

// specType returns the node's declared type, tolerating both the plain string
// and the type-union array. A union takes its first non-null member: that is
// the type the value must satisfy to be usable, and repairing toward it is
// strictly better than refusing.
func specType(spec map[string]json.RawMessage) string {
	raw := specMember(spec, "type")
	if raw == nil {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return ""
	}
	for _, t := range list {
		if t != "null" && t != "" {
			return t
		}
	}
	return ""
}
