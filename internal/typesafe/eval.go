package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// baseURL is the API root; systemOnePath is appended to it. Kept as a
// base (not the full endpoint) so a test can point it at an httptest
// origin and still exercise the real path, and so TYPESAFE_BASE_URL
// semantics match the official SDKs.
var baseURL = "https://api.typesafe.ai"

// systemOnePath is the System One evaluation endpoint.
const systemOnePath = "/v1/systemone"

// EvalRequest is the payload POST /v1/systemone expects.
type EvalRequest struct {
	State     map[string]any `json:"state"`
	Model     string         `json:"model"`
	Questions map[string]any `json:"questions"`
}

// EvalResponse is the shape returned by the API; answers
// arrive as an opaque JSON object keyed by question id.
type EvalResponse struct {
	Model   string          `json:"model"`
	Answers json.RawMessage `json:"answers"`
	Usage   map[string]any  `json:"usage,omitempty"`
}

// Evaluator talks to the TypeSafe System One endpoint.
type Evaluator struct {
	client *http.Client
	model  string
	key    string
}

// NewEvaluator builds an evaluator from a configured settings block.
func NewEvaluator(s Settings) *Evaluator {
	return &Evaluator{
		client: &http.Client{Timeout: s.Timeout},
		model:  s.Model,
		key:    s.APIKey,
	}
}

// Evaluate sends state + questions to System One and returns the
// parsed answer map. An empty or unparseable body returns an empty map.
func (e *Evaluator) Evaluate(ctx context.Context, state map[string]any, questions map[string]any) (map[string]any, error) {
	payload := EvalRequest{
		State:     state,
		Model:     e.model,
		Questions: questions,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("typesafe: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+systemOnePath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("typesafe: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("typesafe: %d %s — %s", resp.StatusCode, resp.Status, string(b))
	}

	var result EvalResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("typesafe: decode response: %w", err)
	}

	return parseAnswers(result.Answers)
}

// parseAnswers extracts the answer map from a raw JSON value.
// Empty or null answers return an empty map.
func parseAnswers(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FormatResult renders typed answers for the model context —
// one line per question with its value, truncated to 200 chars.
func FormatResult(answers map[string]any) string {
	if len(answers) == 0 {
		return "(typesafe: no answers)"
	}
	var buf bytes.Buffer
	buf.WriteString("TypeSafe answers:\n")
	for k, v := range answers {
		line := fmt.Sprintf("  %s: %v", k, v)
		buf.WriteString(truncate(line, 200))
		buf.WriteString("\n")
	}
	return buf.String()
}

const truncateLimit = 200

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
