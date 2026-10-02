package tool

// Tool capability manifests (#420 / ZCode §4.1): one declaration read by
// plan mode, approvals, parallel scheduling, and (later) stream-time
// execution. Call sites must not guess by tool name.

// SideEffectScope is where a tool's effects land (ZCode ModelToolSideEffectScope).
type SideEffectScope string

const (
	ScopeNone            SideEffectScope = "none"
	ScopeWorkspace       SideEffectScope = "workspace"
	ScopeGit             SideEffectScope = "git"
	ScopeNetwork         SideEffectScope = "network"
	ScopeSystem          SideEffectScope = "system"
	ScopeSession         SideEffectScope = "session"
	ScopeUserInteraction SideEffectScope = "userInteraction"
)

// Caps is the capability declaration for one tool. Zero value is undeclared:
// plan mode fails closed; ConcurrentOK is false; Classify errors (Decide then
// falls back to TierExec for unmodeled names).
type Caps struct {
	ReadOnly                bool
	Destructive             bool
	ConcurrentSafe          bool
	SideEffect              SideEffectScope
	AlwaysAsk               bool // survives yolo allow grants; never lifts a deny
	RequiresUserInteraction bool
	StopTurnOnSuccess       bool
	// Tier is the approval tier. Always set on declared tools.
	Tier Tier
}

// Capser is the optional interface a Tool implements to declare Caps without
// going through the name table. Preferred for new tools.
type Capser interface {
	Caps() Caps
}

// builtinCaps is the side table for tools that predate Capser. It encodes
// today's plan-mode allowlist and the core approval tiers; ConcurrentSafe
// matches the read-only research set (stream-time execution / scheduler).
// Keep in sync with caps_test.go's table-diff pins.
var builtinCaps = map[string]Caps{
	// Core four (historical Classify).
	"read":  {ReadOnly: true, ConcurrentSafe: true, SideEffect: ScopeNone, Tier: TierReadOnly},
	"write": {Destructive: true, SideEffect: ScopeWorkspace, Tier: TierWrite},
	"edit":  {Destructive: true, SideEffect: ScopeWorkspace, Tier: TierWrite},
	"bash":  {Destructive: true, SideEffect: ScopeSystem, Tier: TierExec},

	// Former planReadOnlyTools (and concurrent-safe research tools).
	"grep":       {ReadOnly: true, ConcurrentSafe: true, SideEffect: ScopeNone, Tier: TierReadOnly},
	"glob":       {ReadOnly: true, ConcurrentSafe: true, SideEffect: ScopeNone, Tier: TierReadOnly},
	"ast_grep":   {ReadOnly: true, ConcurrentSafe: true, SideEffect: ScopeNone, Tier: TierReadOnly},
	"web_search": {ReadOnly: true, ConcurrentSafe: true, SideEffect: ScopeNetwork, Tier: TierReadOnly},
	"lsp":        {ReadOnly: true, ConcurrentSafe: true, SideEffect: ScopeNone, Tier: TierReadOnly},

	// Known mutators (friendlier plan-mode denial via Destructive).
	"ast_edit": {Destructive: true, SideEffect: ScopeWorkspace, Tier: TierWrite},
}

// CapsOf returns the declaration for t. Capser wins; else builtinCaps by name.
// ok is false when nothing declared — plan mode and concurrent gates fail closed.
func CapsOf(t Tool) (Caps, bool) {
	if t == nil {
		return Caps{}, false
	}
	if c, ok := t.(Capser); ok {
		return c.Caps(), true
	}
	c, ok := builtinCaps[t.Name()]
	return c, ok
}

// CapsByName looks up the builtin table only (no Capser). Used by Classify and
// Decide when the registry handle is not in hand.
func CapsByName(name string) (Caps, bool) {
	c, ok := builtinCaps[name]
	return c, ok
}

// AllowedInPlan reports whether a declared tool may run under plan mode
// (ZCode checkPlanMode): ReadOnly && !Destructive, or session-scoped
// non-destructive side effects. AlwaysAsk and Destructive deny. Undeclared → false.
func AllowedInPlan(c Caps, declared bool) bool {
	if !declared {
		return false
	}
	if c.Destructive || c.AlwaysAsk {
		return false
	}
	if c.ReadOnly {
		return true
	}
	return c.SideEffect == ScopeSession
}

// ConcurrentOK reports whether the tool may run in parallel with others
// (scheduler / stream-time RO execution). Undeclared → false.
func ConcurrentOK(t Tool) bool {
	c, ok := CapsOf(t)
	return ok && c.ConcurrentSafe && !c.Destructive
}
