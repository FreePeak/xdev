package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEvaluateLocalSidecarWithoutAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Fatalf("path: want /v1/systemone, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("auth: local sidecar should not need bearer, got %q", got)
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

	t.Setenv("TYPESAFE_BASE_URL", "")
	t.Setenv("TYPESAFE_API_KEY", "hosted-secret")
	t.Setenv("LAYA_API_KEY", "")
	tl := NewTool(Settings{BaseURL: srv.URL, Model: "english"})
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

func TestNormalizeState(t *testing.T) {
	if got := NormalizeState(nil); len(got) != 0 {
		t.Fatalf("nil: %v", got)
	}
	if got := NormalizeState("hello"); got["text"] != "hello" {
		t.Fatalf("string: %v", got)
	}
	m := map[string]any{"x": 1}
	if got := NormalizeState(m); got["x"] != 1 {
		t.Fatalf("map: %v", got)
	}
	if got := NormalizeState(42); got["data"] != 42 {
		t.Fatalf("other: %v", got)
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
