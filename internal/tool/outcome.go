package tool

// Outcome is the renderer-facing slice of a tool result's structured Details.
// The TUI must not type-switch over another package's payloads, so the tools
// name the facts a status frame can show — how the process ended, whether the
// output window dropped anything, and what changed on disk — and everything
// else stays invisible to the chrome.
type Outcome struct {
	// Exit is the process exit code; meaningful only when HasExit is set.
	Exit int
	// HasExit marks a result that ran a child process to an exit status. A
	// command killed by a signal reports none: that number is a signal, not
	// an exit code, and calling it "Exit: 15" would be a lie. A backgrounded
	// job that has not finished yet reports -1, which is none either.
	HasExit bool
	// Truncated marks output the tool's sink window dropped: the model saw
	// less than the command produced, and the user should know it.
	Truncated bool
	// Diff is the unified diff of the file change the tool made, when it made
	// one. The result's Text stays what the model was shown; this exists so
	// the frame can say what moved on disk in the theme's diff colours.
	Diff string
	// DurationMS is wall time nested in Details (bash durationMs) when the
	// tool recorded one. Zero when unknown. Prefer Message.DurationMS when
	// the agent loop stamped the call; this is the fallback for older
	// sessions and in-memory *bashDetails before a JSON round-trip.
	DurationMS int64
}

// OutcomeOf flattens one Result.Details. Tools that record none of these
// facts (read, grep, every MCP tool) yield the zero Outcome, which renders as
// no status footer rather than a footer of guesses.
func OutcomeOf(details any) Outcome {
	switch d := details.(type) {
	case nil:
		return Outcome{}
	case *bashDetails:
		if d == nil {
			return Outcome{}
		}
		return Outcome{
			Exit:       d.ExitCode,
			HasExit:    !d.Killed && d.ExitCode >= 0,
			Truncated:  d.Truncated,
			DurationMS: d.DurationMs,
		}
	case map[string]any:
		var o Outcome
		if code, ok := d["exitCode"].(int); ok {
			o.Exit, o.HasExit = code, true
		} else if code, ok := d["exitCode"].(float64); ok {
			o.Exit, o.HasExit = int(code), true
		}
		if cut, ok := d["truncated"].(bool); ok {
			o.Truncated = cut
		}
		if diff, ok := d["unifiedDiff"].(string); ok {
			o.Diff = diff
		}
		o.DurationMS = mapDurationMS(d)
		return o
	}
	return Outcome{}
}

func mapDurationMS(d map[string]any) int64 {
	switch v := d["durationMs"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}
