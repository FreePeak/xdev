package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEvaluateLocalSidecarWithoutAPIKey(t *testing.T) {
	t.Setenv("TYPESAFE_BASE_URL", "")
	t.Setenv("TYPESAFE_API_KEY", "hosted-secret")
	t.Setenv("LAYA_API_KEY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path: want /v1/systemone, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("auth: local sidecar should not need bearer, got %q", got)
		}
		var req EvalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "" {
			t.Errorf("model: want omitted for Laya auto-routing, got %q", req.Model)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "english",
			"answers": map[string]any{
				"is_urgent": map[string]any{"type": "noul", "noul": 0.92},
			},
			"usage": map[string]any{"local_ms": 12.3},
		})
	}))
	defer srv.Close()

	tl := NewTool(Settings{BaseURL: srv.URL})
	result, err := tl.Execute(context.Background(), json.RawMessage(`{"state":{"text":"refund now"},"questions":{"is_urgent":{"type":"noul","instructions":"urgent?"}}}`))
	if err != nil {
		t.Fatalf("execute local sidecar: %v", err)
	}
	if result.IsError || !strings.Contains(result.Text, "is_urgent") {
		t.Fatalf("local sidecar result = %+v", result)
	}
}

func TestResolveBaseURL(t *testing.T) {
	t.Setenv("TYPESAFE_BASE_URL", "http://env.example")
	if got := resolveBaseURL("http://config.example/"); got != "http://config.example/" {
		t.Fatalf("configured base URL: got %q", got)
	}
	if got := resolveBaseURL(""); got != "http://env.example" {
		t.Fatalf("env base URL: got %q", got)
	}
	t.Setenv("TYPESAFE_BASE_URL", "")
	if got := resolveBaseURL(""); got != baseURL {
		t.Fatalf("default base URL: got %q", got)
	}
}

func TestConfigDoesNotForwardHostedKeyToLocalEndpoint(t *testing.T) {
	t.Setenv("TYPESAFE_BASE_URL", "")
	t.Setenv("TYPESAFE_API_KEY", "hosted-secret")
	t.Setenv("LAYA_API_KEY", "")
	c := Settings{BaseURL: "http://127.0.0.1:8000", Model: "english"}.Config()
	if c.APIKey != "" {
		t.Fatalf("hosted key forwarded to local endpoint: %q", c.APIKey)
	}
}

func TestEvaluateOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Fatalf("path: want /v1/systemone, got %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Fatalf("auth: got %q", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content type: %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"state"`)) {
			t.Fatalf("body missing state: %s", body)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.13.0",
			"answers": map[string]any{
				"is_urgent": map[string]any{"type": "noul", "noul": 0.92},
			},
			"usage": map[string]any{"input_tokens": 100, "output_tokens": 10},
		})
	}))
	defer srv.Close()

	saved := baseURL
	baseURL = srv.URL
	defer func() { baseURL = saved }()

	e := &Evaluator{}
	e.TestEvaluatorHTTP(srv.Client())
	e.model = "jev-latest"
	e.key = "test-key"

	answers, err := e.Evaluate(context.Background(),
		map[string]any{"ticket": "payouts failing"},
		map[string]any{"is_urgent": map[string]any{"type": "noul", "instructions": "urgent?"}},
	)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if urg, ok := answers["is_urgent"].(map[string]any); !ok {
		t.Fatalf("answer type: %T", answers["is_urgent"])
	} else if urg["noul"] != 0.92 {
		t.Fatalf("noul: %v", urg["noul"])
	}
}

func TestEvaluateErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	saved := baseURL
	baseURL = srv.URL
	defer func() { baseURL = saved }()

	e := &Evaluator{}
	e.TestEvaluatorHTTP(srv.Client())
	e.model = "jev-latest"
	e.key = "bad-key"

	_, err := e.Evaluate(context.Background(),
		map[string]any{"state": "x"}, map[string]any{"q": map[string]any{"type": "noul"}})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !contains(err.Error(), "401") {
		t.Fatalf("error should mention 401: %s", err.Error())
	}
}

func TestEvaluateMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	saved := baseURL
	baseURL = srv.URL
	defer func() { baseURL = saved }()

	e := &Evaluator{}
	e.TestEvaluatorHTTP(srv.Client())
	e.model = "jev-latest"
	e.key = "k"

	_, err := e.Evaluate(context.Background(),
		map[string]any{"state": "x"}, map[string]any{"q": map[string]any{"type": "noul"}})
	if err == nil {
		t.Fatal("expected error for malformed body")
	}
}

func TestFormatResult(t *testing.T) {
	out := FormatResult(map[string]any{"is_urgent": map[string]any{"type": "noul", "noul": 0.92}})
	if !contains(out, "TypeSafe answers") {
		t.Fatalf("missing header: %s", out)
	}
	if !contains(out, "is_urgent") {
		t.Fatalf("missing key: %s", out)
	}
}

func TestFormatResultEmpty(t *testing.T) {
	if got := FormatResult(map[string]any{}); got != "(typesafe: no answers)" {
		t.Fatalf("empty: %s", got)
	}
}

func TestToolName(t *testing.T) {
	if ToolName != "typesafe" {
		t.Fatalf("want typesafe, got %s", ToolName)
	}
}

func TestToolExecuteMissingQuestions(t *testing.T) {
	_, err := NewTool(Settings{}).Execute(context.Background(), json.RawMessage(`{"state":"x"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSettingsDefaults(t *testing.T) {
	c := Settings{APIKey: "k"}.Config()
	if c.Model != DefaultModel {
		t.Fatalf("model: want %s, got %s", DefaultModel, c.Model)
	}
	if c.APIKey != "k" {
		t.Fatalf("api key: want k, got %s", c.APIKey)
	}
}

func TestParseAnswers(t *testing.T) {
	raw := json.RawMessage(`{"is_urgent":{"type":"noul","noul":0.92}}`)
	answers, err := parseAnswers(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if urg, ok := answers["is_urgent"].(map[string]any); !ok {
		t.Fatalf("type: %T", urg)
	} else if urg["noul"] != 0.92 {
		t.Fatalf("noul: %v", urg["noul"])
	}
}

func TestParseAnswersNull(t *testing.T) {
	out, err := parseAnswers(json.RawMessage("null"))
	if err != nil {
		t.Fatalf("null: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("want empty, got %d", len(out))
	}
}

// TestNormalizeStateKeepsAStringAString pins the wire shape, with the two live
// answers the wrapping used to change. The local backend json.dumps a dict
// before tokenizing, so {"text": "…"} is scored as a JSON blob rather than as
// the prose it carries.
func TestNormalizeStateKeepsAStringAString(t *testing.T) {
	if got := NormalizeState(nil); got != "" {
		t.Fatalf("nil: %v", got)
	}
	if got := NormalizeState("hello"); got != "hello" {
		t.Fatalf("a string must not be wrapped: %v", got)
	}
	m := map[string]any{"x": 1}
	if got := NormalizeState(m); got.(map[string]any)["x"] != 1 {
		t.Fatalf("map: %v", got)
	}
	if got := NormalizeState([]any{"a", "b"}); len(got.([]any)) != 2 {
		t.Fatalf("list: %v", got)
	}
}

// TestEvaluateSendsStringStateUnwrapped is the same property at the wire: the
// body a real call posts must carry the bare string, because the two shapes do
// not produce the same answer on the local backend.
func TestEvaluateSendsStringStateUnwrapped(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State any `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		s, ok := req.State.(string)
		if !ok {
			t.Errorf("state is %T, want the bare string", req.State)
		}
		got = s
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "english",
			"answers": map[string]any{"q": map[string]any{"type": "noul", "noul": 0.9}},
		})
	}))
	defer srv.Close()

	e := NewEvaluator(Settings{BaseURL: srv.URL, Model: "english"})
	if _, err := e.Evaluate(context.Background(),
		"The customer wants to cancel their subscription today and get a full refund.",
		map[string]any{"q": map[string]any{"type": "noul", "instructions": "urgent?"}}); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got != "The customer wants to cancel their subscription today and get a full refund." {
		t.Fatalf("state on the wire = %q", got)
	}
}

func TestRetryableStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"detail":"rate limited"}`))
	}))
	defer srv.Close()

	saved := baseURL
	baseURL = srv.URL
	defer func() { baseURL = saved }()

	e := &Evaluator{}
	e.TestEvaluatorHTTP(srv.Client())
	e.model = "jev-latest"
	e.key = "k"

	_, err := e.Evaluate(context.Background(),
		map[string]any{"state": "x"}, map[string]any{"q": map[string]any{"type": "noul"}})
	if err == nil {
		t.Fatal("expected error for 429")
	}
	if !contains(err.Error(), "429") {
		t.Fatalf("error should mention 429: %s", err.Error())
	}
}

func TestEvaluateEmptyAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "jev-latest",
			"answers": json.RawMessage("{}"),
		})
	}))
	defer srv.Close()

	saved := baseURL
	baseURL = srv.URL
	defer func() { baseURL = saved }()

	e := &Evaluator{}
	e.TestEvaluatorHTTP(srv.Client())
	e.model = "jev-latest"
	e.key = "k"

	answers, err := e.Evaluate(context.Background(),
		map[string]any{"state": "x"}, map[string]any{"q": map[string]any{"type": "noul"}})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(answers) != 0 {
		t.Fatalf("want empty answers, got %d", len(answers))
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

// TestFormatResultKeepsTheGatingNumber pins the two properties the `ask`
// confidence gate and the memory pre-filter depend on, against answers a real
// local Laya server returned (POST /v1/systemone, english checkpoint, MPS).
//
// The wide case is the one that earns the limit: a twenty-option choice
// renders at 388 chars, so the old 200-char cut landed inside `probabilities`
// and removed twelve options — the winner's own probability among them. The
// distribution is what separates a decisive answer from a flat one, so the tail
// is not decoration.
func TestFormatResultKeepsTheGatingNumber(t *testing.T) {
	// Every option verbatim from the live twenty-option answer, so the rendered
	// length is the measured one rather than an estimate of it.
	probs := map[string]any{
		"investigation": 0.0009, "fix": 0.0001, "docs": 0.0002, "research": 0.0,
		"config": 0.0151, "deploy": 0.02, "test": 0.9263, "perf": 0.0012,
		"refactor": 0.0036, "migration": 0.0061, "schema": 0.0001, "types": 0.0006,
		"lint": 0.0, "build": 0.0016, "ci": 0.0112, "release": 0.0083,
		"security": 0.001, "ux": 0.0019, "a11y": 0.0004, "misc": 0.0016,
	}
	wide := FormatResult(map[string]any{
		"owner": map[string]any{
			"type":              "choice",
			"choice":            "test",
			"probabilities":     probs,
			"confidence":        0.1926,
			"answer_confidence": 0.9263,
			"action":            map[string]any{"act_probability": 1.0},
		},
	})
	for option, p := range probs {
		if !contains(wide, fmt.Sprintf("%v:%v", option, p)) {
			t.Errorf("option %q lost to truncation: %q", option, wide)
		}
	}

	answers := map[string]any{
		"urgency": map[string]any{
			"type":              "score",
			"score":             1.8983,
			"legend":            map[string]any{"0": "no deadline mentioned", "1": "days", "2": "today or cancellation"},
			"probabilities":     map[string]any{"0": 0.0182, "1": 0.0654, "2": 0.9164},
			"confidence":        0.6986,
			"answer_confidence": 0.9164,
			"action":            map[string]any{"act_probability": 1.0},
		},
	}
	got := FormatResult(answers)
	if !contains(got, "answer_confidence") {
		t.Errorf("header does not name the calibrated field: %q", got)
	}
	if !contains(got, "0.9164") {
		t.Errorf("answer_confidence value lost to truncation: %q", got)
	}
	for _, tail := range []string{"today or cancellation", "0.0654"} {
		if !contains(got, tail) {
			t.Errorf("tail %q cut off: %q", tail, got)
		}
	}
	if contains(got, "…") || contains(wide, "…") {
		t.Errorf("a real answer was truncated: %q %q", got, wide)
	}
}

// TestFormatResultDoesNotNameAMissingField pins the Laya-shaped case, verbatim
// from a live local sidecar (laya 0.3.23, english checkpoint, POST
// /v1/systemone): the payload carries `confidence` and no `answer_confidence`.
// Naming the calibrated field anyway is a confident wrong answer — the model
// looks for a key that is not there — so the header must say the answer is
// uncalibrated instead.
func TestFormatResultDoesNotNameAMissingField(t *testing.T) {
	got := FormatResult(map[string]any{
		"urgency": map[string]any{
			"type":          "score",
			"score":         1.9436,
			"legend":        map[string]any{"0": "no deadline mentioned", "1": "days", "2": "today or cancellation"},
			"probabilities": map[string]any{"0": 0.0201, "1": 0.0161, "2": 0.9637},
			"confidence":    0.8355,
			"action":        map[string]any{"act_probability": 1.0},
		},
	})
	if contains(got, "gate on answer_confidence") {
		t.Errorf("header points at a field this backend never sent: %q", got)
	}
	for _, want := range []string{"uncalibrated", "0.9637", "today or cancellation"} {
		if !contains(got, want) {
			t.Errorf("result missing %q: %q", want, got)
		}
	}
}

// TestGateHintFollowsThePayload keeps the hint tied to the data, not to a
// constant: one answer carries the calibrated field and the hint follows it
// even though the sibling answer does not.
func TestGateHintFollowsThePayload(t *testing.T) {
	mixed := FormatResult(map[string]any{
		"a": map[string]any{"type": "noul", "noul": 0.5, "answer_confidence": 0.5},
		"b": map[string]any{"type": "noul", "noul": 0.5, "confidence": 0.5},
	})
	if !contains(mixed, "gate on answer_confidence") {
		t.Errorf("a payload carrying answer_confidence must name it: %q", mixed)
	}
}

// TestEvaluateWithInfoReportsTheAnsweringBackend pins the two fields the
// envelope carried and nothing read: with a lazy router the requested model is
// empty on purpose and the answering checkpoint is whatever the script routed
// to, so "which backend answered" and "what did the call cost" cannot be
// reconstructed from the answers. Verbatim from a live local sidecar.
func TestEvaluateWithInfoReportsTheAnsweringBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "english",
			"answers": map[string]any{"q": map[string]any{"type": "noul", "noul": 0.9}},
			"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0, "local_ms": 116.5},
		})
	}))
	defer srv.Close()

	ev := NewEvaluator(Settings{BaseURL: srv.URL})
	answers, info, err := ev.EvaluateWithInfo(context.Background(), "cancel today",
		map[string]any{"q": map[string]any{"type": "noul", "instructions": "urgent?"}})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(answers) != 1 {
		t.Fatalf("answers = %v", answers)
	}
	if info.Model != "english" {
		t.Errorf("answering model = %q", info.Model)
	}
	if info.Usage.LocalMS != 116.5 {
		t.Errorf("local_ms = %v", info.Usage.LocalMS)
	}

	out := FormatResultWithInfo(answers, info)
	for _, want := range []string{"answered by english", "116ms"} {
		if !contains(out, want) {
			t.Errorf("result missing %q: %q", want, out)
		}
	}
	// The zeroed token counters a local backend sends must not render as "0
	// tokens" — that reads as a cost, not as an absent one.
	if contains(out, "0 tokens") {
		t.Errorf("absent cost rendered as zero: %q", out)
	}
}

// TestToolSurfacesTheBackendThatAnswered is the tool-level half: a caller of
// the tool sees the footer, not just the answers. Fails on a build where the
// tool drops the Info the evaluator returned.
func TestToolSurfacesTheBackendThatAnswered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "english",
			"answers": map[string]any{"q": map[string]any{"type": "noul", "noul": 0.9}},
			"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0, "local_ms": 116.5},
		})
	}))
	defer srv.Close()

	res, err := NewTool(Settings{BaseURL: srv.URL}).Execute(context.Background(),
		json.RawMessage(`{"state":"cancel today","questions":{"q":{"type":"noul","instructions":"urgent?"}}}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool failed: %s", res.Text)
	}
	if !contains(res.Text, "answered by english") {
		t.Errorf("tool dropped the answering checkpoint: %q", res.Text)
	}
}

// TestFormatInfoOmittedWhenTheBackendSaysNothing keeps the footer absent rather
// than empty-looking, so an answer from a backend that reports no model and no
// cost reads exactly as it did before.
func TestFormatInfoOmittedWhenTheBackendSaysNothing(t *testing.T) {
	answers := map[string]any{"q": map[string]any{"type": "noul", "noul": 0.9}}
	if got, want := FormatResultWithInfo(answers, Info{}), FormatResult(answers); got != want {
		t.Errorf("no info changed the rendering:\n got %q\nwant %q", got, want)
	}
}
