package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/ext"
)

// TestActionRouterAsideAndSteer pins the routing split: steer interrupts the
// live run, while an aside rides the followUp channel with its marker (the
// loop has no aside surface, and an aside must not cut the running turn the
// way steer does).
func TestActionRouterAsideAndSteer(t *testing.T) {
	var steered, followed []string
	r := actionRouter(
		func(s string) { steered = append(steered, s) },
		func(s string) { followed = append(followed, s) },
		nil,
	)
	r(ext.Action{Action: "steer", Text: "stop"})
	r(ext.Action{Action: "aside", Text: "fyi"})
	r(ext.Action{Action: "followUp", Text: "next"})

	if len(steered) != 1 || steered[0] != "stop" {
		t.Fatalf("steer routing = %q", steered)
	}
	if len(followed) != 2 || followed[0] != asidePrefix+"fyi" || followed[1] != "next" {
		t.Fatalf("followUp routing = %q", followed)
	}
}

// TestRegisterProviderActionResolvesModel proves the ext runtime action is
// wired end-to-end: a register_provider payload in models.yml shape lands in
// cfg.Providers, and a model on that provider then builds an adapter whose
// request actually reaches the endpoint.
func TestRegisterProviderActionResolvesModel(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	router := actionRouter(func(string) {}, func(string) {}, cfg)
	payload, err := json.Marshal(map[string]any{
		"name":    "extprov",
		"baseUrl": srv.URL,
		"api":     "openai-completions",
		"apiKey":  "sk-ext",
		"models":  []map[string]any{{"id": "extmodel"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	router(ext.Action{Action: "register_provider", Data: payload})

	pc, ok := cfg.Providers["extprov"]
	if !ok {
		t.Fatalf("provider not registered: %v", cfg.Providers)
	}
	p, err := buildProvider("extprov", pc, "extmodel", cfg)
	if err != nil {
		t.Fatalf("registered provider must build: %v", err)
	}
	stream(t, p)
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/chat/completions" {
		t.Fatalf("request path = %q, want /chat/completions", gotPath)
	}

	// A broken payload must be refused, logged, and must not break the
	// router for the next action.
	router(ext.Action{
		Action: "register_provider", Text: "broken",
		Data: json.RawMessage(`{"baseUrl":"http://x/v1","api":"telepathy","models":[{"id":"m"}]}`),
	})
	if _, ok := cfg.Providers["broken"]; ok {
		t.Fatal("an unsupported api must not register")
	}
}
