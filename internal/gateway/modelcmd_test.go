package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/config"
)

// /model's whole contract, at the seam where it is observable: what the chat
// registry remembers, what the daemon resolves for a turn, and what the phone
// is told. The catalog is injected, so the assertions do not depend on the
// machine's models.yml.
func TestModelCommandSwitchesPerChat(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1, 2})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.modelCommand(context.Background(), 1, "/model onegw/execution")
	got := strings.Join(b.sentTo(1), "\n")
	if !strings.Contains(got, "onegw/execution") {
		t.Fatalf("the switch was not confirmed: %q", got)
	}
	if ref := d.modelFor(1); ref != "onegw/execution" {
		t.Errorf("chat 1 resolves to %q, want onegw/execution", ref)
	}
	// The other chat must be untouched: one bot, several people.
	if ref := d.modelFor(2); ref != "" {
		t.Errorf("chat 2 moved with chat 1: %q", ref)
	}
}

// A bare id is the convenience /model shares with `xdev --model dev`: it must
// resolve against the catalog rather than be forwarded as an unknown ref.
func TestModelCommandExpandsABareID(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.modelCommand(context.Background(), 1, "/model execution")
	if ref := d.modelFor(1); ref != "onegw/execution" {
		t.Errorf("bare id resolved to %q, want onegw/execution", ref)
	}
	// An id two providers serve must NOT be guessed at.
	d.modelCommand(context.Background(), 1, "/model shared")
	if ref := d.modelFor(1); ref != "onegw/execution" {
		t.Errorf("an ambiguous id moved the chat to %q", ref)
	}
	if got := strings.Join(b.sentTo(1), "\n"); !strings.Contains(got, "matches") {
		t.Errorf("an ambiguous id was not reported: %q", got)
	}
}

// An unknown provider is refused at /model, not one turn later inside the
// worker — and the refusal must name the remedy.
func TestModelCommandRefusesAnUnknownProvider(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.modelCommand(context.Background(), 1, "/model nope/anything")
	if ref := d.modelFor(1); ref != "" {
		t.Errorf("an unknown provider was pinned anyway: %q", ref)
	}
	got := strings.Join(b.sentTo(1), "\n")
	if !strings.Contains(got, "unknown provider") {
		t.Errorf("the refusal does not name the problem: %q", got)
	}
}

// `/model default` clears the pin so the chat follows gateway.model again.
func TestModelCommandDefaultClearsThePin(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }
	d.model = "onegw/execution"

	d.modelCommand(context.Background(), 1, "/model onegw/free")
	d.modelCommand(context.Background(), 1, "/model default")
	if ref := d.modelFor(1); ref != "onegw/execution" {
		t.Errorf("after /model default the chat resolves to %q, want the daemon default", ref)
	}
	if !strings.Contains(strings.Join(b.sentTo(1), "\n"), "onegw/execution") {
		t.Error("the reply did not name the model now in force")
	}
}

// Bare /model reports the model in force and the catalog to choose from.
func TestModelCommandBareListsTheCatalog(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.modelCommand(context.Background(), 1, "/model")
	got := strings.Join(b.sentTo(1), "\n")
	for _, want := range []string{"onegw/execution", "onegw/free"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report omits %q: %q", want, got)
		}
	}
	// The fake bot records the RENDERED body, so "<ref>" arrives escaped.
	if !strings.Contains(got, "send /model &lt;ref&gt;") {
		t.Errorf("the report does not say how to switch: %q", got)
	}
}

// A ref with an effort suffix keeps the suffix: the worker splits it off, and
// dropping it here would silently ignore what the user typed.
func TestModelCommandKeepsTheEffortSuffix(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.models = func() (*config.Config, error) { return modelTestCatalog(), nil }

	d.modelCommand(context.Background(), 1, "/model onegw/execution:high")
	if ref := d.modelFor(1); ref != "onegw/execution:high" {
		t.Errorf("the effort suffix was lost: %q", ref)
	}
}

// The choice must survive a daemon restart — the bridge's whole durability
// model is that the FILES hold the state, not the process.
func TestChatsModelSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := NewChats(path)
	if err != nil {
		t.Fatal(err)
	}
	// No session yet: choosing a model before the first message is normal.
	if err := c.SetModel(7, "onegw/execution"); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewChats(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Model(7); got != "onegw/execution" {
		t.Errorf("after reopen, Model(7) = %q, want onegw/execution", got)
	}
}

// The daemon-wide gateway.model is the middle rung: below the chat's choice,
// above the run's own default.
func TestModelForPrefersTheChatOverTheDaemon(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.model = "onegw/execution"

	if got := d.modelFor(1); got != "onegw/execution" {
		t.Errorf("unpinned chat resolves to %q, want the daemon default", got)
	}
	if err := d.chats.SetModel(1, "onegw/free"); err != nil {
		t.Fatal(err)
	}
	if got := d.modelFor(1); got != "onegw/free" {
		t.Errorf("pinned chat resolves to %q, want onegw/free", got)
	}
}

// /status must name the model, because "which model answered me?" is the
// question /model exists for.
func TestStatusNamesTheModel(t *testing.T) {
	b := newFakeBot(t)
	d := newTestDaemon(t, b, []int64{1})
	d.model = "onegw/execution"
	if got := d.statusLine(1); !strings.Contains(got, "onegw/execution") {
		t.Errorf("/status omits the model: %q", got)
	}
}

// modelTestCatalog is the injected catalog: two providers, one of which
// serves an id the other does too (the ambiguity /model must refuse to
// guess at).
func modelTestCatalog() *config.Config {
	return &config.Config{
		Providers: map[string]*config.ProviderConfig{
			"onegw": {Models: []config.ModelConfig{
				{ID: "execution", Name: "Execution"},
				{ID: "free", Name: "Free"},
				{ID: "shared", Name: "Shared"},
			}},
			"local": {Models: []config.ModelConfig{
				{ID: "shared", Name: "Shared"},
			}},
		},
	}
}
