package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSanitizeSchemaForOpenAIResponses pins the three Responses rewrites:
// oneOf -> anyOf, an object always carries properties, and regex lookarounds are
// removed.
func TestSanitizeSchemaForOpenAIResponses(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"properties":{
			"choice":{"oneOf":[{"type":"string"},{"type":"integer"}]},
			"name":{"type":"string","pattern":"^(?!tmp)(?<=a)[a-z]+$"}
		},
		"additionalProperties":false
	}`)
	got := decodeJSON(t, SanitizeSchemaForOpenAIResponses(raw))
	props, _ := got["properties"].(map[string]any)
	choice, _ := props["choice"].(map[string]any)
	if _, has := choice["oneOf"]; has {
		t.Fatalf("choice = %v, want oneOf rewritten", choice)
	}
	if branches, _ := choice["anyOf"].([]any); len(branches) != 2 {
		t.Fatalf("choice.anyOf = %v, want both branches kept", choice["anyOf"])
	}
	name, _ := props["name"].(map[string]any)
	if pat, _ := name["pattern"].(string); pat != "^[a-z]+$" {
		t.Fatalf("pattern = %q, want both lookarounds removed", name["pattern"])
	}
	// A nested object without properties gets an empty map.
	nested := decodeJSON(t, SanitizeSchemaForOpenAIResponses(json.RawMessage(`{"type":"object"}`)))
	if props, ok := nested["properties"].(map[string]any); !ok || len(props) != 0 {
		t.Fatalf("nested = %v, want an empty properties map", nested)
	}
}

// TestSanitizeSchemaForOpenAIResponsesRetypesRootUnions pins the live 400
// (xAI grok, 2026-09-17): a root object with anyOf[{required:[...]}] is
// rejected until every branch declares type "object". Nested unions and
// mixed-type unions stay as the tool wrote them.
func TestSanitizeSchemaForOpenAIResponsesRetypesRootUnions(t *testing.T) {
	ask := json.RawMessage(`{
		"type":"object",
		"properties":{"question":{"type":"string"},"questions":{"type":"array"}},
		"anyOf":[{"required":["question"]},{"required":["questions"]}]
	}`)
	got := decodeJSON(t, SanitizeSchemaForOpenAIResponses(ask))
	branches, _ := got["anyOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("anyOf = %v, want both branches kept", got["anyOf"])
	}
	for i, b := range branches {
		obj, _ := b.(map[string]any)
		if obj["type"] != "object" {
			t.Fatalf("branch %d = %v, want type object (the 400 is an untyped root union)", i, b)
		}
		if _, has := obj["required"]; !has {
			t.Fatalf("branch %d dropped required: %v", i, b)
		}
	}

	nested := json.RawMessage(`{"type":"object","properties":{"choice":{"anyOf":[{"required":["a"]},{"required":["b"]}]}}}`)
	n := decodeJSON(t, SanitizeSchemaForOpenAIResponses(nested))
	props, _ := n["properties"].(map[string]any)
	choice, _ := props["choice"].(map[string]any)
	inner, _ := choice["anyOf"].([]any)
	if len(inner) != 2 {
		t.Fatalf("nested anyOf = %v", choice)
	}
	first, _ := inner[0].(map[string]any)
	if _, has := first["type"]; has {
		t.Fatalf("nested branch was retyped: %v", first)
	}

	mixed := json.RawMessage(`{"type":"object","anyOf":[{"type":"string"},{"required":["x"]}]}`)
	m := decodeJSON(t, SanitizeSchemaForOpenAIResponses(mixed))
	mb, _ := m["anyOf"].([]any)
	untyped, _ := mb[1].(map[string]any)
	if _, has := untyped["type"]; has {
		t.Fatalf("mixed union must stay mixed, got %v", m["anyOf"])
	}
}

// TestSanitizeSchemaForOpenAIResponsesKeepsPatterns pins that a pattern with no
// lookaround (and escapes, classes) survives byte-for-byte.
func TestSanitizeSchemaForOpenAIResponsesKeepsPatterns(t *testing.T) {
	for _, pat := range []string{`^\d{4}-\d{2}$`, `^[a-z()\[]+$`, `^a\(b\)c$`} {
		raw := json.RawMessage(`{"type":"string","pattern":` + strconvQuote(pat) + `}`)
		got := decodeJSON(t, SanitizeSchemaForOpenAIResponses(raw))
		if out, _ := got["pattern"].(string); out != pat {
			t.Fatalf("pattern = %q, want %q preserved", out, pat)
		}
	}
}

// TestStrictModePipeline pins sanitize+enforce: optional properties become
// nullable, everything lands in required, objects close with
// additionalProperties:false, and non-structural keywords are dropped (with
// `default` spilled into the description).
func TestStrictModePipeline(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"a file","default":"/tmp/x","minLength":1},
			"bare":{"type":"string","default":"silently dropped"},
			"limit":{"type":"integer","description":"max rows (default: 10)"},
			"nested":{"type":"object","properties":{"deep":{"type":"string"}},"required":["deep"]},
			"tags":{"type":"array","items":{"type":"string"}}
		},
		"required":["path","nested"]
	}`)
	out, strict := AdaptSchemaForStrict(raw, true)
	if !strict {
		t.Fatal("strict = false, want enforcement to succeed")
	}
	got := decodeJSON(t, out)
	if got["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", got["additionalProperties"])
	}
	required, _ := got["required"].([]any)
	if len(required) != 5 {
		t.Fatalf("required = %v, want every property required", got["required"])
	}
	props, _ := got["properties"].(map[string]any)
	path, _ := props["path"].(map[string]any)
	if _, has := path["minLength"]; has {
		t.Fatalf("path = %v, want minLength stripped", path)
	}
	if desc, _ := path["description"].(string); desc != `a file (default: "/tmp/x")` {
		t.Fatalf("path.description = %q, want the default spilled", path["description"])
	}
	if bare, _ := props["bare"].(map[string]any); bare["description"] != nil || bare["default"] != nil {
		t.Fatalf("bare = %v, want the default dropped with nowhere to spill", bare)
	}
	limit, _ := props["limit"].(map[string]any)
	if branches, _ := limit["anyOf"].([]any); len(branches) != 2 {
		t.Fatalf("limit = %v, want the optional property wrapped as a nullable union", limit)
	} else if branch, _ := branches[1].(map[string]any); branch["type"] != "null" {
		t.Fatalf("limit.anyOf[1] = %v, want a null branch", branch)
	}
	nested, _ := props["nested"].(map[string]any)
	if nested["additionalProperties"] != false {
		t.Fatalf("nested = %v, want nested objects closed too", nested)
	}
	tags, _ := props["tags"].(map[string]any)
	if _, has := tags["anyOf"]; !has {
		t.Fatalf("tags = %v, want the optional array wrapped as a union", tags)
	}
}

// TestStrictModeEnvBypass pins the PI_NO_STRICT escape hatch.
func TestStrictModeEnvBypass(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	t.Setenv("PI_NO_STRICT", "1")
	if !StrictModeDisabled() {
		t.Fatal("StrictModeDisabled = false with PI_NO_STRICT=1")
	}
	out, strict := AdaptSchemaForStrict(raw, true)
	if strict {
		t.Fatal("strict = true, want the bypass to keep it false")
	}
	if _, has := decodeJSON(t, out)["additionalProperties"]; has {
		t.Fatal("schema = enforced, want sanitized-only under the bypass")
	}
	t.Setenv("PI_NO_STRICT", "0")
	if StrictModeDisabled() {
		t.Fatal("PI_NO_STRICT=0 must not disable strict mode")
	}
}

// TestStrictModeTypeUnionVariants pins the type-array expansion: one variant per
// member type with type-specific keywords pruned and the description hoisted.
func TestStrictModeTypeUnionVariants(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"properties":{
			"value":{
				"type":["object","null"],
				"description":"payload",
				"properties":{"a":{"type":"string"}},
				"items":{"type":"string"}
			}
		}
	}`)
	out, strict := AdaptSchemaForStrict(raw, true)
	if !strict {
		t.Fatal("strict = false, want enforcement to succeed")
	}
	value := decodeJSON(t, out)["properties"].(map[string]any)["value"].(map[string]any)
	if value["description"] != "payload" {
		t.Fatalf("value.description = %v, want the description hoisted onto the wrapper", value["description"])
	}
	branches, _ := value["anyOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("value.anyOf = %v, want one variant per type", value["anyOf"])
	}
	obj, _ := branches[0].(map[string]any)
	if _, has := obj["items"]; has {
		t.Fatalf("object variant = %v, want array keywords pruned", obj)
	}
	if _, has := obj["properties"]; !has {
		t.Fatalf("object variant = %v, want object keywords kept", obj)
	}
	nullBranch, _ := branches[1].(map[string]any)
	if nullBranch["type"] != "null" {
		t.Fatalf("null branch = %v", nullBranch)
	}
}

// TestSanitizeSchemaForGoogle pins the Gemini subset: unsupported keywords
// dropped, `null` unions collapsed to nullable, arrays given items, refs
// inlined, and snake_case combinators camelized.
func TestSanitizeSchemaForGoogle(t *testing.T) {
	raw := json.RawMessage(`{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"$defs":{"file":{"type":"string","format":"path"}},
		"type":"object",
		"properties":{
			"path":{"$ref":"#/$defs/file","description":"a file"},
			"count":{"type":["integer","null"],"default":3},
			"items":{"type":"array"},
			"mode":{"any_of":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"string"}}}]},
			"fallback":{"anyOf":[{"type":"string"},{"type":"integer"}]}
		},
		"additionalProperties":false
	}`)
	got := decodeJSON(t, NormalizeSchemaForGoogle(raw))
	if _, has := got["$schema"]; has {
		t.Fatalf("schema = %v, want meta keys dropped", got)
	}
	if _, has := got["$defs"]; has {
		t.Fatalf("schema = %v, want $defs dropped", got)
	}
	if _, has := got["additionalProperties"]; has {
		t.Fatalf("schema = %v, want additionalProperties dropped for Gemini", got)
	}
	props, _ := got["properties"].(map[string]any)
	path, _ := props["path"].(map[string]any)
	if path["type"] != "string" || path["format"] != "path" {
		t.Fatalf("path = %v, want the $ref inlined with siblings kept", path)
	}
	if path["description"] != "a file" {
		t.Fatalf("path.description = %v, want the sibling description to win", path["description"])
	}
	count, _ := props["count"].(map[string]any)
	if count["type"] != "integer" || count["nullable"] != true {
		t.Fatalf("count = %v, want nullable integer", count)
	}
	items, _ := props["items"].(map[string]any)
	if _, has := items["items"]; !has {
		t.Fatalf("items = %v, want array items synthesized", items)
	}
	mode, _ := props["mode"].(map[string]any)
	if _, has := mode["anyOf"]; has {
		t.Fatalf("mode = %v, want object-only branches merged", mode)
	}
	if p2, _ := mode["properties"].(map[string]any); len(p2) != 2 {
		t.Fatalf("mode.properties = %v, want both branch properties merged", mode["properties"])
	}
	fallback, _ := props["fallback"].(map[string]any)
	if branches, _ := fallback["anyOf"].([]any); len(branches) != 2 {
		t.Fatalf("fallback = %v, want mixed-type anyOf preserved for Gemini", fallback)
	}
}

// TestNormalizeSchemaForCCA pins the validate-and-fallback path: a construct
// Claude cannot express collapses to a permissive object schema.
func TestNormalizeSchemaForCCA(t *testing.T) {
	raw := json.RawMessage(`{"type":["string","null"],"properties":{"a":{"type":"string"}}}`)
	got := string(NormalizeSchemaForCCA(raw))
	if !strings.Contains(got, `"type":"object"`) || !strings.Contains(got, `"properties":{}`) {
		t.Fatalf("schema = %s, want the permissive object fallback", got)
	}
	// Supported JSON Schema keywords survive for Claude; draft-only ones do not.
	ok := NormalizeSchemaForCCA(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","minLength":2,"patternProperties":{"x":{"type":"string"}}}},"if":{"type":"object","required":["a"]}}`))
	got = string(ok)
	if !strings.Contains(got, "minLength") {
		t.Fatalf("schema = %s, want supported keywords kept", got)
	}
	if strings.Contains(got, "patternProperties") || strings.Contains(got, `"if"`) {
		t.Fatalf("schema = %s, want draft-only keywords dropped", got)
	}
}

// TestNormalizeSchemaForMCP pins the object-root coercion.
func TestNormalizeSchemaForMCP(t *testing.T) {
	got := decodeJSON(t, NormalizeSchemaForMCP(json.RawMessage(`{"type":"string"}`)))
	props, ok := got["properties"].(map[string]any)
	if !ok || len(props) != 0 {
		t.Fatalf("schema = %v, want an object root with properties", got)
	}
	if got["type"] != "object" {
		t.Fatalf("type = %v, want object", got["type"])
	}
}

// TestSanitizeSchemaForOllama pins the three Go-parser rewrites.
func TestSanitizeSchemaForOllama(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"a":{"type":["string","null"]},
			"b":true,
			"c":{"items":{"type":"string"}}
		}
	}`)
	got := decodeJSON(t, SanitizeSchemaForOllama(raw))
	if _, isBool := got["additionalProperties"].(bool); isBool {
		t.Fatalf("additionalProperties = %v, want schema form", got["additionalProperties"])
	}
	props, _ := got["properties"].(map[string]any)
	if props["b"] == true {
		t.Fatalf("b = %v, want the boolean subschema widened", props["b"])
	}
	a, _ := props["a"].(map[string]any)
	if _, isArr := a["type"].([]any); isArr {
		t.Fatalf("a = %v, want the type array collapsed", a)
	}
	if a["type"] != "string" {
		t.Fatalf("a = %v, want the non-null type kept", a)
	}
}

// TestSanitizeSchemaForGrammarKeepsOpenness pins that boolean openness survives
// for grammar backends while boolean subschemas widen.
func TestSanitizeSchemaForGrammarKeepsOpenness(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","additionalProperties":false,"unevaluatedProperties":true,"properties":{"a":true}}`)
	got := decodeJSON(t, SanitizeSchemaForGrammar(raw))
	if got["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want the boolean preserved", got["additionalProperties"])
	}
	if got["unevaluatedProperties"] != true {
		t.Fatalf("unevaluatedProperties = %v, want the boolean preserved", got["unevaluatedProperties"])
	}
	if props, _ := got["properties"].(map[string]any); props["a"] == true {
		t.Fatalf("a = %v, want the boolean subschema widened", props["a"])
	}
}

// TestNormalizeSchemaForAPI pins the transport -> dispatcher table.
func TestNormalizeSchemaForAPI(t *testing.T) {
	responsesRaw := json.RawMessage(`{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"null"}]}}}`)
	googleRaw := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"a":{"type":["string","null"]}}}`)
	cases := []struct {
		api      string
		raw      json.RawMessage
		contains string
		absent   string
	}{
		{APIOpenAIResponses, responsesRaw, `"anyOf"`, `"oneOf"`},
		{APIOpenAICodexResponses, responsesRaw, `"anyOf"`, `"oneOf"`},
		{APIAzureOpenAIResponses, responsesRaw, `"anyOf"`, `"oneOf"`},
		{APIGoogleVertex, googleRaw, `"nullable":true`, `"additionalProperties"`},
		{APIGeminiCLI, googleRaw, `"nullable":true`, `"additionalProperties"`},
		{APIAnthropicMessages, googleRaw, `"additionalProperties"`, ``},
	}
	for _, tc := range cases {
		got := string(NormalizeSchemaForAPI(tc.api, tc.raw))
		if !strings.Contains(got, tc.contains) {
			t.Fatalf("%s: schema = %s, want %s", tc.api, got, tc.contains)
		}
		if tc.absent != "" && strings.Contains(got, tc.absent) {
			t.Fatalf("%s: schema = %s, want no %s", tc.api, got, tc.absent)
		}
	}
}

// TestNormalizeSchemaForFlavor pins the host flavor dispatch.
func TestNormalizeSchemaForFlavor(t *testing.T) {
	raw := json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"a":{"type":["string","null"],"minLength":2}},"if":{"type":"object"},"additionalProperties":true}`)
	grammar := decodeJSON(t, NormalizeSchemaForFlavor(ToolSchemaFlavorGrammar, json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"a":true}}`)))
	if props, _ := grammar["properties"].(map[string]any); props["a"] == true {
		t.Fatalf("grammar flavor = %v, want boolean subschemas widened", grammar)
	}
	if grammar["additionalProperties"] != false {
		t.Fatalf("grammar flavor = %v, want boolean openness preserved", grammar)
	}
	if got := string(NormalizeSchemaForFlavor(ToolSchemaFlavorNative, raw)); got != string(raw) {
		t.Fatalf("native flavor = %s, want passthrough", got)
	}
	moonshot := string(NormalizeSchemaForFlavor(ToolSchemaFlavorMoonshotMFJS, raw))
	for _, unwanted := range []string{"$schema", `"if"`, `["string","null"]`} {
		if strings.Contains(moonshot, unwanted) {
			t.Fatalf("moonshot flavor = %s, want %s dropped", moonshot, unwanted)
		}
	}
}

// strconvQuote quotes a Go string for embedding in a JSON literal.
func strconvQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
