package typesafe

import "time"

// Verdict is what the Jev evaluation returns. It tells the
// agent run whether to continue normally, inject a wrap-up
// turn, or end the run.
type Verdict string

const (
	// VContinue means the session is still productive; keep going.
	VContinue Verdict = "continue"
	// VWrapUp means the session is nearing its budget; one
	// wrap-up turn should be injected before the hard cap.
	VWrapUp Verdict = "wrap_up"
	// VStopNow means the budget is exhausted; end the run.
	VStopNow Verdict = "stop_now"
)

// SessionBudget defines the advisory thresholds for a run.
// Max is the hard wall-clock cap. WarnPct is the fraction
// of Max at which the advisory fires (e.g. 0.9 means
// advisory at 90% of Max).
type SessionBudget struct {
	Max     time.Duration
	WarnPct float64
}
