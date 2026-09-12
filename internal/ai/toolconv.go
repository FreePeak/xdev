package ai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tool-call dialect conversion (M14 #62).
//
// Some model families emit tool calls as text inside the assistant message
// rather than as structured tool_calls. toolFormatTable maps a model id onto
// the dialect that family emits; the converters below render (Encode*) and
// scan (Decode*) that dialect so an in-band model can still drive the tool
// loop. Unknown models resolve to ToolFormatNative, i.e. the provider's own
// structured tool calls are used untouched.
//
// Wire shapes are taken from the toolconv references:
// deepseek(DSML V3.1), glm-4.5/4.6, harmony(gpt-oss), hermes/qwen3, kimi-k2,
// minimax, gemini(tool_code), gemma-4, and the generic xml invoke protocol.

// ToolFormat names one tool-call dialect.
type ToolFormat string

const (
	// ToolFormatNative is provider-native structured tool calling.
	ToolFormatNative ToolFormat = "native"
	// ToolFormatDeepSeek is the DeepSeek-V3.1 special-token envelope.
	ToolFormatDeepSeek ToolFormat = "deepseek"
	// ToolFormatGemma is the Gemma 4 token-delimited call:NAME{...} grammar.
	ToolFormatGemma ToolFormat = "gemma"
	// ToolFormatGemini is the hosted-Gemini/Gemma-3 pythonic tool_code form.
	ToolFormatGemini ToolFormat = "gemini"
	// ToolFormatGlm is GLM-4.5/4.6's XML-like arg_key/arg_value block.
	ToolFormatGlm ToolFormat = "glm"
	// ToolFormatHarmony is the gpt-oss Harmony channel format.
	ToolFormatHarmony ToolFormat = "harmony"
	// ToolFormatHermes is the Hermes 2/3 JSON-in-tag format.
	ToolFormatHermes ToolFormat = "hermes"
	// ToolFormatKimi is the Kimi K2 tool-calls section format.
	ToolFormatKimi ToolFormat = "kimi"
	// ToolFormatMinimax is the MiniMax <minimax:tool_call> envelope.
	ToolFormatMinimax ToolFormat = "minimax"
	// ToolFormatQwen3 is the Qwen3 adoption of the Hermes convention.
	ToolFormatQwen3 ToolFormat = "qwen3"
	// ToolFormatXML is the generic <invoke>/<parameter> protocol.
	ToolFormatXML ToolFormat = "xml"
)

// toolFormatAliases accepts the spellings seen in the wild (and omp's
// tools.format enum) without duplicating table rows.
var toolFormatAliases = map[string]ToolFormat{
	"":          ToolFormatNative,
	"pi-native": ToolFormatNative,
	"glm-4.5":   ToolFormatGlm,
	"glm-4.6":   ToolFormatGlm,
	"kimi-k2":   ToolFormatKimi,
	"qwen":      ToolFormatQwen3,
	"gpt-oss":   ToolFormatHarmony,
}

// toolFormatTable is the data-driven model-id -> dialect matrix. First match
// wins, so family-specific rows precede the vendor rows they could contain.
type toolFormatTable struct {
	// Families documents what the row covers (kept next to the pattern so the
	// matrix reads as data, not code).
	Families string
	Match    *regexp.Regexp
	Format   ToolFormat
}

var toolFormatMatrix = []toolFormatTable{
	{"deepseek-v3/v3.1/r1", regexp.MustCompile(`(?i)deepseek`), ToolFormatDeepSeek},
	{"glm-4.5/4.6", regexp.MustCompile(`(?i)(^|[/_-])glm`), ToolFormatGlm},
	{"gpt-oss (harmony)", regexp.MustCompile(`(?i)gpt-oss|harmony`), ToolFormatHarmony},
	{"hermes 2/3", regexp.MustCompile(`(?i)hermes`), ToolFormatHermes},
	{"kimi-k2", regexp.MustCompile(`(?i)kimi`), ToolFormatKimi},
	{"minimax/abab", regexp.MustCompile(`(?i)minimax|abab`), ToolFormatMinimax},
	{"qwen2.5/qwen3/qwq", regexp.MustCompile(`(?i)qwen|qwq`), ToolFormatQwen3},
	{"gemma-4", regexp.MustCompile(`(?i)gemma`), ToolFormatGemma},
}

// ToolFormats lists every dialect in stable order (for error messages and
// config validation). The generic xml protocol has no model-id affinity, so it
// is only reachable when selected explicitly.
func ToolFormats() []ToolFormat {
	return []ToolFormat{
		ToolFormatNative, ToolFormatDeepSeek, ToolFormatGemma, ToolFormatGemini,
		ToolFormatGlm, ToolFormatHarmony, ToolFormatHermes, ToolFormatKimi,
		ToolFormatMinimax, ToolFormatQwen3, ToolFormatXML,
	}
}

// ParseToolFormat normalizes a configured dialect name; the empty string means
// native.
func ParseToolFormat(name string) (ToolFormat, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if f, ok := toolFormatAliases[key]; ok {
		return f, nil
	}
	for _, f := range ToolFormats() {
		if string(f) == key {
			return f, nil
		}
	}
	names := make([]string, 0, len(toolFormatAliases))
	for _, f := range ToolFormats() {
		names = append(names, string(f))
	}
	sort.Strings(names)
	return "", fmt.Errorf("unknown tool format %q (want %s)", name, strings.Join(names, "|"))
}

// ResolveToolFormat returns the dialect a model id emits, defaulting to native
// for unknown models.
func ResolveToolFormat(model string) ToolFormat {
	for _, row := range toolFormatMatrix {
		if row.Match.MatchString(model) {
			return row.Format
		}
	}
	return ToolFormatNative
}

// SupportsNativeToolCalls reports whether the dialect rides the provider's
// structured tool_calls (only native does).
func SupportsNativeToolCalls(f ToolFormat) bool { return f == ToolFormatNative }

// --- markers (verbatim wire tokens) ---

const (
	hermesCallOpen    = "<tool_call>"
	hermesCallClose   = "</tool_call>"
	hermesResultOpen  = "<tool_response>"
	hermesResultClose = "</tool_response>"

	glmObservation = "<|observation|>"

	gemmaCallOpen     = "<|tool_call>"
	gemmaCallClose    = "<tool_call|>"
	gemmaResultOpen   = "<|tool_response>"
	gemmaResultClose  = "<tool_response|>"
	gemmaStringMarker = `<|"|>`

	kimiSectionOpen  = "<|tool_calls_section_begin|>"
	kimiSectionClose = "<|tool_calls_section_end|>"
	kimiCallOpen     = "<|tool_call_begin|>"
	kimiArgsOpen     = "<|tool_call_argument_begin|>"
	kimiCallClose    = "<|tool_call_end|>"
	kimiSystemOpen   = "<|im_system|>"
	kimiMiddle       = "<|im_middle|>"
	kimiEnd          = "<|im_end|>"

	harmonyStart   = "<|start|>"
	harmonyChannel = "<|channel|>"
	harmonyMessage = "<|message|>"
	harmonyCall    = "<|call|>"
	harmonyEnd     = "<|end|>"
	harmonyReturn  = "<|return|>"
	harmonyToFunc  = "to=functions."

	minimaxCallOpen    = "<minimax:tool_call>"
	minimaxCallClose   = "</minimax:tool_call>"
	functionResults    = "<function_results>"
	functionResultsEnd = "</function_results>"

	// DeepSeek uses U+FF5C FULLWIDTH VERTICAL LINE and U+2581 LOWER ONE EIGHTH
	// BLOCK (SentencePiece word boundary) inside these markers — never the
	// ASCII pipe/underscore spellings.
	dsCallsOpen  = "<\uFF5Ctool\u2581calls\u2581begin\uFF5C>"
	dsCallsClose = "<\uFF5Ctool\u2581calls\u2581end\uFF5C>"
	dsCallOpen   = "<\uFF5Ctool\u2581call\u2581begin\uFF5C>"
	dsCallClose  = "<\uFF5Ctool\u2581call\u2581end\uFF5C>"
	dsSep        = "<\uFF5Ctool\u2581sep\uFF5C>"

	geminiToolCodeFence    = "```tool_code"
	geminiToolOutputsFence = "```tool_outputs"
)

// ToolFormatPrompt returns the in-band prompt section for a dialect: the tool
// catalog (one OpenAI-style tool object per line inside <tools></tools>) plus
// the dialect's format guide. Native returns "", since the provider carries
// the real tools field.
func ToolFormatPrompt(f ToolFormat, tools []ToolDef) string {
	if SupportsNativeToolCalls(f) {
		return ""
	}
	var b strings.Builder
	b.WriteString("<tools>\n")
	for _, t := range tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		obj := map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		}
		enc, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		b.Write(enc)
		b.WriteString("\n")
	}
	b.WriteString("</tools>\n\n")
	b.WriteString(toolFormatGuide(f))
	return b.String()
}

// toolFormatGuide is the per-dialect "always adhere to this format" section.
func toolFormatGuide(f ToolFormat) string {
	switch f {
	case ToolFormatDeepSeek:
		return "IMPORTANT: ALWAYS adhere to this exact format for tool use:\n" +
			dsCallsOpen + dsCallOpen + "tool_call_name" + dsSep + "tool_call_arguments" + dsCallClose +
			"{additional_tool_calls}" + dsCallsClose + "\n" +
			"tool_call_name must exactly match an available tool; tool_call_arguments is a raw JSON object; " +
			"chain additional calls directly without separators."
	case ToolFormatGlm:
		return "Emit each call as:\n" + hermesCallOpen + "tool_name\n" +
			"<arg_key>name</arg_key>\n<arg_value>value</arg_value>\n" + hermesCallClose + "\n" +
			"String values are written bare (no quotes); numbers, booleans, objects and arrays are JSON."
	case ToolFormatHarmony:
		return "Emit each call as an assistant commentary message addressed to the function:\n" +
			harmonyStart + "assistant" + harmonyChannel + "commentary " + harmonyToFunc + "tool_name" + harmonyMessage +
			`{"arg":"value"}` + harmonyCall + "\n" +
			"Tool results come back as " + harmonyStart + "functions.tool_name to=assistant" + harmonyChannel + "commentary" + harmonyMessage + "{...}" + harmonyEnd + "."
	case ToolFormatHermes:
		return "Emit each call as " + hermesCallOpen + "\n" +
			`{"name": "tool_name", "arguments": {"arg": "value"}}` + "\n" + hermesCallClose + "\n" +
			"arguments is a nested JSON object, never a JSON-encoded string. Use one block per call."
	case ToolFormatQwen3:
		return "Emit each call as " + hermesCallOpen + "\n" +
			`{"name": "tool_name", "arguments": {"arg": "value"}}` + "\n" + hermesCallClose + "\n" +
			"arguments is a nested JSON object. Tool results follow as " + hermesResultOpen + " blocks."
	case ToolFormatKimi:
		return "Emit all calls of a turn in one section:\n" +
			kimiSectionOpen + kimiCallOpen + "functions.tool_name:0" + kimiArgsOpen + `{"arg":"value"}` + kimiCallClose +
			kimiSectionClose + "\n" +
			"The index increments per call in the turn; the arguments are a raw JSON object."
	case ToolFormatMinimax:
		return "Emit calls inside one envelope:\n" + minimaxCallOpen + "\n" +
			`<invoke name="tool_name"><parameter name="arg">value</parameter></invoke>` + "\n" + minimaxCallClose + "\n" +
			"String bodies are verbatim; numbers, booleans and objects are JSON."
	case ToolFormatXML:
		return "Emit one invoke per call, optionally wrapped in <tool_calls>:\n" +
			`<invoke name="tool_name"><parameter name="arg">value</parameter></invoke>` + "\n" +
			"String bodies are verbatim; numbers, booleans and objects are JSON. Never emit tool results yourself."
	case ToolFormatGemma:
		return "Emit one call per block:\n" + gemmaCallOpen + "call:tool_name{arg:" + gemmaStringMarker + "value" + gemmaStringMarker + "}" + gemmaCallClose + "\n" +
			"Strings are wrapped in " + gemmaStringMarker + "; numbers, booleans, null, lists and objects are bare."
	case ToolFormatGemini:
		return "Emit each call as Python inside a fenced block:\n" + geminiToolCodeFence + "\n" +
			`print(default_api.tool_name(arg="value"))` + "\n```\n" +
			"One print(...) call, or one Python list of calls for several. Never emit tool results yourself."
	default:
		return ""
	}
}

// --- encoding ---

// EncodeToolCalls renders structured calls as assistant text in the dialect.
// Native has no text form and returns "".
func EncodeToolCalls(f ToolFormat, calls []ToolCallBlock) (string, error) {
	if len(calls) == 0 || SupportsNativeToolCalls(f) {
		return "", nil
	}
	switch f {
	case ToolFormatDeepSeek:
		var b strings.Builder
		b.WriteString(dsCallsOpen)
		for _, c := range calls {
			b.WriteString(dsCallOpen)
			b.WriteString(c.Name)
			b.WriteString(dsSep)
			b.WriteString(argsString(c))
			b.WriteString(dsCallClose)
		}
		b.WriteString(dsCallsClose)
		return b.String(), nil
	case ToolFormatGlm:
		parts := make([]string, 0, len(calls))
		for _, c := range calls {
			var b strings.Builder
			b.WriteString(hermesCallOpen)
			b.WriteString(c.Name)
			for _, k := range sortedKeys(c) {
				b.WriteString("\n<arg_key>")
				b.WriteString(xmlEscapeAttr(k))
				b.WriteString("</arg_key>\n<arg_value>")
				b.WriteString(glmValue(c.Arguments, k))
				b.WriteString("</arg_value>")
			}
			b.WriteString("\n")
			b.WriteString(hermesCallClose)
			parts = append(parts, b.String())
		}
		return strings.Join(parts, "\n"), nil
	case ToolFormatHarmony:
		parts := make([]string, 0, len(calls))
		for _, c := range calls {
			parts = append(parts, harmonyStart+"assistant"+harmonyChannel+"commentary "+
				harmonyToFunc+c.Name+harmonyMessage+argsString(c)+harmonyCall)
		}
		return strings.Join(parts, "\n"), nil
	case ToolFormatHermes, ToolFormatQwen3:
		parts := make([]string, 0, len(calls))
		for _, c := range calls {
			obj, err := toolCallObject(c)
			if err != nil {
				return "", err
			}
			parts = append(parts, hermesCallOpen+"\n"+obj+"\n"+hermesCallClose)
		}
		return strings.Join(parts, "\n"), nil
	case ToolFormatKimi:
		var b strings.Builder
		b.WriteString(kimiSectionOpen)
		for i, c := range calls {
			b.WriteString(kimiCallOpen)
			b.WriteString(kimiCallID(c, i))
			b.WriteString(kimiArgsOpen)
			b.WriteString(argsString(c))
			b.WriteString(kimiCallClose)
		}
		b.WriteString(kimiSectionClose)
		return b.String(), nil
	case ToolFormatMinimax:
		return minimaxCallOpen + "\n" + xmlInvokes(calls) + "\n" + minimaxCallClose, nil
	case ToolFormatXML:
		return xmlInvokes(calls), nil
	case ToolFormatGemma:
		parts := make([]string, 0, len(calls))
		for _, c := range calls {
			args, err := decodeArgsObject(c.Arguments)
			if err != nil {
				return "", err
			}
			parts = append(parts, gemmaCallOpen+"call:"+c.Name+"{"+gemmaArgs(args)+"}"+gemmaCallClose)
		}
		return strings.Join(parts, "\n"), nil
	case ToolFormatGemini:
		exprs := make([]string, 0, len(calls))
		for _, c := range calls {
			args, err := decodeArgsObject(c.Arguments)
			if err != nil {
				return "", err
			}
			exprs = append(exprs, "default_api."+c.Name+"("+pythonKwargs(args)+")")
		}
		expr := exprs[0]
		if len(exprs) > 1 {
			expr = "[" + strings.Join(exprs, ", ") + "]"
		} else {
			expr = "print(" + expr + ")"
		}
		return geminiToolCodeFence + "\n" + expr + "\n```", nil
	default:
		return "", nil
	}
}

// EncodeToolResults renders toolResult messages as the dialect's result text.
func EncodeToolResults(f ToolFormat, results []Message) string {
	if len(results) == 0 || SupportsNativeToolCalls(f) {
		return ""
	}
	parts := make([]string, 0, len(results))
	switch f {
	case ToolFormatDeepSeek:
		for _, m := range results {
			parts = append(parts, "<\uFF5Ctool\u2581output\u2581begin\uFF5C>"+toolResultTextOf(m)+"<\uFF5Ctool\u2581output\u2581end\uFF5C>")
		}
		return strings.Join(parts, "")
	case ToolFormatGlm:
		for _, m := range results {
			parts = append(parts, hermesResultOpen+"\n"+toolResultTextOf(m)+"\n"+hermesResultClose)
		}
		return glmObservation + "\n" + strings.Join(parts, "\n")
	case ToolFormatHarmony:
		for _, m := range results {
			name := m.ToolName
			if name == "" {
				name = "tool"
			}
			parts = append(parts, harmonyStart+"functions."+name+" to=assistant"+harmonyChannel+
				"commentary"+harmonyMessage+toolResultTextOf(m)+harmonyEnd)
		}
		return strings.Join(parts, "\n")
	case ToolFormatHermes:
		for _, m := range results {
			obj := map[string]any{"name": m.ToolName, "content": jsonDecodedOrString(toolResultTextOf(m))}
			enc, err := json.Marshal(obj)
			if err != nil {
				continue
			}
			parts = append(parts, hermesResultOpen+"\n"+string(enc)+"\n"+hermesResultClose)
		}
		return strings.Join(parts, "\n")
	case ToolFormatQwen3:
		for _, m := range results {
			parts = append(parts, hermesResultOpen+"\n"+toolResultTextOf(m)+"\n"+hermesResultClose)
		}
		return strings.Join(parts, "\n")
	case ToolFormatKimi:
		for _, m := range results {
			name := m.ToolName
			if name == "" {
				name = "tool"
			}
			id := m.ToolCallID
			if id == "" {
				id = "functions." + name
			}
			parts = append(parts, kimiSystemOpen+name+kimiMiddle+"## Return of "+id+"\n"+toolResultTextOf(m)+kimiEnd)
		}
		return strings.Join(parts, "\n")
	case ToolFormatMinimax:
		var b strings.Builder
		b.WriteString(functionResults)
		for _, m := range results {
			name := m.ToolName
			if name == "" {
				name = "tool"
			}
			b.WriteString("\n<result>\n<tool_name>" + xmlEscapeText(name) + "</tool_name>\n<stdout>" +
				toolResultTextOf(m) + "</stdout>\n</result>")
		}
		b.WriteString("\n" + functionResultsEnd)
		return b.String()
	case ToolFormatXML:
		for _, m := range results {
			parts = append(parts, hermesResultOpen+"\n"+toolResultTextOf(m)+"\n"+hermesResultClose)
		}
		return strings.Join(parts, "\n")
	case ToolFormatGemma:
		for _, m := range results {
			name := m.ToolName
			if name == "" {
				name = "tool"
			}
			parts = append(parts, gemmaResultOpen+"response:"+name+"{output:"+
				gemmaValue(jsonDecodedOrString(toolResultTextOf(m)))+"}"+gemmaResultClose)
		}
		return strings.Join(parts, "\n")
	case ToolFormatGemini:
		for _, m := range results {
			parts = append(parts, geminiToolOutputsFence+"\n"+toolResultTextOf(m)+"\n```")
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// --- decoding ---

// DecodeToolCalls scans assistant text for dialect calls, returning them plus
// the residual visible text (everything outside the call envelopes). Dialects
// that carry no call id get a synthesized one.
func DecodeToolCalls(f ToolFormat, text string) ([]ToolCallBlock, string, error) {
	if SupportsNativeToolCalls(f) {
		return nil, text, nil
	}
	switch f {
	case ToolFormatDeepSeek:
		return decodeDeepSeek(text)
	case ToolFormatGlm:
		return decodeGlm(text)
	case ToolFormatHarmony:
		return decodeHarmony(text)
	case ToolFormatHermes, ToolFormatQwen3:
		return decodeHermes(text)
	case ToolFormatKimi:
		return decodeKimi(text)
	case ToolFormatMinimax, ToolFormatXML:
		return decodeXMLInvokes(text)
	case ToolFormatGemma:
		return decodeGemma(text)
	case ToolFormatGemini:
		return decodeGemini(text)
	default:
		return nil, text, nil
	}
}

func decodeDeepSeek(text string) ([]ToolCallBlock, string, error) {
	var calls []ToolCallBlock
	residual := text
	for {
		i := strings.Index(residual, dsCallsOpen)
		if i < 0 {
			break
		}
		j := strings.Index(residual[i:], dsCallsClose)
		if j < 0 {
			break
		}
		body := residual[i+len(dsCallsOpen) : i+j]
		residual = residual[:i] + residual[i+j+len(dsCallsClose):]
		chunks := strings.Split(body, dsCallOpen)
		for _, chunk := range chunks {
			chunk = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(chunk), dsCallClose))
			if chunk == "" {
				continue
			}
			name, raw, ok := strings.Cut(chunk, dsSep)
			if !ok {
				return nil, text, fmt.Errorf("deepseek: tool call %q has no %s separator", chunk, dsSep)
			}
			args, err := parseArgsRepair(raw)
			if err != nil {
				return nil, text, fmt.Errorf("deepseek: tool %s arguments: %w", strings.TrimSpace(name), err)
			}
			calls = append(calls, ToolCallBlock{
				ID:          synthCallID(ToolFormatDeepSeek, len(calls)),
				Name:        strings.TrimSpace(name),
				Arguments:   args,
				PartialArgs: strings.TrimSpace(raw),
				StreamIndex: len(calls),
			})
		}
	}
	return calls, strings.TrimSpace(residual), nil
}

func decodeHermes(text string) ([]ToolCallBlock, string, error) {
	var calls []ToolCallBlock
	residual := text
	for {
		i := strings.Index(residual, hermesCallOpen)
		if i < 0 {
			break
		}
		j := strings.Index(residual[i+len(hermesCallOpen):], hermesCallClose)
		if j < 0 {
			return nil, text, fmt.Errorf("hermes: unterminated %s block", hermesCallOpen)
		}
		body := strings.TrimSpace(residual[i+len(hermesCallOpen) : i+len(hermesCallOpen)+j])
		residual = residual[:i] + residual[i+len(hermesCallOpen)+j+len(hermesCallClose):]
		var obj struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			return nil, text, fmt.Errorf("hermes: tool call body: %w", err)
		}
		args, err := normalizeArguments(obj.Arguments)
		if err != nil {
			return nil, text, fmt.Errorf("hermes: tool %s arguments: %w", obj.Name, err)
		}
		calls = append(calls, ToolCallBlock{
			ID:          synthCallID(ToolFormatHermes, len(calls)),
			Name:        obj.Name,
			Arguments:   args,
			PartialArgs: string(args),
			StreamIndex: len(calls),
		})
	}
	return calls, strings.TrimSpace(residual), nil
}

func decodeGlm(text string) ([]ToolCallBlock, string, error) {
	var calls []ToolCallBlock
	residual := text
	for {
		i := strings.Index(residual, hermesCallOpen)
		if i < 0 {
			break
		}
		j := strings.Index(residual[i+len(hermesCallOpen):], hermesCallClose)
		if j < 0 {
			return nil, text, fmt.Errorf("glm: unterminated %s block", hermesCallOpen)
		}
		body := residual[i+len(hermesCallOpen) : i+len(hermesCallOpen)+j]
		residual = residual[:i] + residual[i+len(hermesCallOpen)+j+len(hermesCallClose):]
		name, rest, _ := strings.Cut(body, "\n")
		args := map[string]any{}
		for _, m := range glmArgRe.FindAllStringSubmatch(rest, -1) {
			args[xmlUnescape(m[1])] = glmValueIn(strings.TrimSpace(m[2]))
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, text, fmt.Errorf("glm: tool %s arguments: %w", strings.TrimSpace(name), err)
		}
		calls = append(calls, ToolCallBlock{
			ID:          synthCallID(ToolFormatGlm, len(calls)),
			Name:        strings.TrimSpace(name),
			Arguments:   raw,
			PartialArgs: string(raw),
			StreamIndex: len(calls),
		})
	}
	return calls, strings.TrimSpace(residual), nil
}

func decodeHarmony(text string) ([]ToolCallBlock, string, error) {
	var calls []ToolCallBlock
	residual := text
	for {
		i := strings.Index(residual, harmonyToFunc)
		if i < 0 {
			break
		}
		rest := residual[i+len(harmonyToFunc):]
		name, tail := splitFuncName(rest)
		k := strings.Index(tail, harmonyMessage)
		if k < 0 {
			break
		}
		body := tail[k+len(harmonyMessage):]
		endIdx, endMarker := indexAny(body, harmonyCall, harmonyEnd, harmonyReturn)
		if endIdx < 0 {
			return nil, text, fmt.Errorf("harmony: unterminated %s call", name)
		}
		args, err := parseArgsRepair(strings.TrimSpace(body[:endIdx]))
		if err != nil {
			return nil, text, fmt.Errorf("harmony: tool %s arguments: %w", name, err)
		}
		calls = append(calls, ToolCallBlock{
			ID:          synthCallID(ToolFormatHarmony, len(calls)),
			Name:        name,
			Arguments:   args,
			PartialArgs: strings.TrimSpace(body[:endIdx]),
			StreamIndex: len(calls),
		})
		// Cut the whole envelope (from <|start|> when present) out of the text.
		start := strings.LastIndex(residual[:i], harmonyStart)
		if start < 0 {
			start = i
		}
		envelopeEnd := i + len(harmonyToFunc) + len(name) + k + len(harmonyMessage) + endIdx + len(endMarker)
		residual = residual[:start] + residual[envelopeEnd:]
	}
	return calls, strings.TrimSpace(residual), nil
}

func decodeKimi(text string) ([]ToolCallBlock, string, error) {
	var calls []ToolCallBlock
	residual := text
	for {
		i := strings.Index(residual, kimiCallOpen)
		if i < 0 {
			break
		}
		rest := residual[i+len(kimiCallOpen):]
		k := strings.Index(rest, kimiArgsOpen)
		if k < 0 {
			return nil, text, fmt.Errorf("kimi: call without %s", kimiArgsOpen)
		}
		id := strings.TrimSpace(rest[:k])
		body := rest[k+len(kimiArgsOpen):]
		e := strings.Index(body, kimiCallClose)
		if e < 0 {
			return nil, text, fmt.Errorf("kimi: unterminated %s", kimiCallClose)
		}
		raw := strings.TrimSpace(body[:e])
		consumedStart, consumedEnd := i, i+len(kimiCallOpen)+k+len(kimiArgsOpen)+e+len(kimiCallClose)
		if s := strings.LastIndex(residual[:i], kimiSectionOpen); s >= 0 {
			consumedStart = s
		}
		if c := strings.Index(residual[consumedEnd:], kimiSectionClose); c >= 0 && strings.TrimSpace(residual[consumedEnd:consumedEnd+c]) == "" {
			consumedEnd += c + len(kimiSectionClose)
		}
		residual = residual[:consumedStart] + residual[consumedEnd:]
		args, err := parseArgsRepair(raw)
		if err != nil {
			return nil, text, fmt.Errorf("kimi: tool %s arguments: %w", id, err)
		}
		calls = append(calls, ToolCallBlock{
			ID:          id,
			Name:        kimiFuncName(id),
			Arguments:   args,
			PartialArgs: raw,
			StreamIndex: len(calls),
		})
	}
	return calls, strings.TrimSpace(residual), nil
}

// decodeXMLInvokes handles the generic xml protocol and MiniMax's envelope
// (the scanner accepts either, with or without the wrapper).
func decodeXMLInvokes(text string) ([]ToolCallBlock, string, error) {
	var calls []ToolCallBlock
	residual := text
	for {
		i := strings.Index(residual, "<invoke ")
		if i < 0 {
			break
		}
		e := strings.Index(residual[i:], "</invoke>")
		if e < 0 {
			return nil, text, fmt.Errorf("xml: unterminated invoke")
		}
		block := residual[i : i+e+len("</invoke>")]
		name := attrValue(block[:strings.Index(block, ">")+1], "name")
		args := map[string]any{}
		for _, m := range xmlParamRe.FindAllStringSubmatch(block, -1) {
			args[attrValue(m[1], "name")] = xmlStringArgIn(m[1], m[2])
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, text, fmt.Errorf("xml: tool %s arguments: %w", name, err)
		}
		calls = append(calls, ToolCallBlock{
			ID:          synthCallID(ToolFormatXML, len(calls)),
			Name:        name,
			Arguments:   raw,
			PartialArgs: string(raw),
			StreamIndex: len(calls),
		})
		start := i
		if s := strings.LastIndex(residual[:i], minimaxCallOpen); s >= 0 && strings.TrimSpace(residual[s+len(minimaxCallOpen):i]) == "" {
			start = s
		} else if s := strings.LastIndex(residual[:i], "<tool_calls>"); s >= 0 && strings.TrimSpace(residual[s+len("<tool_calls>"):i]) == "" {
			start = s
		}
		end := i + e + len("</invoke>")
		for _, closer := range []string{minimaxCallClose, "</tool_calls>", "</function_calls>"} {
			if c := strings.Index(residual[end:], closer); c >= 0 && strings.TrimSpace(residual[end:end+c]) == "" {
				end += c + len(closer)
				break
			}
		}
		residual = residual[:start] + residual[end:]
	}
	return calls, strings.TrimSpace(residual), nil
}

// decodeGemma walks the Gemma 4 <|tool_call>call:NAME{...}<tool_call|> grammar
// with string-span and brace-depth awareness.
func decodeGemma(text string) ([]ToolCallBlock, string, error) {
	var calls []ToolCallBlock
	residual := text
	for {
		i := strings.Index(residual, gemmaCallOpen)
		if i < 0 {
			break
		}
		body := residual[i+len(gemmaCallOpen):]
		end := findGemmaBlockEnd(body)
		if end < 0 {
			break // unterminated block: dropped, per the dialect's flush rule
		}
		block := body[:end]
		residual = residual[:i] + body[end+len(gemmaCallClose):]
		name, args, err := parseGemmaCall(block)
		if err != nil {
			return nil, text, err
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, text, fmt.Errorf("gemma: tool %s arguments: %w", name, err)
		}
		calls = append(calls, ToolCallBlock{
			ID:          synthCallID(ToolFormatGemma, len(calls)),
			Name:        name,
			Arguments:   raw,
			PartialArgs: string(raw),
			StreamIndex: len(calls),
		})
	}
	return calls, strings.TrimSpace(residual), nil
}

// decodeGemini scans the pythonic tool_code fences.
func decodeGemini(text string) ([]ToolCallBlock, string, error) {
	var calls []ToolCallBlock
	residual := text
	for {
		i := strings.Index(residual, geminiToolCodeFence)
		if i < 0 {
			break
		}
		body := residual[i+len(geminiToolCodeFence):]
		end := strings.Index(body, "```")
		if end < 0 {
			break
		}
		block := body[:end]
		residual = residual[:i] + body[end+len("```"):]
		for _, m := range geminiCallRe.FindAllStringSubmatch(block, -1) {
			args := map[string]any{}
			for _, kv := range splitTopLevel(m[2], ',') {
				key, val, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				args[strings.TrimSpace(key)] = pythonValueIn(strings.TrimSpace(val))
			}
			raw, err := json.Marshal(args)
			if err != nil {
				return nil, text, fmt.Errorf("gemini: tool %s arguments: %w", m[1], err)
			}
			calls = append(calls, ToolCallBlock{
				ID:          synthCallID(ToolFormatGemini, len(calls)),
				Name:        m[1],
				Arguments:   raw,
				PartialArgs: string(raw),
				StreamIndex: len(calls),
			})
		}
	}
	return calls, strings.TrimSpace(residual), nil
}

// --- conversion helpers ---

var (
	glmArgRe     = regexp.MustCompile(`(?s)<arg_key>(.*?)</arg_key>\s*<arg_value>(.*?)</arg_value>`)
	xmlParamRe   = regexp.MustCompile(`(?s)<parameter\s+([^>]*)>(.*?)</parameter>`)
	geminiCallRe = regexp.MustCompile(`(?s)default_api\.([A-Za-z_][A-Za-z0-9_]*)\(([^()]*)\)`)
	attrRe       = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)\s*=\s*"([^"]*)"`)
)

func synthCallID(f ToolFormat, n int) string {
	return "call_" + string(f) + "_" + strconv.Itoa(n)
}

func argsString(c ToolCallBlock) string {
	return emptyJSONObjectArgs(string(c.Arguments))
}

// toolCallObject renders the Hermes/Qwen3 two-key call object.
func toolCallObject(c ToolCallBlock) (string, error) {
	args, err := decodeArgsObject(c.Arguments)
	if err != nil {
		return "", fmt.Errorf("hermes: tool %s arguments: %w", c.Name, err)
	}
	obj := map[string]any{"name": c.Name, "arguments": args}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// kimiCallID preserves an id already in wire form, otherwise mints
// functions.<name>:<index>.
func kimiCallID(c ToolCallBlock, fallbackIdx int) string {
	if strings.HasPrefix(c.ID, "functions.") {
		return c.ID
	}
	idx := c.StreamIndex
	if idx == 0 {
		idx = fallbackIdx
	}
	return fmt.Sprintf("functions.%s:%d", c.Name, idx)
}

func kimiFuncName(id string) string {
	id = strings.TrimPrefix(id, "functions.")
	if i := strings.LastIndex(id, ":"); i >= 0 {
		id = id[:i]
	}
	return id
}

// xmlInvokes renders calls as <invoke name="..."> elements.
func xmlInvokes(calls []ToolCallBlock) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		var b strings.Builder
		b.WriteString(`<invoke name="` + xmlEscapeAttr(c.Name) + `">`)
		args, err := decodeArgsObject(c.Arguments)
		if err != nil {
			continue
		}
		for _, k := range sortedKeysOf(args) {
			b.WriteString(`<parameter name="` + xmlEscapeAttr(k) + `">`)
			b.WriteString(xmlArgValue(args[k]))
			b.WriteString(`</parameter>`)
		}
		b.WriteString("</invoke>")
		parts = append(parts, b.String())
	}
	return strings.Join(parts, "\n")
}

// toolResultTextOf flattens a toolResult message to its text blocks.
func toolResultTextOf(m Message) string { return toolResultText(m) }

// decodeArgsObject parses a call's arguments into a generic object; an empty
// argument set is an empty object.
func decodeArgsObject(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// parseArgsRepair validates a dialect's argument text, accepting the code-fence
// wrapper some models leak around it.
func parseArgsRepair(raw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, fmt.Errorf("invalid JSON args %s", trimmed)
	}
	return json.RawMessage(trimmed), nil
}

// normalizeArguments accepts Hermes' nested-object arguments and the
// JSON-encoded-string variant some fine-tunes emit.
func normalizeArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	if raw[0] == '"' {
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil, err
		}
		return parseArgsRepair(inner)
	}
	return parseArgsRepair(string(raw))
}

// jsonDecodedOrString parses tool-result text as JSON when it is valid and
// falls back to the raw string (dialect result bodies carry either).
func jsonDecodedOrString(text string) any {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return v
	}
	return text
}

func glmValue(raw json.RawMessage, key string) string {
	args, err := decodeArgsObject(raw)
	if err != nil {
		return ""
	}
	if s, ok := args[key].(string); ok {
		return s
	}
	enc, err := json.Marshal(args[key])
	if err != nil {
		return ""
	}
	return string(enc)
}

// glmValueIn decodes one <arg_value> body: JSON when it parses, raw text
// otherwise (GLM renders string values unquoted).
func glmValueIn(body string) any {
	if body == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err == nil {
		return v
	}
	return body
}

// xmlStringArgIn decodes a parameter body, honoring the `string` attribute
// override both the xml and minimax scanners accept.
func xmlStringArgIn(attrs, body string) any {
	override := strings.ToLower(strings.TrimSpace(attrValue("<x "+attrs+">", "string")))
	switch override {
	case "":
		return xmlArgValueIn(body)
	case "false", "0", "no":
		return xmlArgValueIn(body)
	default:
		return body
	}
}

func xmlArgValueIn(body string) any { return glmValueIn(strings.TrimSpace(body)) }

// xmlArgValue renders a parameter body: strings verbatim, everything else JSON.
func xmlArgValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	enc, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(enc)
}

// pythonKwargs renders a call's arguments as Python keyword arguments.
func pythonKwargs(args map[string]any) string {
	keys := sortedKeysOf(args)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+pythonValue(args[k]))
	}
	return strings.Join(parts, ", ")
}

func pythonValue(v any) string {
	switch tv := v.(type) {
	case string:
		enc, err := json.Marshal(tv)
		if err != nil {
			return `""`
		}
		return string(enc)
	case nil:
		return "None"
	case bool:
		if tv {
			return "True"
		}
		return "False"
	default:
		enc, err := json.Marshal(v)
		if err != nil {
			return "None"
		}
		return string(enc)
	}
}

// pythonValueIn decodes one Python literal back to a JSON value.
func pythonValueIn(raw string) any {
	switch raw {
	case "True", "true":
		return true
	case "False", "false":
		return false
	case "None", "null":
		return nil
	case "":
		return ""
	}
	if raw[0] == '"' || raw[0] == '\'' {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err == nil {
			return s
		}
		if len(raw) >= 2 {
			return raw[1 : len(raw)-1]
		}
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	return raw
}

// gemmaArgs renders a Gemma 4 brace body: key:value pairs, comma separated.
func gemmaArgs(args map[string]any) string {
	keys := sortedKeysOf(args)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+":"+gemmaValue(args[k]))
	}
	return strings.Join(parts, ",")
}

// gemmaValue renders one Gemma brace value (strings use the <|"|> delimiter).
func gemmaValue(v any) string {
	switch tv := v.(type) {
	case string:
		return gemmaStringMarker + tv + gemmaStringMarker
	case nil:
		return "null"
	case bool:
		if tv {
			return "true"
		}
		return "false"
	case json.RawMessage:
		return string(tv)
	case []any:
		parts := make([]string, 0, len(tv))
		for _, el := range tv {
			parts = append(parts, gemmaValue(el))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := sortedKeysOf(tv)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+":"+gemmaValue(tv[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		enc, err := json.Marshal(v)
		if err != nil {
			return "null"
		}
		return string(enc)
	}
}

// findGemmaBlockEnd returns the index of the closing <tool_call|> marker,
// skipping over <|"|> string spans.
func findGemmaBlockEnd(body string) int {
	for i := 0; i < len(body); {
		if strings.HasPrefix(body[i:], gemmaStringMarker) {
			next := strings.Index(body[i+len(gemmaStringMarker):], gemmaStringMarker)
			if next < 0 {
				return -1
			}
			i += len(gemmaStringMarker) + next + len(gemmaStringMarker)
			continue
		}
		if strings.HasPrefix(body[i:], gemmaCallClose) {
			return i
		}
		i++
	}
	return -1
}

// parseGemmaCall parses `call:NAME{...}` with depth/string-aware splitting.
func parseGemmaCall(block string) (string, map[string]any, error) {
	block = strings.TrimSpace(block)
	body := strings.TrimPrefix(block, "call:")
	open := strings.Index(body, "{")
	if open < 0 {
		return "", nil, fmt.Errorf("gemma: call block %q has no {", block)
	}
	name := strings.TrimSpace(body[:open])
	braceBody, err := balancedBraceBody(body[open:])
	if err != nil {
		return "", nil, fmt.Errorf("gemma: call %s: %w", name, err)
	}
	args := map[string]any{}
	for _, pair := range splitTopLevel(braceBody, ',') {
		key, val, ok := cutTopLevel(pair, ':')
		if !ok {
			continue
		}
		args[strings.TrimSpace(key)] = gemmaValueIn(strings.TrimSpace(val))
	}
	return name, args, nil
}

// balancedBraceBody returns the content of the brace group that starts at s[0],
// skipping string spans.
func balancedBraceBody(s string) (string, error) {
	depth := 0
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], gemmaStringMarker) {
			next := strings.Index(s[i+len(gemmaStringMarker):], gemmaStringMarker)
			if next < 0 {
				return "", fmt.Errorf("unterminated string span")
			}
			i += len(gemmaStringMarker) + next + len(gemmaStringMarker)
			continue
		}
		switch s[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return s[1:i], nil
			}
		}
		i++
	}
	return "", fmt.Errorf("unterminated brace body")
}

// gemmaValueIn decodes one brace value.
func gemmaValueIn(raw string) any {
	if strings.HasPrefix(raw, gemmaStringMarker) && strings.HasSuffix(raw, gemmaStringMarker) && len(raw) >= 2*len(gemmaStringMarker) {
		return raw[len(gemmaStringMarker) : len(raw)-len(gemmaStringMarker)]
	}
	if strings.HasPrefix(raw, "{") {
		if body, err := balancedBraceBody(raw); err == nil {
			out := map[string]any{}
			for _, pair := range splitTopLevel(body, ',') {
				k, v, ok := cutTopLevel(pair, ':')
				if !ok {
					continue
				}
				out[strings.TrimSpace(k)] = gemmaValueIn(strings.TrimSpace(v))
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		inner := raw[1 : len(raw)-1]
		out := []any{}
		for _, el := range splitTopLevel(inner, ',') {
			if strings.TrimSpace(el) == "" {
				continue
			}
			out = append(out, gemmaValueIn(strings.TrimSpace(el)))
		}
		return out
	}
	switch raw {
	case "true":
		return true
	case "false":
		return false
	case "null", "none":
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	return raw
}

// splitTopLevel splits on sep outside brackets and string spans.
func splitTopLevel(s string, sep byte) []string {
	var (
		out   []string
		depth int
		start int
	)
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], gemmaStringMarker) {
			next := strings.Index(s[i+len(gemmaStringMarker):], gemmaStringMarker)
			if next < 0 {
				break
			}
			i += len(gemmaStringMarker) + next + len(gemmaStringMarker)
			continue
		}
		switch s[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
		i++
	}
	out = append(out, s[start:])
	return out
}

// cutTopLevel splits on the first sep that is outside brackets/strings.
func cutTopLevel(s string, sep byte) (string, string, bool) {
	depth := 0
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], gemmaStringMarker) {
			next := strings.Index(s[i+len(gemmaStringMarker):], gemmaStringMarker)
			if next < 0 {
				return "", "", false
			}
			i += len(gemmaStringMarker) + next + len(gemmaStringMarker)
			continue
		}
		switch s[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case sep:
			if depth == 0 {
				return s[:i], s[i+1:], true
			}
		}
		i++
	}
	return "", "", false
}

// attrValue reads one named attribute out of a tag; missing returns "".
func attrValue(tag, name string) string {
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		if m[1] == name {
			return xmlUnescape(m[2])
		}
	}
	return ""
}

// splitFuncName reads a Harmony function name out of a header section.
func splitFuncName(rest string) (string, string) {
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '<', ' ', '\n', '\t', '\r':
			return rest[:i], rest[i:]
		}
	}
	return rest, ""
}

// indexAny returns the first index among markers and the marker that matched.
func indexAny(s string, markers ...string) (int, string) {
	best, bestMarker := -1, ""
	for _, m := range markers {
		i := strings.Index(s, m)
		if i < 0 {
			continue
		}
		if best < 0 || i < best {
			best, bestMarker = i, m
		}
	}
	return best, bestMarker
}

func xmlEscapeAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

func xmlEscapeText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func xmlUnescape(s string) string {
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

// sortedKeys returns the argument names of a call in sorted order (stable wire
// text for a given call).
func sortedKeys(c ToolCallBlock) []string {
	args, err := decodeArgsObject(c.Arguments)
	if err != nil {
		return nil
	}
	return sortedKeysOf(args)
}

func sortedKeysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
