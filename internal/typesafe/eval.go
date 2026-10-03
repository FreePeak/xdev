package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// defaultBaseURL is the hosted API root. baseURL remains a test seam
// for tests that construct Evaluator directly.
const defaultBaseURL = "https://api.typesafe.ai"

var baseURL = defaultBaseURL

// resolveBaseURL applies endpoint precedence after config.LoadEnv runs:
// explicit settings, then TYPESAFE_BASE_URL, then the hosted API.
func resolveBaseURL(configured string) string {
	if v := strings.TrimSpace(configured); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("TYPESAFE_BASE_URL")); v != "" {
		return v
	}
	return baseURL
}

// systemOnePath is the System One evaluation endpoint.
const systemOnePath = "/v1/systemone"

// DefaultModel is the model the tool sends when the caller omits one.
// Local Laya servers accept "english", "multilingual", or
// "typed-decisions"; the hosted Jev API accepts "jev-latest".
const DefaultModel = "jev-latest"

// DefaultTimeout bounds a single request to the API.
const DefaultTimeout = 10 * time.Second

// EvalRequest is the payload POST /v1/systemone expects.
//
// State is `any`, not a map, because a single string of text is the one state
// that must travel as itself. Every other System One caller — agentloop's
// guardrail battery, agent-decision-mcp's router, the local Laya sidecar's
// own docstring — posts a bare string, and a bare string is what the local
// backend scores as prose. Wrapping it in an object makes the model read
// `{"text": "..."}` instead, which is a different input and a different
// answer: the same question against the same live sidecar returned
// {0: 0.0369, 1: 0.0261, 2: 0.9369} for a string and
// {0: 0.0201, 1: 0.0161, 2: 0.9637} for the same text wrapped in one key.
// A genuinely multi-part state stays an object, which is what it is for.
type EvalRequest struct {
	State     any            `json:"state"`
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
	client     *http.Client
	baseURL    string
	model      string
	key        string
	httpClient *http.Client // override for tests; nil means use client
}

// NewEvaluator builds an evaluator from a configured settings block.
func NewEvaluator(s Settings) *Evaluator {
	return &Evaluator{
		client:     &http.Client{Timeout: s.Timeout},
		baseURL:    resolveBaseURL(s.BaseURL),
		model:      s.Model,
		key:        s.APIKey,
		httpClient: nil,
	}
}

// TestEvaluatorHTTP sets an alternate http.Client for Evaluate.
// Tests build a request through Evaluate without touching baseURL.
func (e *Evaluator) TestEvaluatorHTTP(c *http.Client) { e.httpClient = c }

// Evaluate sends state + questions to System One and returns the parsed answer
// map. State is normalized here rather than at the tool boundary so every
// caller — the tool, a future agent-loop gate — sends the same wire.
func (e *Evaluator) Evaluate(ctx context.Context, state any, questions map[string]any) (map[string]any, error) {
	payload := EvalRequest{
		State:     NormalizeState(state),
		Model:     e.model,
		Questions: questions,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("typesafe: marshal request: %w", err)
	}

	root := e.baseURL
	if strings.TrimSpace(root) == "" {
		root = baseURL
	}
	endpoint, err := url.JoinPath(root, systemOnePath)
	if err != nil {
		return nil, fmt.Errorf("typesafe: build URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("typesafe: build request: %w", err)
	}
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	req.Header.Set("Content-Type", "application/json")

	client := e.client
	if e.httpClient != nil {
		client = e.httpClient
	}
	resp, err := client.Do(req)
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
// one line per question with its value, bounded by truncateLimit.
func FormatResult(answers map[string]any) string {
	if len(answers) == 0 {
		return "(typesafe: no answers)"
	}
	var buf bytes.Buffer
	buf.WriteString(gateHint(answers))
	for k, v := range answers {
		line := fmt.Sprintf("  %s: %v", k, v)
		buf.WriteString(truncate(line, truncateLimit))
		buf.WriteString("\n")
	}
	return buf.String()
}

// gateHint names the field to gate on, and only says so when this answer
// actually carries it. `confidence` on choice and score is one minus
// normalised entropy — how concentrated the distribution is — and carries no
// calibration guarantee, so a threshold read off it does not mean what it
// appears to mean. But Laya 0.3.23 returns only `confidence`, and telling a
// model to gate on a field that is not in the payload is a confident wrong
// answer, not a useful hint; there, say plainly that there is nothing to gate
// on rather than naming a missing key.
func gateHint(answers map[string]any) string {
	for _, v := range answers {
		a, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := a["answer_confidence"]; ok {
			return "TypeSafe answers (gate on answer_confidence, not confidence):\n"
		}
	}
	return "TypeSafe answers (this backend returned no answer_confidence — only confidence, which is NOT calibrated; treat every answer as uncalibrated):\n"
}

// truncateLimit bounds one answer line so a pathological payload cannot eat the
// window. 200 was not enough for a real choice answer: the twenty-option answer
// a local Laya returned renders at 388 chars, and the cut landed at
// `config:0.0151`, taking the other twelve options — including
// `test:0.9263`, the winner's own probability. Go's %v sorts map keys, so the
// loss is always the tail of `probabilities`: the distribution a caller needs
// in order to tell a decisive answer from a flat one. 1024 holds that answer
// whole.
const truncateLimit = 1024

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
