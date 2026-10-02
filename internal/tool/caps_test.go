package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// Table-diff pin: declarations encode the pre-#420 name tables verbatim.
func TestBuiltinCapsMatchHistoricalTables(t *testing.T) {
	// Historical Classify core four.
	for name, want := range map[string]Tier{
		"read": TierReadOnly, "write": TierWrite, "edit": TierWrite, "bash": TierExec,
	} {
		c, ok := CapsByName(name)
		if !ok {
			t.Fatalf("%s missing from builtinCaps", name)
		}
		if c.Tier != want {
			t.Errorf("%s tier = %v, want %v", name, c.Tier, want)
		}
	}
	// Former planReadOnlyTools.
	for _, name := range []string{"read", "grep", "glob", "ast_grep", "web_search", "lsp"} {
		c, ok := CapsByName(name)
		if !ok || !AllowedInPlan(c, true) {
			t.Errorf("%s must be allowed in plan mode", name)
		}
		if !c.ConcurrentSafe {
			t.Errorf("%s must be ConcurrentSafe (stream-time / scheduler)", name)
		}
	}
	// Mutators denied in plan mode.
	for _, name := range []string{"write", "edit", "bash", "ast_edit"} {
		c, ok := CapsByName(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if AllowedInPlan(c, true) {
			t.Errorf("%s must be denied in plan mode", name)
		}
	}
}

func TestUndeclaredFailsClosed(t *testing.T) {
	if AllowedInPlan(Caps{}, false) {
		t.Fatal("undeclared must not pass plan mode")
	}
	if ConcurrentOK(nil) {
		t.Fatal("nil tool ConcurrentOK")
	}
	// Synthetic tool with no Capser and no builtin entry.
	u := undeclaredStub{name: "synth_undeclared"}
	if _, ok := CapsOf(u); ok {
		t.Fatal("undeclared stub must not resolve Caps")
	}
	if ConcurrentOK(u) {
		t.Fatal("undeclared must not schedule in parallel")
	}
	if AllowedInPlan(Caps{}, false) {
		t.Fatal("undeclared plan")
	}
}

func TestCapserOverridesBuiltin(t *testing.T) {
	ttool := capserStub{
		name: "read",
		c:    Caps{ReadOnly: false, Destructive: true, SideEffect: ScopeWorkspace, Tier: TierWrite},
	}
	c, ok := CapsOf(ttool)
	if !ok || !c.Destructive || c.ReadOnly {
		t.Fatalf("Capser must win over builtinCaps: %+v ok=%v", c, ok)
	}
	if AllowedInPlan(c, true) {
		t.Fatal("destructive Capser must fail plan mode")
	}
}

func TestSessionScopedAllowedInPlan(t *testing.T) {
	c := Caps{SideEffect: ScopeSession, Tier: TierReadOnly}
	if !AllowedInPlan(c, true) {
		t.Fatal("session-scoped non-destructive must pass plan mode")
	}
	c.AlwaysAsk = true
	if AllowedInPlan(c, true) {
		t.Fatal("AlwaysAsk must deny plan mode")
	}
}

func TestClassifyStillCoreFourOnly(t *testing.T) {
	// Behaviour pin: grep is declared for plan mode but Classify still errors
	// so Decide treats it as TierExec (historical).
	if _, err := Classify("grep", nil); err == nil {
		t.Fatal("Classify(grep) must still error for Decide's TierExec default")
	}
	got, err := Classify("read", json.RawMessage(`{}`))
	if err != nil || got != TierReadOnly {
		t.Fatalf("Classify(read) = %v %v", got, err)
	}
}

type undeclaredStub struct{ name string }

func (u undeclaredStub) Name() string                 { return u.name }
func (u undeclaredStub) Description() string          { return "stub" }
func (u undeclaredStub) Parameters() json.RawMessage  { return json.RawMessage(`{}`) }
func (u undeclaredStub) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{}, nil
}

type capserStub struct {
	name string
	c    Caps
}

func (s capserStub) Name() string                { return s.name }
func (s capserStub) Description() string         { return "stub" }
func (s capserStub) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (s capserStub) Execute(context.Context, json.RawMessage) (Result, error) {
	return Result{}, nil
}
func (s capserStub) Caps() Caps { return s.c }
