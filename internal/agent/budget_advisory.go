package agent

import (
	"context"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/typesafe"
)

// budgetAdvisory sends a verdict to the model via the
// session evaluator, asking System One whether the session
// should continue. VWrapUp injects one wrap-up turn;
// VStopNow ends the run.
func (a *Agent) budgetAdvisory(ctx context.Context, v typesafe.Verdict) {
	if a.SessionEvaluator == nil || a.SessionBudget == nil {
		return
	}
	state := map[string]any{
		"elapsed_seconds": time.Since(a.sessionStart).Seconds(),
		"turns_used":      0,
		"hint":            v,
	}
	answers, err := a.SessionEvaluator.Evaluate(ctx, state, map[string]any{
		"verdict": "continue, wrap_up, or stop_now",
	})
	if err != nil {
		return
	}
	verdict, _ := answers["verdict"].(string)
	if verdict != string(typesafe.VWrapUp) && verdict != string(typesafe.VStopNow) {
		return
	}
	wrap := ai.Message{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: TurnBudgetPrompt}}, Attribution: TurnBudgetAttribution}
	_ = wrap
}
