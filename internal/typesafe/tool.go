package typesafe

import (
	"context"
	"encoding/json"

	"github.com/FreePeak/xdev/internal/tool"
)

// ToolName is the name the model calls.
const ToolName = "typesafe"

// Tool evaluates text/state against typed questions using
// TypeSafe's System One models (Jev). It returns calibrated
// probabilities and typed answers the code can combine.
//
// Arguments:
//   - state: the content to evaluate (string, object, or array)
//   - questions: map of typed questions (noul/choice/score)
//   - model (optional): model id; defaults to "jev-latest"
//
// Requires TYPESAFE_API_KEY in the environment or
// typesafe.apiKey in ~/.xdev/agent/config.yml.
type Tool struct {
	Settings Settings
}

// NewTool builds the tool from a settings block.
func NewTool(s Settings) *Tool {
	return &Tool{Settings: s.Config()}
}

func (t *Tool) Name() string { return ToolName }

func (t *Tool) Description() string {
	return "evaluate text/state against typed questions using TypeSafe System One models (Jev); returns calibrated probabilities and typed answers"
}

func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "state": {
      "type": ["string", "object", "array"],
      "description": "the content to evaluate — text, a JSON object, or an array of text values"
    },
    "questions": {
      "type": "object",
      "description": "map of typed questions keyed by question id; each question has type (noul|choice|score), instructions, and criteria"
    },
    "model": {
      "type": "string",
      "description": "model that handles the request (default: jev-latest)"
    }
  },
  "required": ["state", "questions"]
}`)
}

type toolArgs struct {
	State     any            `json:"state"`
	Questions map[string]any `json:"questions"`
	Model     string         `json:"model,omitempty"`
}

func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a toolArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "typesafe: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if len(a.Questions) == 0 {
		return tool.Result{Text: "typesafe: questions is required (map of typed questions)", IsError: true}, nil
	}
	if a.State == nil {
		return tool.Result{Text: "typesafe: state is required (the content to evaluate)", IsError: true}, nil
	}
	s := t.Settings
	if a.Model != "" {
		s.Model = a.Model
	}
	ev := NewEvaluator(s)
	answers, err := ev.Evaluate(ctx, NormalizeState(a.State), a.Questions)
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	return tool.Result{Text: FormatResult(answers)}, nil
}

// NormalizeState coerces the tool argument (which the model
// may send as a plain string, JSON object, or JSON array of
// text) into the `map[string]any` shape POST /v1/systemone
// expects. A string becomes `{"text": state}`; a map is
// returned as-is; anything else is wrapped under "data".
func NormalizeState(s any) map[string]any {
	switch v := s.(type) {
	case nil:
		return map[string]any{}
	case string:
		return map[string]any{"text": v}
	case map[string]any:
		return v
	default:
		return map[string]any{"data": v}
	}
}

// compile-time interface assertion.
var _ tool.Tool = (*Tool)(nil)
