package tool

import "encoding/json"

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

// OpReadOnly is the optional interface a Tool implements when its OPS split
// into reads and writes.
//
// A whole-tool cap cannot describe such a tool honestly: the github tool
// searches (search_prs, repo_view) and creates (pr_create, pr_push) under one
// name, so a cap that says "read" is a lie for one op and a cap that says
// "write" is a lie for the other. The verdict is per op.
//
// Live 2026-10-21 (session 4c4addab): the model called the github tool's
// search_prs op while answering "list me all pr urls openningin xdev". The
// keep-going gate had no caps entry for the name, so the call counted as a
// mutation, the gate armed, and the harness pushed the model past the answer
// it had just delivered.
type OpReadOnly interface {
	// ReadOnlyOps lists the ops that only READ.
	ReadOnlyOps() []string
}

// CallIsReadOnly reports whether one tool call only READ.
//
// It is the one definition of "this call changed nothing", so the keep-going
// gate and the reasoning about it cannot drift apart.
//
//   - bash is judged by its command line (ReadOnlyCommand).
//   - a tool that declares its read-only ops is judged by its op.
//   - everything else is judged by the caps manifest, and an UNDECLARED tool
//     counts as a mutation: the conservative default Decide already uses, so
//     an MCP tool this build has never heard of keeps its keep-going.
//
// reg may be nil, which leaves the op path unavailable — the caps manifest
// then decides, and an undeclared name still fails closed.
func CallIsReadOnly(reg *Registry, name string, args json.RawMessage) bool {
	if name == "bash" {
		return BashCallIsReadOnly(args)
	}
	if reg != nil {
		if t, ok := reg.Get(name); ok {
			if o, ok := t.(OpReadOnly); ok {
				return opIsReadOnly(o.ReadOnlyOps(), args)
			}
		}
	}
	caps, declared := CapsByName(name)
	if !declared {
		return false
	}
	return caps.ReadOnly
}

// opIsReadOnly reports whether the op named in the call's arguments is one of
// the tool's read-only ops. An unparseable call, or a call with no op, is
// not a read: the tool's write ops are the dangerous direction.
func opIsReadOnly(ops []string, args json.RawMessage) bool {
	var a struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return false
	}
	if a.Op == "" {
		return false
	}
	for _, o := range ops {
		if o == a.Op {
			return true
		}
	}
	return false
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

	// Session scratch — does not change the workspace. Without this entry
	// callMutated treated every todo call as a mutation (CapsByName miss →
	// fail closed), so a research run that opened a todo list kept its
	// keep-going nudge after the report (session 724e3fbb).
	"todo": {ReadOnly: true, ConcurrentSafe: true, SideEffect: ScopeSession, Tier: TierReadOnly},

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
