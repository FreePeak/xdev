package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiscoverModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %s, want /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-d" {
			t.Errorf("auth header = %q", got)
		}
		fmt.Fprint(w, `{"data":[{"id":"llama3:8b","context_length":8192},{"id":"qwen","max_tokens":4096,"supports_thinking":true},{"id":"","bogus":1}]}`)
	}))
	t.Cleanup(srv.Close)
	pc := &ProviderConfig{
		BaseURL: srv.URL, APIKey: "sk-d",
		Discovery: &DiscoveryConfig{Type: DiscoveryOpenAIModels},
		Models:    []ModelConfig{{ID: "pinned", ContextWindow: 1000}},
	}
	got, err := DiscoverModels(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	// Pinned first, then discovered, and the empty-id entry dropped.
	if len(got) != 3 {
		t.Fatalf("models = %+v", got)
	}
	if got[0].ID != "pinned" || got[1].ID != "llama3:8b" || got[2].ID != "qwen" {
		t.Fatalf("order = %v", got)
	}
	if got[1].ContextWindow != 8192 {
		t.Fatalf("context_length lost: %+v", got[1])
	}
	if !got[2].Reasoning || got[2].ContextWindow != 4096 {
		t.Fatalf("max_tokens/supports_thinking lost: %+v", got[2])
	}
}

func TestDiscoverModelsMergeKeepsPinnedWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"shared","context_length":99}]}`)
	}))
	t.Cleanup(srv.Close)
	pc := &ProviderConfig{
		BaseURL: srv.URL, Discovery: &DiscoveryConfig{Type: DiscoveryOpenAIModels},
		Models: []ModelConfig{{ID: "shared", ContextWindow: 200000, Reasoning: true}},
	}
	got, err := DiscoverModels(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ContextWindow != 200000 || !got[0].Reasoning {
		t.Fatalf("a server default clobbered the user's pin: %+v", got)
	}
}

func TestDiscoverModelsAlternateShapes(t *testing.T) {
	for name, body := range map[string]string{
		"wrapped": `{"models":[{"id":"m1"}]}`,
		"bare":    `[{"id":"m2"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			pc := &ProviderConfig{BaseURL: srv.URL, Discovery: &DiscoveryConfig{Type: DiscoveryOpenAIModels}}
			got, err := DiscoverModels(context.Background(), pc)
			if err != nil || len(got) != 1 {
				t.Fatalf("%s: got %+v err=%v", name, got, err)
			}
		})
	}
}

func TestDiscoverModelsRequiresDiscoveryConfig(t *testing.T) {
	// Absent block: nothing to do, not an error.
	if got, err := DiscoverModels(context.Background(), &ProviderConfig{BaseURL: "http://x"}); err != nil || got != nil {
		t.Fatalf("no discovery: %+v %v", got, err)
	}
	// Unknown scheme: inert rather than guessed.
	pc := &ProviderConfig{BaseURL: "http://x", Discovery: &DiscoveryConfig{Type: "mystery"}}
	if got, err := DiscoverModels(context.Background(), pc); err != nil || got != nil {
		t.Fatalf("unknown type: %+v %v", got, err)
	}
}

func TestDiscoverModelsDownServerIsAnErrorNotNil(t *testing.T) {
	// A dead local server must return an error the caller can log-and-skip;
	// startup never depends on it.
	pc := &ProviderConfig{BaseURL: "http://127.0.0.1:1", Discovery: &DiscoveryConfig{Type: DiscoveryOpenAIModels}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := DiscoverModels(context.Background(), pc); err == nil {
			t.Error("unreachable server must error")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("discovery did not respect its timeout")
	}
}

func TestDiscoverModelsHTTPStatusPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, strings.Repeat("x", 5000))
	}))
	t.Cleanup(srv.Close)
	pc := &ProviderConfig{BaseURL: srv.URL, Discovery: &DiscoveryConfig{Type: DiscoveryOpenAIModels}}
	_, err := DiscoverModels(context.Background(), pc)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("status lost: %v", err)
	}
	if len(err.Error()) > 600 {
		t.Fatalf("error body not bounded: %d chars", len(err.Error()))
	}
}

func TestDiscoverModelsInjectV1Path(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	t.Cleanup(srv.Close)
	pc := &ProviderConfig{BaseURL: srv.URL, Discovery: &DiscoveryConfig{Type: DiscoveryOpenAIModels, InjectV1: true}}
	if _, err := DiscoverModels(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", path)
	}
}

// TestDiscoverModelsProviderWindowFillsWindowless pins the rung below a
// stated context_length: a gateway answering bare {id, object, owned_by}
// (onegw does that for all 108 of its ids, measured 2026-10-04) leaves every
// discovered model at ContextWindow 0, and compaction then falls through to
// the compiled 200000 default. The provider's own contextWindow is the one
// honest number an operator has for that case, so discovery hands it to
// every windowless entry instead of reporting "unknown".
func TestDiscoverModelsProviderWindowFillsWindowless(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"bare"},{"id":"stated","context_length":64000},{"id":"out-token","max_tokens":8192}]}`)
	}))
	t.Cleanup(srv.Close)
	pc := &ProviderConfig{
		BaseURL: srv.URL, Discovery: &DiscoveryConfig{Type: DiscoveryOpenAIModels},
		ContextWindow: 1_000_000,
		Models:        []ModelConfig{{ID: "pinned", ContextWindow: 4096}},
	}
	got, err := DiscoverModels(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		switch m.ID {
		case "pinned":
			if m.ContextWindow != 4096 {
				t.Fatalf("a per-model pin lost: %+v", m)
			}
		case "stated":
			if m.ContextWindow != 64000 {
				t.Fatalf("a stated context_length lost: %+v", m)
			}
		case "bare":
			if m.ContextWindow != 1_000_000 {
				t.Fatalf("windowless entry did not take the provider window: %+v", m)
			}
		case "out-token":
			// max_tokens stays the smaller rung it always was; the
			// provider's window is a ceiling, not a floor that shrinks
			// below a stated per-model bound.
			if m.ContextWindow != 1_000_000 || m.MaxTokens != 8192 {
				t.Fatalf("max_tokens window lost: %+v", m)
			}
		}
	}
}

// TestDiscoverModelsNoProviderWindowStaysUnknown: with no provider window
// configured the entry keeps reporting unknown (0), which is what lets
// modelWindow's ladder decide. Discovery must not invent a default.
func TestDiscoverModelsNoProviderWindowStaysUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"bare"}]}`)
	}))
	t.Cleanup(srv.Close)
	pc := &ProviderConfig{BaseURL: srv.URL, Discovery: &DiscoveryConfig{Type: DiscoveryOpenAIModels}}
	got, err := DiscoverModels(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ContextWindow != 0 {
		t.Fatalf("discovery invented a window: %+v", got)
	}
}
