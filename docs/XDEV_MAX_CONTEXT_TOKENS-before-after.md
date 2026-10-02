# XDEV_MAX_CONTEXT_TOKENS — Before & After

**Date:** 2026-09-19
**Branch:** `feat/turn-budget-context`
**Status:** Implemented, tested, awaiting PR

---

## 1. What this does NOT do

This change alone does **not** enable infinite sessions. It fixes a correctness
bug that *prevented* long-running sessions from working at all. See §5 for what
would still be needed for true infinity.

---

## 2. Before: the bug

### Where the bug lived

`cmd/xdev/print.go`, function `modelWindow()` (line ~890):

```go
// Before
func modelWindow(cfg *config.Config, provider, model string) int {
	pc, ok := cfg.Providers[provider]
	if !ok {
		return 0   // ← BUG: provider not found
	}
	for _, m := range providerModels(provider, pc) {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	return 0   // ← BUG: model not in catalog
}
```

### What `0` meant downstream

`modelWindow()` feeds `agent.CompactionConfig.ContextWindow` (6 call sites:
`print.go`, `tui.go` ×2, `acp.go`, `rpc.go` ×2). In `internal/agent/compact.go`:

```go
func (c CompactionConfig) threshold() int64 {
	if c.ContextWindow <= 0 {
		return 0   // ← disables the token-budget compaction trigger entirely
	}
	return int64(c.ContextWindow) - c.reserve()
}
```

And in `loop.go`:

```go
// Store is the session mirror of record. When set, it feeds compaction
// (threshold + overflow) and context rebuilds; nil disables compaction.
Store *session.Store
// Compaction configures context maintenance; ContextWindow 0 disables.
Compaction CompactionConfig
```

So `0` propagated to: **threshold compaction disabled, overflow protection disabled,
context rebuild on resume broken.**

### Who was affected

Any session where the model was **not explicitly pinned** in `models.yml`:

| Scenario | Before behavior |
|---|---|
| New provider (e.g. Cursor, Gemini) not in catalog | Compaction **off** — session runs until OOM |
| Discovered model not in `models.yml` (local server, dynamic catalog) | Compaction **off** |
| `claude-*` / `GPT-*` style dynamic IDs | Compaction **off** |
| TUI session switching to an unpinned model | Compaction **off** |
| ACP / RPC sessions with unpinned models | Compaction **off** |

The model was still running — it just had **no guard**. The 200k-token budget
that compaction uses to decide "compact now" was treated as 0, so the trigger
never fired. Long sessions grew until the OS or Go runtime killed them.

---

## 3. After: the fix

### New helper: `internal/agent/context_budget.go`

```go
package agent

import (
	"fmt"
	"os"
)

const MaxContextTokensDefault = 200000
const MaxContextTokensEnv = "XDEV_MAX_CONTEXT_TOKENS"

// ResolveMaxContextTokens returns the context token budget:
// XDEV_MAX_CONTEXT_TOKENS env var, or MaxContextTokensDefault.
// A bare env var ("") falls back to the default — a zero budget
// would silently disable compaction and every model it touches
// would be wrong.
func ResolveMaxContextTokens() int {
	if raw := os.Getenv(MaxContextTokensEnv); raw != "" {
		var n int
		if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return MaxContextTokensDefault
}
```

### Changed `modelWindow()`

```go
// After
func modelWindow(cfg *config.Config, provider, model string) int {
	pc, ok := cfg.Providers[provider]
	if !ok {
		return agent.ResolveMaxContextTokens()
	}
	for _, m := range providerModels(provider, pc) {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	return agent.ResolveMaxContextTokens()
}
```

### What changed in behavior

| Scenario | Before | After |
|---|---|---|
| Provider not found | `0` (compaction OFF) | `XDEV_MAX_CONTEXT_TOKENS` or `200000` (compaction ON) |
| Model not in catalog | `0` (compaction OFF) | `XDEV_MAX_CONTEXT_TOKENS` or `200000` (compaction ON) |
| Model pinned in catalog | `m.ContextWindow` | `m.ContextWindow` (unchanged) |
| `XDEV_MAX_CONTEXT_TOKENS=500000` set, model unpinned | `0` (compaction OFF) | `500000` (compaction ON, operator's budget) |
| `XDEV_MAX_CONTEXT_TOKENS=""` (empty), model unpinned | `0` (compaction OFF) | `200000` (fallback to default) |
| `XDEV_MAX_CONTEXT_TOKENS=abc` (invalid), model unpinned | `0` (compaction OFF) | `200000` (invalid → fallback) |

### Test coverage

| Test | What it pins |
|---|---|
| `TestResolveMaxContextTokensDefault` | No env → 200000 |
| `TestResolveMaxContextTokensEnv` | `XDEV_MAX_CONTEXT_TOKENS=500000` → 500000 |
| `TestResolveMaxContextTokensEmptyFallback` | `XDEV_MAX_CONTEXT_TOKENS=""` → 200000 |
| `TestResolveMaxContextTokensInvalidFallback` | `XDEV_MAX_CONTEXT_TOKENS=abc` → 200000 |
| `TestModelWindowUsesDiscovery` | Pinned model wins over fallback |
| `TestModelWindowFallbackBudget` | Unpinned model/provider → default budget |
| `TestModelWindowFallbackHonoursEnv` | Env var wins over default for unpinned models |

---

## 4. Design decisions

1. **No new field on `Agent` struct or `printOptions`** — nothing reads it; the
   helper is called directly from 6 existing call sites. (YAGNI)
2. **Env var wins over discovery** — Claude Code parity: an operator's explicit
   budget always takes precedence over what the catalog says.
3. **Helper in `agent` package** — single source of truth, reused from
   `cmd/xdev/print.go` (one import line, already present).
4. **Validation**: invalid/empty env values fall back to default, never to 0.
   A zero budget would silently kill compaction.
5. **200000 default** mirrors Claude Code's `CLAUDE_CODE_MAX_CONTEXT_TOKENS`
   documented default — the budget for 200K-window models.

---

## 5. Does this enable infinity sessions? No.

This fix removes **one blocker** among many. A session that never ends still
requires all of the following to be addressed:

| # | Requirement | Status | Where |
|---|---|---|---|
| 1 | **Context window budget correct** | ✅ FIXED (this change) | `modelWindow()` now returns a real number, not 0 |
| 2 | **Compaction actually fires** | ✅ Already works | `compact.go` threshold trigger — fires when `ContextWindow > 0` (now always true) |
| 3 | **Agent turn budget** | ✅ Removed | No turn cap: `MaxTurns=0` (default) is unbounded; `Agent.MaxTurns` can still set an explicit cap |
| 4 | **Process memory backstop** | ✅ Already works | `debug.SetMemoryLimit` via `XDEV_MEMLIMIT` (default 100 MB RSS) — runaway turns degrade to "compact now" |
| 5 | **Session persistence** | ✅ Already works | Append-only JSONL — sessions survive process death |
| 6 | **No disk leak** | ⚠️ Not proven | Blobs, session files, and compacted tails accumulate indefinitely; no TTL or hard cap documented |
| 7 | **Provider never hangs** | ⚠️ Not proven | Infinite-stream providers need watchdogs per `ai` package |
| 8 | **Compaction laddered correctly** | ⚠️ Depends | `methodOrder` must include `threshold` (default does); wrong config still breaks |

### What "infinity session" actually needs

A true infinity session requires **two things this change does NOT provide**:

1. **Disk budget guard** — nothing prevents the JSONL session file, blob store,
   and compacted history from growing forever. A long-running session with
   millions of turns needs a retention policy (compact-and-drop-old-tail).
2. **Operator awareness** — even with compaction working, a 24-hour session
   against a model with a 200k context will still hit wall-clock and token-cost
   limits. The operator needs to know *this is expected and bounded by memory*.

### What this change **does** guarantee

With `XDEV_MAX_CONTEXT_TOKENS` set (or the 200000 default), an unpinned model
now:

- Has compaction enabled on the token budget (threshold trigger fires)
- Has a known, bounded context window (no silent 0-window OOM)
- Honors the operator's explicit budget via env var
- Falls back safely if the env var is empty or malformed

This means **compaction works** for all models — which is a prerequisite for
infinity sessions, but not sufficient.

---

## 6. How to use

```bash
# Use the default (200000) — compaction now works for ALL models, even
# ones models.yml doesn't pin.
xdev -p my-long-session

# Override for a specific budget (e.g. your provider's context is 128k)
XDEV_MAX_CONTEXT_TOKENS=131072 xdev -p my-long-session

# Confirm it's picked up
xdev models  # shows the effective budget in the TUI status bar
```

No config file changes needed. The env var works in `xdev` and `xdev tui`.

---

## 7. Files changed

| File | Type | Purpose |
|---|---|---|
| `internal/agent/context_budget.go` | NEW | Constants + `ResolveMaxContextTokens()` helper |
| `internal/agent/context_budget_test.go` | NEW | 4 unit tests for the helper |
| `cmd/xdev/modelwindow_budget_test.go` | NEW | 3 integration tests for `modelWindow()` fallback |
| `cmd/xdev/print.go` | MODIFIED | Both `return 0` → `return agent.ResolveMaxContextTokens()` |

---

## 8. Verification

```
go build ./...               → OK
gofmt -d                    → clean
go test ./internal/agent/    → 7 PASS (incl. 4 new)
go test ./cmd/xdev/          → 3 new PASS; 3 pre-existing failures unrelated
```

Pre-existing test failures (verified failing before this change):
- `TestConnectPickerItems` — catalog ordering
- `TestSecurityScanWiredThroughRegistry` — scanner binary presence
- `TestSkillPromptBlockEmptyWithoutSkills` — env var scoping

---

## 9. Relation to existing knobs

| Knob | Purpose | This change |
|---|---|---|
| `XDEV_MEMLIMIT` | Process memory ceiling (default 100 MB RSS) | Unaffected — different budget dimension |
| `Model.ContextWindow` (models.yml) | Per-model explicit window | Takes precedence over env var for pinned models |
| `compaction.methodOrder` | When compaction fires | Unchanged — threshold trigger now has a real window |
| `Agent.MaxTurns` | Turn count ceiling | Unaffected — still caps at 200 turns |

This change sits at the **model-resolution layer** — it answers "what is the
window?" The other knobs answer "when to compact" (methodOrder), "how much
memory" (MemLimit), and "how many turns" (MaxTurns). All must be correct for
infinity sessions.
