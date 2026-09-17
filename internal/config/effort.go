package config

// Reasoning effort: a ":effort" suffix on a model ref, the --thinking
// flag, and agent frontmatter `thinkingLevel` all resolve through this
// vocabulary. Model-role aliases (@smol, @task, …) are gone — every model
// ref is a literal provider/model or bare id.

// ThinkingLevels is the request-side vocabulary the `thinking` settings key
// and --thinking share: "auto" leaves the decision to the model's own
// ":effort" (or the provider default), "off" asks the provider for no
// reasoning, and the rest pin a rung of EffortLevels. "xhigh"/"max" are the
// omp names for the top rung; applyThinkingFlag clamps them to "high".
var ThinkingLevels = []string{"auto", "off", "minimal", "low", "medium", "high", "xhigh", "max"}

// EffortLevels are accepted, ordered low→high; "minimal" is the off switch.
var EffortLevels = []string{"minimal", "low", "medium", "high"}

// EffortTokens maps an effort name onto a reasoning token budget
// (ai.ThinkingBudget.Tokens is the transport-agnostic carrier; adapters
// translate it to their own effort vocabulary).
var EffortTokens = map[string]int{
	"minimal": 0,
	"low":     2048,
	"medium":  8192,
	"high":    16384,
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
