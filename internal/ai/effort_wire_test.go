package ai

import "testing"

// The effort ladder's top two rungs do NOT reach an OpenAI-compatible
// provider.
//
// The ladder's reasoning budget is a NUMBER (config.EffortTokens): high
// 16,384, xhigh 32,768, max 49,152. The OpenAI-compatible wire carries a
// LEVEL string instead, and reasoningEffort folds every budget above 8,192
// onto "high" — because the OpenAI reasoning_effort vocabulary is
// low/medium/high and has no rung above high.
//
// So on the openai-completions and openai-responses wires, `effort: xhigh`
// and `effort: max` send byte-identical requests to `effort: high`. The
// user pays for a rung the provider never hears about.
//
// This is not a bug in the fold: the wire vocabulary genuinely stops at
// high. It IS worth pinning, because "I set xhigh and nothing changed" is
// exactly the kind of silent downgrade that reads as "the agent is
// overthinking and I asked it not to" — the knob appears to do something and
// does not.
//
// The Anthropic and Google wires carry the NUMBER (anthropic.go's
// thinking.budget_tokens, google_genai.go's thinkingBudget), so there the
// top two rungs really do buy a deeper reasoning budget.
//
// The budgets below are config.EffortTokens' values, spelled out because
// internal/config imports this package and a test import would be a cycle.
func TestOpenAIWireEffortLadderStopsAtHigh(t *testing.T) {
	for _, tc := range []struct {
		rung   string
		budget int
		want   string
	}{
		{"low", 2048, "low"},
		{"medium", 8192, "medium"},
		{"high", 16384, "high"},
		{"xhigh", 32768, "high"}, // the wire has no rung above high
		{"max", 49152, "high"},   // the wire has no rung above high
	} {
		if got := reasoningEffort(tc.budget); got != tc.want {
			t.Errorf("reasoningEffort(%s budget %d) = %q, want %q", tc.rung, tc.budget, got, tc.want)
		}
	}
}
