// This file exposes the package as an agent tool, so the tts backend, chunker
// and settings resolution have exactly one implementation.
package tts

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/FreePeak/xdev/internal/tool"
)

// ToolName is the name the tool registers under.
const ToolName = "tts"

// Tool is the agent-callable front end: settings carry the session defaults,
// per-call arguments override them.
type Tool struct {
	Settings Settings
	Backend  Backend
	// Lookup resolves the backend binary on PATH (nil = exec.LookPath).
	// Test seam for stubbing the platform binary.
	Lookup func(string) (string, error)
}

// NewTool builds the tool with the configured voice/rate defaults. It
// succeeds on unsupported platforms with a nil Backend: Execute then reports
// the actionable error, so a registration call site never needs error
// handling and the model still learns the platform has no backend.
func NewTool(s Settings) *Tool {
	be, _ := ForGOOS("", nil)
	return &Tool{Settings: s, Backend: be}
}

func (t *Tool) Name() string { return ToolName }

func (t *Tool) Description() string {
	return "Speak text aloud through the local synthesizer (macOS say, Linux spd-say/espeak-ng, Windows PowerShell SAPI). Long text is chunked on sentence boundaries and played in order. Useful for accessibility, listening to a summary, or signalling that a long task finished; requires audio hardware."
}

func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"text": {
				"type": "string",
				"description": "Text to speak. Trailing text beyond 8192 bytes is dropped (reported in the result)."
			},
			"voice": {
				"type": "string",
				"description": "Voice name override, backend-specific (e.g. Samantha on macOS). Defaults to the tts.voice setting or the system voice."
			},
			"rate": {
				"type": "integer",
				"description": "Speaking rate in words per minute, 80-600. Defaults to the tts.rate setting or the backend default."
			},
			"dry_run": {
				"type": "boolean",
				"description": "Report the chunking without playing audio; spawns nothing. Use to preview how a long text will be split."
			}
		},
		"required": ["text"]
	}`)
}

type toolArgs struct {
	Text   string `json:"text"`
	Voice  string `json:"voice"`
	Rate   int    `json:"rate"`
	DryRun bool   `json:"dry_run"`
}

// Execute speaks the text. Malformed arguments and playback failures come
// back as IsError results (the loop renders Result.Text, so the model can
// correct itself); only the harness never seeing a tts tool at all is fatal.
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a toolArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "tts: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if t.Backend == nil {
		return tool.Result{Text: "tts: no local speech backend on this platform (supported: macOS, Linux, Windows)", IsError: true}, nil
	}
	if strings.TrimSpace(a.Text) == "" {
		return tool.Result{Text: "tts: text is required", IsError: true}, nil
	}
	if err := ValidateRate(a.Rate); err != nil {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	sp := Speaker{backend: t.Backend, Lookup: t.Lookup}
	req := merge(t.Settings, a.Text, a.Voice, a.Rate, a.DryRun)
	rep, err := sp.Speak(ctx, req)
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	detail := struct {
		Report
		Plan []string `json:"plan,omitempty"`
	}{Report: rep}
	if rep.DryRun {
		detail.Plan = sp.Plan(req)
	}
	buf, err := json.Marshal(detail)
	if err != nil {
		return tool.Result{Text: rep.Summary(), Details: rep}, nil
	}
	return tool.Result{Text: rep.Summary(), Details: json.RawMessage(buf)}, nil
}
