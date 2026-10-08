package config

import "strings"

// Reasoning effort: a ":effort" suffix on a model ref, the --thinking
// flag, and agent frontmatter `thinkingLevel` all resolve through this
// vocabulary. Model-role aliases (@smol, @task, …) are gone — every model
// ref is a literal provider/model or bare id.
//
// The ladder is Claude Code's (docs: model-config#adjust-effort-level):
// low → medium → high → xhigh → max, with "minimal" kept as xdev's off
// switch (a zero budget, which effortBudget drops to no reasoning at all).
// A rung a model does not advertise is CLAMPED to the highest rung it does
// (ClampEffort), which is CC's own rule — "if you set a level the active
// model does not support, Claude Code falls back to the highest supported
// level at or below the one you set".

// ThinkingLevels is the request-side vocabulary the `thinking` settings key
// and --thinking share: "auto" leaves the decision to the model's own
// ":effort" (or the provider default), "off" asks the provider for no
// reasoning, and the rest pin a rung of EffortLevels.
var ThinkingLevels = []string{"auto", "off", "minimal", "low", "medium", "high", "xhigh", "max"}

// EffortLevels are accepted, ordered low→high; "minimal" is the off switch.
// The top two rungs exist so a model that advertises them is not clamped
// down to high (see ClampEffort); below "high" a rung only narrows the tool
// surface and the reasoning budget, never the permissions.
var EffortLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// EffortTokens maps an effort name onto a reasoning token budget
// (ai.ThinkingBudget.Tokens is the transport-agnostic carrier; adapters
// translate it to their own effort vocabulary). Only "minimal" is a
// semantic zero (the off switch); every other rung is a real budget, which
// matters for the adapters that carry a number (Anthropic's
// thinking.budget_tokens) and is invisible to the ones that carry a level
// name (the OpenAI wires clamp every rung at or above high onto "high").
//
// xhigh/max stay under 64k because an adapter that raises max_tokens from
// the budget (anthropic.go) would otherwise ask for an impossible output
// cap: a model whose ceiling is lower advertises its own `efforts:` list in
// models.yml and is clamped to it by ClampEffort.
var EffortTokens = map[string]int{
	"minimal": 0,
	"low":     2048,
	"medium":  8192,
	"high":    16384,
	"xhigh":   32768,
	"max":     49152,
}

// IsEffort reports whether v names a reasoning effort.
func IsEffort(v string) bool {
	for _, e := range EffortLevels {
		if e == v {
			return true
		}
	}
	return false
}

// EffortRank is a rung's position on the ladder (minimal 0 … max 5). ok is
// false for "" and for a name the ladder does not know — the same "no
// opinion" answer IsEffort gives, kept distinct so a caller can tell a rung
// from a typo.
func EffortRank(level string) (rank int, ok bool) {
	for i, e := range EffortLevels {
		if e == strings.ToLower(strings.TrimSpace(level)) {
			return i, true
		}
	}
	return 0, false
}

// ClampEffort folds a requested rung onto the rungs a model advertises:
// the highest advertised rung at or below the request, else the lowest
// advertised one (a model that starts above the request cannot go lower
// than it can).
//
// An EMPTY advertised list means "no opinion" and returns the request
// unchanged — the same rule thinkingForModel applies to a model the catalog
// does not list, because a gateway routinely serves ids models.yml never
// pinned and reading "not listed" as "cannot reason" would silently drop a
// budget the user asked for. A list is how a models.yml entry states what
// its model accepts (`efforts: [low, medium, high]`).
func ClampEffort(level string, advertised []string) string {
	want := strings.ToLower(strings.TrimSpace(level))
	if len(advertised) == 0 {
		return level
	}
	wantRank, ok := EffortRank(want)
	if !ok {
		return level // auto/off/unknown: not a rung, nothing to clamp
	}
	best, bestRank := "", -1
	for _, a := range advertised {
		r, ok := EffortRank(a)
		if !ok || r > wantRank {
			continue
		}
		if r > bestRank {
			best, bestRank = strings.ToLower(strings.TrimSpace(a)), r
		}
	}
	if bestRank >= 0 {
		return best
	}
	// Nothing at or below the request: the model's own floor is the answer.
	lowest, lowestRank := "", 1<<30
	for _, a := range advertised {
		r, ok := EffortRank(a)
		if !ok || r >= lowestRank {
			continue
		}
		lowest, lowestRank = strings.ToLower(strings.TrimSpace(a)), r
	}
	if lowest == "" {
		return level // a list of names the ladder does not know clamps nothing
	}
	return lowest
}

// EffortBudget returns the reasoning token budget for an effort name
// ("" → no thinking requested).
func EffortBudget(effort string) (tokens int, ok bool) {
	if effort == "" {
		return 0, false
	}
	n, known := EffortTokens[effort]
	if !known {
		return 0, false
	}
	return n, true
}
