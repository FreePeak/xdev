package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Provider tool-schema normalization (M14 #62).
//
// One option-driven walker serves every provider dispatcher: each transport
// pins a schemaOptions set, walks the tool's JSON Schema, and returns the shape
// that provider accepts on the wire. Adding a provider means adding an option
// set, never another walker.
//
// The transport -> dispatcher mapping (NormalizeSchemaForAPI) mirrors omp's
// packages/ai/src/utils/schema dispatcher table:
//
//	openai-completions|openai-responses|openai-codex-responses|azure-openai-responses
//	    -> SanitizeSchemaForOpenAIResponses (+ strict-mode adaptation at the call site)
//	google-generative-ai|google-vertex|gemini-cli -> NormalizeSchemaForGoogle
//	anthropic-messages (Cloud Code Assist Claude)  -> NormalizeSchemaForCCA
//	Moonshot/Kimi native hosts (MFJS)              -> NormalizeSchemaForMoonshot
//	grammar-flavored OpenAI-compatible hosts       -> SanitizeSchemaForGrammar
//	Ollama tool parameters                         -> SanitizeSchemaForOllama
//	MCP inputSchema ingestion                      -> NormalizeSchemaForMCP

// --- dispatcher options ---

// spillMode selects which dropped keywords survive as description text.
type spillMode int

const (
	// spillNone drops silently.
	spillNone spillMode = iota
	// spillDefault lifts only `default`, as " (default: X)" (OpenAI strict mode).
	spillDefault
	// spillHuman lifts every human-meaningful keyword (Gemini, CCA).
	spillHuman
)

// schemaOptions is the per-dispatcher option set the shared walker runs with.
type schemaOptions struct {
	// drop lists keyword names the target provider rejects.
	drop []string
	// spill controls which dropped keywords are lifted into the sibling
	// description instead of being lost.
	spill spillMode
	// nullableKey rewrites `type: ["T","null"]` to type "T" plus
	// `nullable: true` (Gemini's marker). Otherwise the union collapses to the
	// first non-null type and the null branch is dropped.
	nullableKey bool
	// collapseObjects merges `anyOf`/`oneOf` whose branches are all object
	// schemas into a single object schema (union of properties/required).
	collapseObjects bool
	// lossyCombos collapses mixed-type combiners by keeping the first branch's
	// keywords (Cloud Code Assist path).
	lossyCombos bool
	// fallback replaces the whole schema with {"type":"object","properties":{}}
	// when a residual construct the provider cannot represent survives the walk
	// (type array, `type: "null"`, `nullable`, any combinator).
	fallback bool
	// requireItems gives every array node an `items` schema.
	requireItems bool
	// exampleSingular renames `examples` to Gemini's singular `example`.
	exampleSingular bool
	// collapseTypes collapses `type: [T,"null"]` to the first non-null type for
	// providers whose `type` field is a single string (Moonshot, Ollama).
	// Google does the same via nullableKey.
	collapseTypes bool

	// typeSets expands `type: [T,U]` into one variant schema per type with
	// type-specific keywords pruned (the OpenAI strict-mode path).
	typeSets bool
	// ensureProps gives every object node a `properties` map (the Responses API
	// rejects an object schema without one).
	ensureProps bool
	// stripLookaround removes regex lookaround groups from `pattern`.
	stripLookaround bool
	// boolSubschemas rewrites boolean subschemas: true -> {}, false ->
	// {"not":{}} (Ollama's Go schema parser rejects booleans).
	boolSubschemas bool
	// openness rewrites boolean `additionalProperties`/`unevaluatedProperties`
	// to schema form ({}/{"not":{}}); keepOpenness preserves the booleans
	// (grammar backends need them).
	openness     bool
	keepOpenness bool
}

// Keyword groups.
var (
	schemaMetaKeys   = []string{"$schema", "$id", "$comment", "$anchor", "$dynamicAnchor", "$vocabulary", "$dynamicRef"}
	schemaDraftOnly  = []string{"if", "then", "else", "not", "dependentSchemas", "dependentRequired", "patternProperties", "propertyNames", "contains", "minContains", "maxContains", "prefixItems", "contentMediaType", "contentEncoding", "contentSchema", "unevaluatedItems", "unevaluatedProperties", "definitions", "$defs"}
	schemaSpillable  = []string{"format", "pattern", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties", "default", "examples", "defaultValue"}
	schemaContainers = []string{"items", "additionalProperties", "propertyNames", "contains", "not", "if", "then", "else", "unevaluatedProperties", "unevaluatedItems"}
)

// snakeCaseRenames maps draft/typed-style snake_case keys onto their camelCase
// spellings. python-genai semantics: the snake_case spelling wins on collision.
var snakeCaseRenames = map[string]string{
	"any_of":                 "anyOf",
	"one_of":                 "oneOf",
	"all_of":                 "allOf",
	"additional_properties":  "additionalProperties",
	"unevaluated_properties": "unevaluatedProperties",
	"unevaluated_items":      "unevaluatedItems",
	"pattern_properties":     "patternProperties",
	"property_names":         "propertyNames",
	"dependent_schemas":      "dependentSchemas",
	"dependent_required":     "dependentRequired",
	"prefix_items":           "prefixItems",
	"min_contains":           "minContains",
	"max_contains":           "maxContains",
	"min_length":             "minLength",
	"max_length":             "maxLength",
	"min_items":              "minItems",
	"max_items":              "maxItems",
	"min_properties":         "minProperties",
	"max_properties":         "maxProperties",
	"exclusive_minimum":      "exclusiveMinimum",
	"exclusive_maximum":      "exclusiveMaximum",
	"multiple_of":            "multipleOf",
	"unique_items":           "uniqueItems",
	"content_media_type":     "contentMediaType",
	"content_encoding":       "contentEncoding",
	"content_schema":         "contentSchema",
}

// --- dispatchers ---

// NormalizeSchemaForGoogle prepares a tool schema for google-generative-ai,
// google-vertex and gemini-cli. Gemini's Schema message has no
// additionalProperties, no $ref, no oneOf/allOf and no draft-only keywords; the
// dropped human-meaningful keywords are spilled into the description, nullable
// unions become `nullable: true`, and regex lookarounds are removed (RE2 backs
// Gemini's `pattern`).
func NormalizeSchemaForGoogle(raw json.RawMessage) json.RawMessage {
	return normalizeSchema(raw, schemaOptions{
		drop:            append(append([]string{}, schemaMetaKeys...), append(schemaDraftOnly, "additionalProperties", "uniqueItems", "exclusiveMinimum", "exclusiveMaximum", "multipleOf")...),
		spill:           spillHuman,
		nullableKey:     true,
		collapseObjects: true,
		requireItems:    true,
		exampleSingular: true,
		stripLookaround: true,
	})
}

// NormalizeSchemaForCCA prepares a tool schema for Cloud Code Assist Claude
// (Antigravity / GCA). Anything the walker cannot express in Claude's input
// schema falls back to a permissive object schema rather than being sent and
// rejected on the wire.
func NormalizeSchemaForCCA(raw json.RawMessage) json.RawMessage {
	return normalizeSchema(raw, schemaOptions{
		drop:            append(append([]string{}, schemaMetaKeys...), schemaDraftOnly...),
		spill:           spillHuman,
		collapseObjects: true,
		lossyCombos:     true,
		fallback:        true,
	})
}

// SanitizeSchemaForOpenAIResponses rewrites what the OpenAI Responses API
// rejects: `oneOf` becomes `anyOf`, object schemas always carry `properties`,
// and regex lookarounds are removed from `pattern` (RE2 has no lookarounds).
// Root anyOf/oneOf branches that are already object-compatible also gain an
// explicit `type:"object"` — xAI (and similar strict validators) 400 a tool
// whose parameters root is a union with an untyped branch (the ask tool's
// "one of these property sets" shape).
func SanitizeSchemaForOpenAIResponses(raw json.RawMessage) json.RawMessage {
	out := normalizeSchema(raw, schemaOptions{
		drop:            append(append([]string{}, schemaMetaKeys...), "definitions", "$defs"),
		ensureProps:     true,
		stripLookaround: true,
	})
	return retypeRootObjectUnions(out)
}

// NormalizeSchemaForMCP prepares an MCP tool inputSchema before it enters the
// registry: refs are inlined, meta keys are dropped, and the root is always an
// object schema (MCP requires `type: object`).
func NormalizeSchemaForMCP(raw json.RawMessage) json.RawMessage {
	out := normalizeSchema(raw, schemaOptions{
		drop: append(append([]string{}, schemaMetaKeys...), "definitions", "$defs"),
	})
	return ensureObjectRoot(out)
}

// NormalizeSchemaForMoonshot prepares a tool schema for Moonshot/Kimi native
// hosts, whose MFJS subset has no $ref, no meta keys and no draft-only
// keywords. Combiners collapse to the first branch instead of being sent.
func NormalizeSchemaForMoonshot(raw json.RawMessage) json.RawMessage {
	return normalizeSchema(raw, schemaOptions{
		drop:            append(append([]string{}, schemaMetaKeys...), schemaDraftOnly...),
		collapseObjects: true,
		lossyCombos:     true,
		collapseTypes:   true,
	})
}

// SanitizeSchemaForOllama rewrites the three constructs Ollama's Go schema
// parser rejects: boolean subschemas, type arrays, and boolean object-openness
// keywords.
func SanitizeSchemaForOllama(raw json.RawMessage) json.RawMessage {
	return normalizeSchema(raw, schemaOptions{
		drop:           append(append([]string{}, schemaMetaKeys...), "definitions", "$defs"),
		boolSubschemas: true,
		openness:       true,
		requireItems:   true,
		collapseTypes:  true,
	})
}

// SanitizeSchemaForGrammar widens boolean subschemas for grammar-constrained
// OpenAI-compatible backends while preserving boolean
// additionalProperties/unevaluatedProperties.
func SanitizeSchemaForGrammar(raw json.RawMessage) json.RawMessage {
	return normalizeSchema(raw, schemaOptions{
		boolSubschemas: true,
		keepOpenness:   true,
	})
}

// ToolSchemaFlavor names a host-specific tool-schema family (models.yml
// `toolSchemaFlavor`).
type ToolSchemaFlavor string

const (
	// ToolSchemaFlavorNative leaves the transport dispatcher in charge.
	ToolSchemaFlavorNative ToolSchemaFlavor = ""
	// ToolSchemaFlavorMoonshotMFJS pins Moonshot/Kimi's MFJS subset.
	ToolSchemaFlavorMoonshotMFJS ToolSchemaFlavor = "moonshot-mfjs"
	// ToolSchemaFlavorGrammar pins grammar-constrained OpenAI-compatible hosts.
	ToolSchemaFlavorGrammar ToolSchemaFlavor = "grammar"
)

// NormalizeSchemaForFlavor applies the host-specific flavor pinned by a
// models.yml provider (`toolSchemaFlavor`). Unknown/empty flavors are a no-op.
func NormalizeSchemaForFlavor(f ToolSchemaFlavor, raw json.RawMessage) json.RawMessage {
	switch f {
	case ToolSchemaFlavorMoonshotMFJS:
		return NormalizeSchemaForMoonshot(raw)
	case ToolSchemaFlavorGrammar:
		return SanitizeSchemaForGrammar(raw)
	default:
		return raw
	}
}

// NormalizeSchemaForAPI dispatches a tool schema by wire transport (the
// dispatcher table above). Transports with no normalizer (anthropic-messages,
// unknown) pass the schema through unchanged.
func NormalizeSchemaForAPI(api string, raw json.RawMessage) json.RawMessage {
	switch api {
	case APIOpenAICompletions, APIOpenAIResponses, APIOpenAICodexResponses, APIAzureOpenAIResponses:
		return SanitizeSchemaForOpenAIResponses(raw)
	case APIGoogleGenerativeAI, APIGoogleVertex, APIGeminiCLI:
		return NormalizeSchemaForGoogle(raw)
	default:
		return raw
	}
}

// NormalizeToolsForAPI runs the transport dispatcher over every tool schema
// (the request-encoding entry point used by the wire adapters).
func NormalizeToolsForAPI(api string, tools []ToolDef) []ToolDef {
	out := make([]ToolDef, len(tools))
	copy(out, tools)
	for i := range out {
		out[i].Parameters = NormalizeSchemaForAPI(api, out[i].Parameters)
	}
	return out
}

// --- the shared walker ---

func normalizeSchema(raw json.RawMessage, o schemaOptions) json.RawMessage {
	v, err := decodeSchemaValue(raw)
	if err != nil {
		return raw
	}
	out, err := walkSchema(v, v, &o, nil)
	if err != nil {
		return raw
	}
	if o.fallback && residualIncompatible(out) {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return encodeSchemaValue(out, raw)
}

// walkSchema normalizes one schema node. root resolves local $refs; refs
// guards recursive definitions (a cycle collapses to {} rather than looping).
func walkSchema(v, root any, o *schemaOptions, refs map[string]bool) (any, error) {
	switch node := v.(type) {
	case bool:
		if !o.boolSubschemas {
			return node, nil
		}
		if node {
			return map[string]any{}, nil
		}
		return map[string]any{"not": map[string]any{}}, nil
	case map[string]any:
		return walkSchemaObject(node, root, o, refs)
	case []any:
		out := make([]any, len(node))
		for i, el := range node {
			walked, err := walkSchema(el, root, o, refs)
			if err != nil {
				return nil, err
			}
			out[i] = walked
		}
		return out, nil
	default:
		return v, nil
	}
}

func walkSchemaObject(node map[string]any, root any, o *schemaOptions, refs map[string]bool) (any, error) {
	// 1. snake_case -> camelCase (snake_case wins on collision).
	for from, to := range snakeCaseRenames {
		if val, ok := node[from]; ok {
			delete(node, from)
			node[to] = val
		}
	}

	// 2. Inline a local $ref; sibling keys win over the resolved definition.
	if ref, ok := node["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#") {
			// Remote refs cannot be resolved offline: drop them.
			delete(node, "$ref")
		} else {
			if refs[ref] {
				return map[string]any{}, nil
			}
			target, ok := resolveJSONPointer(root, ref)
			if !ok {
				delete(node, "$ref")
			} else {
				next := make(map[string]bool, len(refs)+1)
				for k := range refs {
					next[k] = true
				}
				next[ref] = true
				resolved, err := walkSchema(target, root, o, next)
				if err != nil {
					return nil, err
				}
				base, ok := resolved.(map[string]any)
				if !ok {
					base = map[string]any{}
				}
				delete(node, "$ref")
				merged := make(map[string]any, len(base)+len(node))
				for k, val := range base {
					merged[k] = val
				}
				for k, val := range node {
					merged[k] = val
				}
				node = merged
			}
		}
	}
	delete(node, "$ref")

	// 3. const -> single-value enum.
	if c, ok := node["const"]; ok {
		delete(node, "const")
		if _, has := node["enum"]; !has {
			node["enum"] = []any{c}
		}
	}

	// 4. examples -> example (Gemini's singular field).
	if o.exampleSingular {
		if ex, ok := node["examples"].([]any); ok && len(ex) > 0 {
			if _, has := node["example"]; !has {
				node["example"] = ex[0]
			}
			delete(node, "examples")
		}
	}

	// 5. Type unions: strict mode expands them into variants, every other
	// provider gets a single collapsed type.
	if types, ok := node["type"].([]any); ok {
		switch {
		case o.typeSets:
			// Strict mode keeps the union but emits one variant per member type.
			return typeVariants(node, root, o, refs, types)
		case o.nullableKey || o.collapseTypes:
			node["type"] = collapseTypeUnion(node, types, o)
		default:
			// Providers whose validator rejects a type union (Cloud Code Assist)
			// need the union left in place for residualIncompatible to catch.
		}
	}

	// 6. Recurse into sub-schema containers.
	for _, key := range schemaContainers {
		child, ok := node[key]
		if !ok {
			continue
		}
		if key == "additionalProperties" || key == "unevaluatedProperties" {
			if b, isBool := child.(bool); isBool {
				if o.openness && !o.keepOpenness {
					node[key] = boolAsSchema(b)
				}
				continue
			}
		}
		walked, err := walkSchema(child, root, o, refs)
		if err != nil {
			return nil, err
		}
		node[key] = walked
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for name, sub := range props {
			walked, err := walkSchema(sub, root, o, refs)
			if err != nil {
				return nil, err
			}
			props[name] = walked
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := node[key].([]any)
		if !ok {
			continue
		}
		for i, br := range branches {
			walked, err := walkSchema(br, root, o, refs)
			if err != nil {
				return nil, err
			}
			branches[i] = walked
		}
		node[key] = branches
	}
	if _, ok := node["$defs"].(map[string]any); ok {
		delete(node, "$defs")
	}
	if _, ok := node["definitions"].(map[string]any); ok {
		delete(node, "definitions")
	}

	// 7. Combiners.
	if err := collapseCombiners(node, o); err != nil {
		return nil, err
	}

	// 8. Pattern lookarounds (RE2/OpenAI reject them).
	if o.stripLookaround {
		if pat, ok := node["pattern"].(string); ok {
			node["pattern"] = stripLookarounds(pat)
		}
	}

	// 9. Provider-required shapes.
	if o.requireItems && node["type"] == "array" {
		if _, ok := node["items"]; !ok {
			node["items"] = map[string]any{}
		}
	}
	if o.ensureProps && node["type"] == "object" {
		if _, ok := node["properties"]; !ok {
			node["properties"] = map[string]any{}
		}
	}

	// 10. Strip unsupported keywords, spilling what carries meaning.
	dropUnsupportedKeys(node, o)

	return node, nil
}

// collapseTypeUnion handles `type: ["T","null"]` (and wider unions) for
// providers whose `type` field is a single string.
func collapseTypeUnion(node map[string]any, types []any, o *schemaOptions) any {
	names := make([]string, 0, len(types))
	for _, t := range types {
		if s, ok := t.(string); ok {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return types
	}
	nonNull := ""
	for _, n := range names {
		if n != "null" {
			nonNull = n
			break
		}
	}
	if nonNull == "" {
		return "null"
	}
	if o.nullableKey {
		for _, n := range names {
			if n == "null" {
				node["nullable"] = true
				return nonNull
			}
		}
	}
	return nonNull
}

// collapseCombiners applies oneOf->anyOf rewriting, single-item allOf inlining,
// object-only combiner merging, and the lossy/drop fallbacks.
func collapseCombiners(node map[string]any, o *schemaOptions) error {
	if branches, ok := node["oneOf"].([]any); ok {
		other, hasAny := node["anyOf"]
		if hasAny {
			if list, ok := other.([]any); ok {
				node["anyOf"] = append(list, branches...)
			}
		} else {
			node["anyOf"] = branches
		}
		delete(node, "oneOf")
	}
	if all, ok := node["allOf"].([]any); ok {
		switch {
		case len(all) == 1:
			single, ok := all[0].(map[string]any)
			if ok {
				delete(node, "allOf")
				for k, v := range single {
					if _, exists := node[k]; !exists {
						node[k] = v
					}
				}
			}
		case allObjects(all) && o.collapseObjects:
			merged := map[string]any{"type": "object"}
			props := map[string]any{}
			var required []any
			for _, br := range all {
				m, _ := br.(map[string]any)
				if p, ok := m["properties"].(map[string]any); ok {
					for k, v := range p {
						props[k] = v
					}
				}
				if req, ok := m["required"].([]any); ok {
					required = append(required, req...)
				}
			}
			if len(props) > 0 {
				merged["properties"] = props
			}
			if len(required) > 0 {
				merged["required"] = required
			}
			delete(node, "allOf")
			for k, v := range merged {
				if _, exists := node[k]; !exists {
					node[k] = v
				}
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		branches, ok := node[key].([]any)
		if !ok || len(branches) == 0 {
			continue
		}
		if o.collapseObjects && allObjects(branches) {
			merged := map[string]any{"type": "object"}
			props := map[string]any{}
			var required []any
			for _, br := range branches {
				m, _ := br.(map[string]any)
				if p, ok := m["properties"].(map[string]any); ok {
					for k, v := range p {
						props[k] = v
					}
				}
				if req, ok := m["required"].([]any); ok {
					required = append(required, req...)
				}
			}
			if len(props) > 0 {
				merged["properties"] = props
			}
			if len(required) > 0 {
				merged["required"] = required
			}
			delete(node, key)
			for k, v := range merged {
				if _, exists := node[k]; !exists {
					node[k] = v
				}
			}
			continue
		}
		if o.lossyCombos {
			first, ok := branches[0].(map[string]any)
			if !ok {
				continue
			}
			delete(node, key)
			for k, v := range first {
				if _, exists := node[k]; !exists {
					node[k] = v
				}
			}
			continue
		}
	}
	return nil
}

func allObjects(branches []any) bool {
	if len(branches) == 0 {
		return false
	}
	for _, br := range branches {
		m, ok := br.(map[string]any)
		if !ok {
			return false
		}
		if t, ok := m["type"].(string); !ok || t != "object" {
			return false
		}
	}
	return true
}

// boolAsSchema converts a boolean openness keyword to schema form.
func boolAsSchema(open bool) any {
	if open {
		return map[string]any{}
	}
	return map[string]any{"not": map[string]any{}}
}

// dropUnsupportedKeys removes keywords the target provider rejects, spilling
// the human-meaningful ones into the sibling description first.
func dropUnsupportedKeys(node map[string]any, o *schemaOptions) {
	for _, key := range o.drop {
		val, ok := node[key]
		if !ok {
			continue
		}
		if o.spill != spillNone && isSpillable(key) && (o.spill == spillHuman || key == "default") {
			spillIntoDescription(node, key, val)
		}
		delete(node, key)
	}
}

func isSpillable(key string) bool {
	for _, k := range schemaSpillable {
		if k == key {
			return true
		}
	}
	return false
}

// spillIntoDescription lifts a dropped keyword into the description as
// " (key: value)". A missing description stays missing (there is nowhere to
// spill to) and an existing "(key:" marker means it was spilled before.
func spillIntoDescription(node map[string]any, key string, val any) {
	desc, ok := node["description"].(string)
	if !ok || desc == "" {
		return
	}
	if strings.Contains(desc, "("+key+":") {
		return
	}
	text := encodeSchemaValue(val, nil)
	node["description"] = desc + " (" + key + ": " + string(text) + ")"
}

// resolveJSONPointer resolves a local "#/..." JSON pointer against the document
// root (RFC 6901 escaping included).
func resolveJSONPointer(root any, ref string) (any, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	cur := root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(part, "~1", "/")
		part = strings.ReplaceAll(part, "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			var idx int
			if _, err := fmt.Sscanf(part, "%d", &idx); err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// stripLookarounds removes regex lookaround groups from a pattern: RE2 (and
// the OpenAI schema validator) reject `(?=`, `(?!`, `(?<=` and `(?<!` groups.
func stripLookarounds(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); {
		if pattern[i] == '\\' && i+1 < len(pattern) {
			b.WriteString(pattern[i : i+2])
			i += 2
			continue
		}
		if pattern[i] == '(' && i+1 < len(pattern) && pattern[i+1] == '?' {
			rest := pattern[i+2:]
			if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "!") ||
				strings.HasPrefix(rest, "<=") || strings.HasPrefix(rest, "<!") {
				i = skipGroup(pattern, i)
				continue
			}
		}
		b.WriteByte(pattern[i])
		i++
	}
	return b.String()
}

// skipGroup returns the index just past the group starting at open (which
// points at an unescaped '('), honoring nesting and character classes.
func skipGroup(pattern string, open int) int {
	depth := 0
	for i := open; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '[':
			for i++; i < len(pattern) && pattern[i] != ']'; i++ {
				if pattern[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(pattern)
}

// residualIncompatible reports whether a normalized schema still carries a
// construct the target provider cannot represent.
func residualIncompatible(v any) bool {
	switch node := v.(type) {
	case map[string]any:
		if _, ok := node["nullable"]; ok {
			return true
		}
		if t, ok := node["type"]; ok {
			switch tv := t.(type) {
			case []any:
				return true
			case string:
				if tv == "null" {
					return true
				}
			}
		}
		for _, key := range []string{"anyOf", "oneOf", "allOf"} {
			if _, ok := node[key]; ok {
				return true
			}
		}
		for _, val := range node {
			if residualIncompatible(val) {
				return true
			}
		}
	case []any:
		for _, el := range node {
			if residualIncompatible(el) {
				return true
			}
		}
	}
	return false
}

// ensureObjectRoot coerces a schema to an object root (MCP requires one).
func ensureObjectRoot(raw json.RawMessage) json.RawMessage {
	v, err := decodeSchemaValue(raw)
	if err != nil {
		return raw
	}
	node, ok := v.(map[string]any)
	if !ok {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if typ, has := node["type"]; has {
		name, isString := typ.(string)
		if !isString || name != "object" {
			// A non-object root cannot be expressed as an MCP inputSchema.
			return json.RawMessage(`{"type":"object","properties":{}}`)
		}
	} else {
		node["type"] = "object"
	}
	if _, has := node["properties"]; !has {
		node["properties"] = map[string]any{}
	}
	return encodeSchemaValue(node, raw)
}

// retypeRootObjectUnions writes `type:"object"` onto every branch of a tool
// parameters root anyOf/oneOf when every branch is already object-compatible
// (no type, or type "object"). xAI's validator 400s the untyped-branch shape
// (`ask: tool parameter root must be an object type`); under a root that is
// already an object, typing the branches admits no extra instance. A mixed
// union (object beside string) is left verbatim — that is the tool's own
// semantics. Nested unions are not touched: only the live 400 was at the root.
func retypeRootObjectUnions(raw json.RawMessage) json.RawMessage {
	v, err := decodeSchemaValue(raw)
	if err != nil {
		return raw
	}
	node, ok := v.(map[string]any)
	if !ok {
		return raw
	}
	changed := retypeObjectUnionBranches(node, "anyOf")
	if retypeObjectUnionBranches(node, "oneOf") {
		changed = true
	}
	if !changed {
		return raw
	}
	return encodeSchemaValue(node, raw)
}

func retypeObjectUnionBranches(node map[string]any, key string) bool {
	branches, ok := node[key].([]any)
	if !ok || len(branches) == 0 {
		return false
	}
	objs := make([]map[string]any, 0, len(branches))
	for _, b := range branches {
		obj, ok := b.(map[string]any)
		if !ok {
			return false
		}
		if tv, has := obj["type"]; has && tv != "object" {
			return false
		}
		objs = append(objs, obj)
	}
	wrote := false
	for _, obj := range objs {
		if _, has := obj["type"]; has {
			continue
		}
		obj["type"] = "object"
		wrote = true
	}
	return wrote
}

// --- strict-mode pipeline ---

// StrictModeDisabled reports whether PI_NO_STRICT is set: a global escape hatch
// that keeps every provider from emitting `"strict": true` (and from running
// the enforcement pass that would justify it).
func StrictModeDisabled() bool {
	v := strings.TrimSpace(os.Getenv("PI_NO_STRICT"))
	switch strings.ToLower(v) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// AdaptSchemaForStrict is the provider call-site composer: it sanitizes the
// schema and, when strict mode is requested and not bypassed, runs the
// enforcement pass. The returned bool is whether the caller MAY emit
// `"strict": true` — enforcement is fail-open, so a schema that could not be
// enforced must be sent with strict false instead of being rejected on the wire.
func AdaptSchemaForStrict(raw json.RawMessage, strict bool) (json.RawMessage, bool) {
	sanitized := SanitizeSchemaForStrictMode(raw)
	if !strict || StrictModeDisabled() {
		return sanitized, false
	}
	out, ok := TryEnforceStrictSchema(sanitized)
	if !ok {
		return sanitized, false
	}
	return out, true
}

// SanitizeSchemaForStrictMode strips everything OpenAI strict mode rejects:
// non-structural keywords (spilling `default` into the description), local
// $refs (siblings winning), single-item allOf, and type unions — which expand
// into one variant per type with type-specific keywords pruned.
func SanitizeSchemaForStrictMode(raw json.RawMessage) json.RawMessage {
	return normalizeSchema(raw, schemaOptions{
		drop:     strictDropKeys,
		spill:    spillDefault,
		typeSets: true,
	})
}

// strictDropKeys are the non-structural keywords strict mode rejects.
var strictDropKeys = []string{
	"$schema", "$id", "$comment", "$anchor", "$dynamicAnchor", "$vocabulary", "$dynamicRef",
	"definitions", "$defs", "if", "then", "else", "not", "dependentSchemas", "dependentRequired",
	"patternProperties", "propertyNames", "contains", "minContains", "maxContains", "prefixItems",
	"contentMediaType", "contentEncoding", "contentSchema", "unevaluatedItems", "unevaluatedProperties",
	"format", "pattern", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties",
	"examples", "title", "uniqueItems", "default",
}

// enforceStrictSchema JSON-Schema-strictifies every object node: object nodes
// get additionalProperties:false, every property becomes required, and
// properties that were optional become nullable unions.
func EnforceStrictSchema(raw json.RawMessage) (json.RawMessage, error) {
	v, err := decodeSchemaValue(raw)
	if err != nil {
		return raw, fmt.Errorf("strict: decode schema: %w", err)
	}
	out, err := enforceStrictNode(v)
	if err != nil {
		return raw, err
	}
	return encodeSchemaValue(out, raw), nil
}

// TryEnforceStrictSchema is the fail-open wrapper: on any error it reports ok
// false and the caller must keep `strict` off.
func TryEnforceStrictSchema(raw json.RawMessage) (out json.RawMessage, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			out, ok = raw, false
		}
	}()
	res, err := EnforceStrictSchema(raw)
	if err != nil {
		return raw, false
	}
	return res, true
}

func enforceStrictNode(v any) (any, error) {
	switch node := v.(type) {
	case bool:
		// Boolean subschemas cannot express strict object rules.
		return node, nil
	case []any:
		out := make([]any, len(node))
		for i, el := range node {
			walked, err := enforceStrictNode(el)
			if err != nil {
				return nil, err
			}
			out[i] = walked
		}
		return out, nil
	case map[string]any:
		return enforceStrictObject(node)
	default:
		return v, nil
	}
}

func enforceStrictObject(node map[string]any) (any, error) {
	for _, key := range []string{"items", "additionalProperties", "propertyNames", "contains", "unevaluatedProperties", "unevaluatedItems"} {
		child, ok := node[key]
		if !ok {
			continue
		}
		if _, isBool := child.(bool); isBool {
			continue
		}
		walked, err := enforceStrictNode(child)
		if err != nil {
			return nil, err
		}
		node[key] = walked
	}
	if props, ok := node["properties"].(map[string]any); ok {
		required := map[string]bool{}
		if req, ok := node["required"].([]any); ok {
			for _, r := range req {
				if name, ok := r.(string); ok {
					required[name] = true
				}
			}
		}
		names := make([]string, 0, len(props))
		for name, sub := range props {
			walked, err := enforceStrictNode(sub)
			if err != nil {
				return nil, err
			}
			if !required[name] && walked != nil {
				walked = nullableUnion(walked)
			}
			props[name] = walked
			names = append(names, name)
		}
		if len(names) > 0 {
			sort.Strings(names)
			req := make([]any, 0, len(names))
			for _, name := range names {
				req = append(req, name)
			}
			node["required"] = req
		}
		if node["type"] == "object" || node["type"] == nil {
			if _, has := node["type"]; !has {
				node["type"] = "object"
			}
			node["additionalProperties"] = false
		}
	}
	// A node declared object without a properties map still needs the marker.
	if node["type"] == "object" {
		if _, has := node["additionalProperties"]; !has {
			node["additionalProperties"] = false
		}
		if _, has := node["properties"]; !has {
			node["properties"] = map[string]any{}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := node[key].([]any)
		if !ok {
			continue
		}
		for i, br := range branches {
			walked, err := enforceStrictNode(br)
			if err != nil {
				return nil, err
			}
			branches[i] = walked
		}
		node[key] = branches
	}
	return node, nil
}

// nullableUnion wraps a property schema so it accepts null (strict mode marks
// optional properties this way instead of omitting them from `required`).
func nullableUnion(schema any) any {
	if m, ok := schema.(map[string]any); ok {
		if branches, ok := m["anyOf"].([]any); ok {
			for _, br := range branches {
				if bm, ok := br.(map[string]any); ok && bm["type"] == "null" {
					return schema
				}
			}
			m["anyOf"] = append(branches, map[string]any{"type": "null"})
			return m
		}
	}
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
}

// typeVariants implements strict mode's type-union handling: one variant schema
// per member type with type-specific keywords pruned, the shared description
// hoisted onto the anyOf wrapper.
func typeVariants(node map[string]any, root any, o *schemaOptions, refs map[string]bool, types []any) (any, error) {
	names := make([]string, 0, len(types))
	for _, t := range types {
		if s, ok := t.(string); ok {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return node, nil
	}
	shared := map[string]any{}
	if desc, ok := node["description"]; ok {
		shared["description"] = desc
	}
	branches := make([]any, 0, len(names))
	for _, name := range names {
		variant := map[string]any{"type": name}
		for k, val := range node {
			if k == "type" || k == "description" {
				continue
			}
			if !keywordAppliesTo(k, name) {
				continue
			}
			variant[k] = val
		}
		walked, err := walkSchemaObject(variant, root, o, refs)
		if err != nil {
			return nil, err
		}
		branches = append(branches, walked)
	}
	out := map[string]any{"anyOf": branches}
	for k, v := range shared {
		out[k] = v
	}
	return out, nil
}

// keywordAppliesTo reports whether a keyword is meaningful for a JSON type
// (strict mode prunes type-specific keywords from the other variants).
func keywordAppliesTo(keyword, typ string) bool {
	switch typ {
	case "object":
		return contains([]string{"additionalProperties", "properties", "required", "minProperties", "maxProperties", "patternProperties", "propertyNames", "dependentSchemas", "dependentRequired"}, keyword)
	case "array":
		return contains([]string{"items", "minItems", "maxItems", "uniqueItems", "prefixItems", "contains"}, keyword)
	case "string":
		return contains([]string{"minLength", "maxLength", "pattern", "format", "enum", "const"}, keyword)
	case "integer", "number":
		return contains([]string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "enum", "const"}, keyword)
	case "null":
		return false
	default:
		return true
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- JSON value plumbing ---

func decodeSchemaValue(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("schema: empty")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func encodeSchemaValue(v any, fallback json.RawMessage) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return fallback
	}
	return b
}
