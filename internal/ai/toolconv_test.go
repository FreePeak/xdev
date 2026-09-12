package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResolveToolFormat pins the model-id -> dialect matrix, including the
// native default for unknown models.
func TestResolveToolFormat(t *testing.T) {
	cases := []struct {
		model string
		want  ToolFormat
	}{
		{"deepseek-v3.1", ToolFormatDeepSeek},
		{"deepseek-ai/DeepSeek-R1-0528", ToolFormatDeepSeek},
		{"zai-org/GLM-4.5-Air", ToolFormatGlm},
		{"glm-4.6", ToolFormatGlm},
		{"openai/gpt-oss-120b", ToolFormatHarmony},
		{"NousResearch/Hermes-3-Llama-3.1-8B", ToolFormatHermes},
		{"moonshotai/Kimi-K2-Instruct", ToolFormatKimi},
		{"MiniMax-M2", ToolFormatMinimax},
		{"Qwen/Qwen3-8B", ToolFormatQwen3},
		{"qwq-32b", ToolFormatQwen3},
		{"google/gemma-4-E2B-it", ToolFormatGemma},
		{"claude-sonnet-4", ToolFormatNative},
		{"gpt-5", ToolFormatNative},
		{"gemini-2.5-pro", ToolFormatNative},
	}
	for _, tc := range cases {
		if got := ResolveToolFormat(tc.model); got != tc.want {
			t.Fatalf("ResolveToolFormat(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
	if !SupportsNativeToolCalls(ToolFormatNative) {
		t.Fatal("native must support structured tool calls")
	}
	if SupportsNativeToolCalls(ToolFormatHermes) {
		t.Fatal("hermes must be an in-band dialect")
	}
}

// TestParseToolFormat pins the configured-name normalization and its error.
func TestParseToolFormat(t *testing.T) {
	for name, want := range map[string]ToolFormat{
		"": ToolFormatNative, "native": ToolFormatNative, "pi-native": ToolFormatNative,
		"qwen": ToolFormatQwen3, "qwen3": ToolFormatQwen3, "glm-4.5": ToolFormatGlm,
		"kimi-k2": ToolFormatKimi, "xml": ToolFormatXML, "gemma": ToolFormatGemma,
	} {
		got, err := ParseToolFormat(name)
		if err != nil || got != want {
			t.Fatalf("ParseToolFormat(%q) = %q, %v", name, got, err)
		}
	}
	if _, err := ParseToolFormat("bogus"); err == nil || !strings.Contains(err.Error(), "qwen3") {
		t.Fatalf("err = %v, want an error naming the valid formats", err)
	}
}

// dialectFixtures are the reference streams from the toolconv docs, one per
// dialect, used to pin the decoder against real wire text.
var dialectFixtures = []struct {
	format ToolFormat
	name   string
	text   string
	args   string
}{
	{
		format: ToolFormatDeepSeek,
		name:   "get_weather",
		text:   "<\uFF5Ctool\u2581calls\u2581begin\uFF5C><\uFF5Ctool\u2581call\u2581begin\uFF5C>get_weather<\uFF5Ctool\u2581sep\uFF5C>{\"location\": \"San Francisco, CA\"}<\uFF5Ctool\u2581call\u2581end\uFF5C><\uFF5Ctool\u2581calls\u2581end\uFF5C>",
		args:   `{"location":"San Francisco, CA"}`,
	},
	{
		format: ToolFormatGlm,
		name:   "get_weather",
		text:   "<tool_call>get_weather\n<arg_key>location</arg_key>\n<arg_value>Beijing</arg_value>\n<arg_key>days</arg_key>\n<arg_value>3</arg_value>\n<arg_key>verbose</arg_key>\n<arg_value>true</arg_value>\n</tool_call>",
		args:   `{"days":3,"location":"Beijing","verbose":true}`,
	},
	{
		format: ToolFormatHarmony,
		name:   "get_current_weather",
		text:   `<|start|>assistant<|channel|>commentary to=functions.get_current_weather<|message|>{"location":"San Francisco, CA"}<|call|>`,
		args:   `{"location":"San Francisco, CA"}`,
	},
	{
		format: ToolFormatHermes,
		name:   "get_stock_fundamentals",
		text:   "<tool_call>\n{\"name\": \"get_stock_fundamentals\", \"arguments\": {\"symbol\": \"TSLA\"}}\n</tool_call>",
		args:   `{"symbol":"TSLA"}`,
	},
	{
		format: ToolFormatQwen3,
		name:   "get_weather",
		text:   "<tool_call>\n{\"name\": \"get_weather\", \"arguments\": \"{\\\"city\\\": \\\"Beijing\\\"}\"}\n</tool_call>",
		args:   `{"city":"Beijing"}`,
	},
	{
		format: ToolFormatKimi,
		name:   "get_weather",
		text:   `<|tool_calls_section_begin|><|tool_call_begin|>functions.get_weather:0<|tool_call_argument_begin|>{"city": "Beijing"}<|tool_call_end|><|tool_calls_section_end|>`,
		args:   `{"city":"Beijing"}`,
	},
	{
		format: ToolFormatMinimax,
		name:   "read",
		text:   "<minimax:tool_call>\n<invoke name=\"read\"><parameter name=\"path\">src/main.ts</parameter><parameter name=\"count\">40</parameter></invoke>\n</minimax:tool_call>",
		args:   `{"count":40,"path":"src/main.ts"}`,
	},
	{
		format: ToolFormatXML,
		name:   "read",
		text:   "<invoke name=\"read\"><parameter name=\"path\">src/main.ts</parameter><parameter name=\"count\">40</parameter></invoke>",
		args:   `{"count":40,"path":"src/main.ts"}`,
	},
	{
		format: ToolFormatGemma,
		name:   "get_current_temperature",
		text:   `<|tool_call>call:get_current_temperature{location:<|"|>London<|"|>,days:3}<tool_call|>`,
		args:   `{"days":3,"location":"London"}`,
	},
	{
		format: ToolFormatGemini,
		name:   "get_current_temperature",
		text:   "```tool_code\nprint(default_api.get_current_temperature(location=\"London\", unit=\"celsius\"))\n```",
		args:   `{"location":"London","unit":"celsius"}`,
	},
}

// TestDecodeToolCallsFixtures pins each decoder against its dialect's reference
// text, including that the envelope is stripped from the visible text.
func TestDecodeToolCallsFixtures(t *testing.T) {
	for _, tc := range dialectFixtures {
		text := "preamble " + tc.text + " trailer"
		calls, residual, err := DecodeToolCalls(tc.format, text)
		if err != nil {
			t.Fatalf("%s: DecodeToolCalls: %v", tc.format, err)
		}
		if len(calls) != 1 {
			t.Fatalf("%s: calls = %+v, want one", tc.format, calls)
		}
		if calls[0].Name != tc.name {
			t.Fatalf("%s: name = %q, want %q", tc.format, calls[0].Name, tc.name)
		}
		if tc.args != "" {
			assertJSONEqual(t, string(tc.format), calls[0].Arguments, tc.args)
		}
		if calls[0].ID == "" {
			t.Fatalf("%s: call has no id (ids must be synthesized)", tc.format)
		}
		if strings.Contains(residual, string(tc.format)) {
			t.Fatalf("%s: residual = %q, want the envelope removed", tc.format, residual)
		}
		if strings.Contains(residual, "<tool_call") || strings.Contains(residual, "default_api.") {
			t.Fatalf("%s: residual = %q, want no call markup left", tc.format, residual)
		}
		if !strings.Contains(residual, "preamble") || !strings.Contains(residual, "trailer") {
			t.Fatalf("%s: residual = %q, want the surrounding text kept", tc.format, residual)
		}
	}
}

// TestGLMDecodeKeepsBareStrings pins GLM's unquoted string values against a
// JSON-decodable number/boolean in the same call.
func TestGLMDecodeKeepsBareStrings(t *testing.T) {
	text := "<tool_call>get_weather\n<arg_key>location</arg_key>\n<arg_value>Beijing</arg_value>\n<arg_key>days</arg_key>\n<arg_value>3</arg_value>\n</tool_call>"
	calls, _, err := DecodeToolCalls(ToolFormatGlm, text)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertJSONEqual(t, "glm", calls[0].Arguments, `{"days":3,"location":"Beijing"}`)
}

// TestParallelCallsPerDialect pins that consecutive call envelopes decode to
// one call each.
func TestParallelCallsPerDialect(t *testing.T) {
	for _, tc := range []struct {
		format ToolFormat
		text   string
	}{
		{ToolFormatKimi, `<|tool_calls_section_begin|><|tool_call_begin|>functions.get_weather:0<|tool_call_argument_begin|>{"city":"Beijing"}<|tool_call_end|><|tool_call_begin|>functions.get_weather:1<|tool_call_argument_begin|>{"city":"Shanghai"}<|tool_call_end|><|tool_calls_section_end|>`},
		{ToolFormatMinimax, "<minimax:tool_call>\n<invoke name=\"read\"><parameter name=\"path\">a</parameter></invoke>\n<invoke name=\"read\"><parameter name=\"path\">b</parameter></invoke>\n</minimax:tool_call>"},
		{ToolFormatXML, "<tool_calls>\n<invoke name=\"read\"><parameter name=\"path\">a</parameter></invoke>\n<invoke name=\"read\"><parameter name=\"path\">b</parameter></invoke>\n</tool_calls>"},
		{ToolFormatGemma, `<|tool_call>call:read{path:<|"|>a<|"|>}<tool_call|><|tool_call>call:read{path:<|"|>b<|"|>}<tool_call|>`},
		{ToolFormatDeepSeek, "<\uFF5Ctool\u2581calls\u2581begin\uFF5C><\uFF5Ctool\u2581call\u2581begin\uFF5C>read<\uFF5Ctool\u2581sep\uFF5C>{\"path\":\"a\"}<\uFF5Ctool\u2581call\u2581end\uFF5C><\uFF5Ctool\u2581call\u2581begin\uFF5C>read<\uFF5Ctool\u2581sep\uFF5C>{\"path\":\"b\"}<\uFF5Ctool\u2581call\u2581end\uFF5C><\uFF5Ctool\u2581calls\u2581end\uFF5C>"},
		{ToolFormatHermes, "<tool_call>\n{\"name\":\"read\",\"arguments\":{\"path\":\"a\"}}\n</tool_call>\n<tool_call>\n{\"name\":\"read\",\"arguments\":{\"path\":\"b\"}}\n</tool_call>"},
		{ToolFormatHarmony, `<|start|>assistant<|channel|>commentary to=functions.read<|message|>{"path":"a"}<|call|><|start|>assistant<|channel|>commentary to=functions.read<|message|>{"path":"b"}<|call|>`},
		{ToolFormatGemini, "```tool_code\n[default_api.read(path=\"a\"), default_api.read(path=\"b\")]\n```"},
	} {
		calls, residual, err := DecodeToolCalls(tc.format, tc.text)
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.format, err)
		}
		if len(calls) != 2 {
			t.Fatalf("%s: calls = %+v, want two", tc.format, calls)
		}
		if residual != "" {
			t.Fatalf("%s: residual = %q, want empty", tc.format, residual)
		}
	}
}

// TestRoundTripPerDialect encodes a two-call batch in each dialect and decodes
// it back to the same names and arguments.
func TestRoundTripPerDialect(t *testing.T) {
	calls := []ToolCallBlock{
		{ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"path":"notes/a & b.txt","limit":40}`), StreamIndex: 0},
		{ID: "call_2", Name: "bash", Arguments: json.RawMessage(`{"command":"ls -la","env":{"A":"1"},"list":["x","y"],"flag":true}`), StreamIndex: 1},
	}
	for _, f := range ToolFormats() {
		if SupportsNativeToolCalls(f) {
			if text, err := EncodeToolCalls(f, calls); text != "" || err != nil {
				t.Fatalf("%s: encoded %q, %v; want no text form", f, text, err)
			}
			continue
		}
		text, err := EncodeToolCalls(f, calls)
		if err != nil {
			t.Fatalf("%s: encode: %v", f, err)
		}
		if text == "" {
			t.Fatalf("%s: encoded text is empty", f)
		}
		got, residual, err := DecodeToolCalls(f, text)
		if err != nil {
			t.Fatalf("%s: decode: %v", f, err)
		}
		if len(got) != len(calls) {
			t.Fatalf("%s: round trip produced %d calls, want %d (%q)", f, len(got), len(calls), text)
		}
		if residual != "" {
			t.Fatalf("%s: residual = %q, want empty", f, residual)
		}
		for i := range calls {
			if got[i].Name != calls[i].Name {
				t.Fatalf("%s: call %d name = %q, want %q", f, i, got[i].Name, calls[i].Name)
			}
			assertJSONEqual(t, string(f), got[i].Arguments, string(calls[i].Arguments))
		}
	}
}

// TestRoundTripToolResults pins that each dialect's result rendering is stable
// and carries the result text.
func TestRoundTripToolResults(t *testing.T) {
	results := []Message{
		{Role: RoleToolResult, ToolName: "read", ToolCallID: "call_1", Content: []Block{TextBlock{Text: "file contents"}}},
		{Role: RoleToolResult, ToolName: "bash", Content: []Block{TextBlock{Text: `{"stdout":"ok"}`}}},
	}
	// Dialects whose result envelope carries the tool name; deepseek, glm,
	// qwen3, xml and gemini correlate results positionally instead.
	named := map[ToolFormat]bool{
		ToolFormatHermes: true, ToolFormatKimi: true, ToolFormatMinimax: true,
		ToolFormatGemma: true, ToolFormatHarmony: true,
	}
	for _, f := range ToolFormats() {
		if SupportsNativeToolCalls(f) {
			continue
		}
		text := EncodeToolResults(f, results)
		if text == "" {
			t.Fatalf("%s: result text is empty", f)
		}
		if !strings.Contains(text, "file contents") {
			t.Fatalf("%s: result text = %q, want the result body", f, text)
		}
		if named[f] && !strings.Contains(text, "read") {
			t.Fatalf("%s: result text = %q, want the tool name", f, text)
		}
	}
}

// TestDecodeRejectsBrokenArguments pins the untrusted-input boundary: malformed
// JSON arguments are an error, not a silently empty call.
func TestDecodeRejectsBrokenArguments(t *testing.T) {
	if _, _, err := DecodeToolCalls(ToolFormatDeepSeek, "<\uFF5Ctool\u2581calls\u2581begin\uFF5C><\uFF5Ctool\u2581call\u2581begin\uFF5C>read<\uFF5Ctool\u2581sep\uFF5C>{not json}<\uFF5Ctool\u2581call\u2581end\uFF5C><\uFF5Ctool\u2581calls\u2581end\uFF5C>"); err == nil {
		t.Fatal("deepseek: want an error on broken arguments")
	}
	if _, _, err := DecodeToolCalls(ToolFormatHermes, `<tool_call>{"name":"read","arguments":{"path":`); err == nil {
		t.Fatal("hermes: want an error on an unterminated block")
	}
	if _, _, err := DecodeToolCalls(ToolFormatHermes, `<tool_call>not json</tool_call>`); err == nil {
		t.Fatal("hermes: want an error on a non-JSON body")
	}
}

// TestDecodeRepairsFencedArguments pins the code-fence wrapper some models leak
// around DeepSeek arguments.
func TestDecodeRepairsFencedArguments(t *testing.T) {
	text := "<\uFF5Ctool\u2581calls\u2581begin\uFF5C><\uFF5Ctool\u2581call\u2581begin\uFF5C>get_weather<\uFF5Ctool\u2581sep\uFF5C>```json\n{\"location\": \"SF\"}\n```<\uFF5Ctool\u2581call\u2581end\uFF5C><\uFF5Ctool\u2581calls\u2581end\uFF5C>"
	calls, _, err := DecodeToolCalls(ToolFormatDeepSeek, text)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertJSONEqual(t, "deepseek", calls[0].Arguments, `{"location":"SF"}`)
}

// TestEncodeGemmaStringDelimiter pins the Gemma string encoding on the wire.
func TestEncodeGemmaStringDelimiter(t *testing.T) {
	text, err := EncodeToolCalls(ToolFormatGemma, []ToolCallBlock{{
		Name: "get_current_temperature", Arguments: json.RawMessage(`{"days":3,"location":"London"}`),
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `<|tool_call>call:get_current_temperature{days:3,location:<|"|>London<|"|>}<tool_call|>`
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

// TestEncodeDeepSeekBatch pins the batch envelope and its literal tokens.
func TestEncodeDeepSeekBatch(t *testing.T) {
	text, err := EncodeToolCalls(ToolFormatDeepSeek, []ToolCallBlock{
		{Name: "get_weather", Arguments: json.RawMessage(`{"location": "San Francisco, CA"}`)},
		{Name: "get_weather", Arguments: json.RawMessage(`{"location": "Seattle, WA"}`)},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := "<\uFF5Ctool\u2581calls\u2581begin\uFF5C>" +
		"<\uFF5Ctool\u2581call\u2581begin\uFF5C>get_weather<\uFF5Ctool\u2581sep\uFF5C>{\"location\": \"San Francisco, CA\"}<\uFF5Ctool\u2581call\u2581end\uFF5C>" +
		"<\uFF5Ctool\u2581call\u2581begin\uFF5C>get_weather<\uFF5Ctool\u2581sep\uFF5C>{\"location\": \"Seattle, WA\"}<\uFF5Ctool\u2581call\u2581end\uFF5C>" +
		"<\uFF5Ctool\u2581calls\u2581end\uFF5C>"
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

// TestKimiCallIDPreserved pins that a wire-form id survives re-encoding.
func TestKimiCallIDPreserved(t *testing.T) {
	text, err := EncodeToolCalls(ToolFormatKimi, []ToolCallBlock{{
		ID: "functions.get_weather:7", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Beijing"}`),
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(text, "functions.get_weather:7<|tool_call_argument_begin|>") {
		t.Fatalf("text = %q, want the existing call id preserved", text)
	}
}

// TestToolFormatPrompt pins that the in-band prompt carries the catalog (one
// OpenAI tool object per line) and the dialect guide, and that native has none.
func TestToolFormatPrompt(t *testing.T) {
	tools := []ToolDef{
		{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		{Name: "bash", Description: "Run a command", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
	}
	if got := ToolFormatPrompt(ToolFormatNative, tools); got != "" {
		t.Fatalf("native prompt = %q, want empty", got)
	}
	for _, f := range []ToolFormat{ToolFormatHermes, ToolFormatDeepSeek, ToolFormatGlm, ToolFormatHarmony, ToolFormatKimi, ToolFormatMinimax, ToolFormatXML, ToolFormatGemma, ToolFormatQwen3, ToolFormatGemini} {
		got := ToolFormatPrompt(f, tools)
		if !strings.Contains(got, "<tools>") || !strings.Contains(got, `"name":"read"`) {
			t.Fatalf("%s: prompt = %q, want the tool catalog", f, got)
		}
		if !strings.Contains(got, "IMPORTANT") && !strings.Contains(got, "Emit") {
			t.Fatalf("%s: prompt = %q, want the format guide", f, got)
		}
	}
}

// assertJSONEqual compares two JSON documents semantically.
func assertJSONEqual(t *testing.T, label string, got json.RawMessage, want string) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("%s: decode got %s: %v", label, got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantVal); err != nil {
		t.Fatalf("%s: decode want %s: %v", label, want, err)
	}
	gotText, _ := json.Marshal(gotVal)
	wantText, _ := json.Marshal(wantVal)
	if string(gotText) != string(wantText) {
		t.Fatalf("%s: arguments = %s, want %s", label, gotText, wantText)
	}
}
