package typesafe

import (
	"context"
	"encoding/json"

	"github.com/FreePeak/xdev/internal/tool"
)

// ToolName is the name the model calls.
const ToolName = "typesafe"

// Tool evaluates text/state against typed questions using TypeSafe's
// hosted System One API or a self-hosted Laya-compatible endpoint. It
// returns calibrated probabilities and typed answers the code can combine.
//
// Arguments:
//   - state: the content to evaluate (string, object, or array)
//   - questions: map of typed questions (noul/choice/score)
//   - model (optional): model id; hosted defaults to "jev-latest"; Laya accepts its checkpoint names
//
// TYPESAFE_BASE_URL or typesafe.baseUrl selects the endpoint.
// TYPESAFE_API_KEY or typesafe.apiKey is optional for keyless local servers.
type Tool struct {
	Settings Settings
}

// NewTool builds the tool from a settings block.
func NewTool(s Settings) *Tool {
	return &Tool{Settings: s.Config()}
}

func (t *Tool) Name() string { return ToolName }

func (t *Tool) Description() string {
	return "evaluate text/state against typed questions using TypeSafe System One or a self-hosted Laya endpoint; returns calibrated probabilities and typed answers"
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
      "description": "map of typed questions keyed by question id; each has type (noul|choice|score), instructions, and criteria. choice takes criteria as an object keyed by option label; score takes a LIST of level descriptions, index 0 first; noul takes either or omits criteria. Gate on the returned answer_confidence, not confidence."
    },
    "model": {
      "type": "string",
      "description": "model that handles the request (hosted default: jev-latest; Laya auto-routes when omitted)"
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
	answers, info, err := ev.EvaluateWithInfo(ctx, a.State, a.Questions)
	if err != nil {
		return tool.Result{Text: err.Error(), IsError: true}, nil
	}
	return tool.Result{Text: FormatResultWithInfo(answers, info)}, nil
}

// NormalizeState coerces the tool argument (which the model may send as a
// plain string, a JSON object, or an array of text) into the shape
// POST /v1/systemone expects.
//
// A string travels as itself. It used to be wrapped as {"text": …}, on the
// assumption that System One wants a state object — but the local backend
// serialises a dict with json.dumps before tokenizing, so the model reads the
// JSON blob instead of the prose and answers a different question. Measured
// against a live sidecar, same text and same score question:
//
//	state as string          {0: 0.0369, 1: 0.0261, 2: 0.9369}  confidence 0.7468
//	state as {"text": …}     {0: 0.0201, 1: 0.0161, 2: 0.9637}  confidence 0.8355
//
// A map stays a map (a multi-part state is what an object is for), and so does
// a list; only nil collapses, because there is nothing to judge.
func NormalizeState(s any) any {
	if s == nil {
		return ""
	}
	return s
}

// compile-time interface assertion.
var _ tool.Tool = (*Tool)(nil)
