package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// TestImageGenToolWiredThroughRegistry is the cmd-seam smoke test for M15
// #69: newToolRegistry registers generate_image, the credential lookup reuses
// the harness chain (models.yml → login → env), and the registered tool
// reports a missing credential actionably instead of failing at call time.
func TestImageGenToolWiredThroughRegistry(t *testing.T) {
	// Isolate the data dir so models.yml/credentials.json come from the test
	// and not from the developer's real ~/.xdev/agent.
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	for _, k := range []string{"OPENAI_API_KEY", "OPENAI_KEY", "GEMINI_API_KEY", "GEMINI_KEY"} {
		t.Setenv(k, "")
	}

	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	it, ok := reg.Get(tool.ImageGenToolName)
	if !ok {
		t.Fatal("generate_image not registered")
	}

	// 1. models.yml provider block wins, with ${VAR} expanded by LoadModels.
	if err := os.WriteFile(filepath.Join(dataDir, "models.yml"), []byte(`
providers:
  openai:
    baseUrl: https://api.openai.com/v1
    api: openai-completions
    apiKey: ${IMAGE_SEAM_KEY}
    models:
      - id: gpt-4o-mini
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IMAGE_SEAM_KEY", "sk-seam")
	lookup := imageGenCreds()
	key, source, err := lookup("openai")
	if err != nil || key != "sk-seam" {
		t.Fatalf("models.yml credential = %q (%s), err=%v", key, source, err)
	}
	if source != "models.yml" {
		t.Fatalf("credential source = %q, want models.yml", source)
	}

	// 2. No models.yml entry: the chain still resolves the environment rung.
	if err := os.Remove(filepath.Join(dataDir, "models.yml")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-env")
	key, source, err = lookup("openai")
	if err != nil || key != "sk-env" {
		t.Fatalf("env credential = %q (%s), err=%v", key, source, err)
	}
	if source != "env:OPENAI_API_KEY" {
		t.Fatalf("credential source = %q, want env:OPENAI_API_KEY", source)
	}

	// 3. Nothing configured: the tool's own error names the fix.
	t.Setenv("OPENAI_API_KEY", "")
	res, err := it.Execute(context.Background(), json.RawMessage(`{"subject":"a red cube"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "OPENAI_API_KEY") {
		t.Fatalf("missing credential not actionable through the registry: %+v", res)
	}
}
