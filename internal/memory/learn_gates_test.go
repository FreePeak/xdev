package memory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// #88: the learn tool promised secret scrubbing in the consolidation PROMPT
// only, and `autolearn.enabled` did not exist. Both are now enforced here.

func learnArgs(memory, context string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"memory": memory, "context": context})
	return b
}

func TestLearnToolRedactsAtStoreTime(t *testing.T) {
	b := &Backend{Dir: filepath.Join(t.TempDir(), "memory")}
	if err := b.Ensure(); err != nil {
		t.Fatal(err)
	}
	secret := "ghp_live-token-value"
	tool := &LearnTool{
		Backend: b,
		Redactor: func(s string) string {
			return strings.ReplaceAll(s, secret, "$$REDACTED$$")
		},
	}
	if _, err := tool.Execute(context.Background(), learnArgs(
		"prefer table tests; token "+secret, "use "+secret)); err != nil {
		t.Fatal(err)
	}
	raw := readFileOrEmpty(filepath.Join(b.Dir, "learned.md"))
	if strings.Contains(raw, secret) {
		t.Fatalf("the live credential was stored verbatim:\n%s", raw)
	}
	if !strings.Contains(raw, "$$REDACTED$$") {
		t.Fatalf("no redaction applied:\n%s", raw)
	}
}

func TestLearnToolDisabledRefuses(t *testing.T) {
	b := &Backend{Dir: filepath.Join(t.TempDir(), "memory")}
	if err := b.Ensure(); err != nil {
		t.Fatal(err)
	}
	tool := &LearnTool{Backend: b, Disabled: true}
	res, _ := tool.Execute(context.Background(), learnArgs("a lesson", "ctx"))
	if !res.IsError || !strings.Contains(res.Text, "autolearn.enabled") {
		t.Fatalf("a disabled recorder must refuse with the setting named: %+v", res)
	}
	if raw := readFileOrEmpty(filepath.Join(b.Dir, "learned.md")); strings.Contains(raw, "a lesson") {
		t.Fatalf("disabled recorder still wrote: %s", raw)
	}
}
