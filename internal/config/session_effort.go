package config

import "strings"

// Session effort is the one dial that scales how hard a session works:
// tools advertised eagerly, optional prompt appendix, and the thinking
// default when `thinking` is still "auto". It is NOT the permission
// posture (/mode) and NOT the request-side reasoning budget (:effort /
// /thinking) — those stay their own knobs. A plain string key on
// Settings, like Thinking: "" means unset in this layer, default standard.
//
// deliberately NOT in repoSafeSettingsKeys: a cloned repository must not
// force how much of the tool surface and reasoning budget a user spends.

const (
	// SessionEffortLean is pi-core tools only — daily simple asks.
	SessionEffortLean = "lean"
	// SessionEffortStandard is today's xdev default (partial deferred catalog).
	SessionEffortStandard = "standard"
	// SessionEffortFull opens the omp-shaped surface for complex work.
	SessionEffortFull = "full"
)

// SessionEffortLevels is the closed vocabulary /effort and the settings key share.
var SessionEffortLevels = []string{SessionEffortLean, SessionEffortStandard, SessionEffortFull}

// SessionEffortCycle is the Shift-Tab order: lean → standard → full → lean.
// All three are safe to cycle; none widens permissions (that is /mode).
var SessionEffortCycle = []string{SessionEffortLean, SessionEffortStandard, SessionEffortFull}

// IsSessionEffort reports whether v is one of the three writeable rungs
// (not an alias). /effort and config set use this so "simple" is rejected
// with a usage error rather than silently stored under a different spelling.
func IsSessionEffort(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case SessionEffortLean, SessionEffortStandard, SessionEffortFull:
		return true
	}
	return false
}

// NormalizeSessionEffort folds aliases and empty onto a canonical rung.
// Unknown values fall back to standard (never lean — a typo must not
// silently strip tools; never full — a typo must not silently widen).
func NormalizeSessionEffort(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case SessionEffortLean, "simple", "min", "minimal":
		return SessionEffortLean
	case SessionEffortFull, "max", "omp", "complex":
		return SessionEffortFull
	case SessionEffortStandard, "default", "normal", "auto", "":
		return SessionEffortStandard
	default:
		return SessionEffortStandard
	}
}

// SessionEffortThinking is the request-side reasoning level this effort
// implies when the user's `thinking` key is still "auto". standard leaves
// the model / :effort alone; lean pins low; full pins high.
func SessionEffortThinking(effort string) string {
	switch NormalizeSessionEffort(effort) {
	case SessionEffortLean:
		return "low"
	case SessionEffortFull:
		return "high"
	default:
		return "auto"
	}
}

// SessionEffortBlurb is the one line /effort and the cycle announce.
func SessionEffortBlurb(effort string) string {
	switch NormalizeSessionEffort(effort) {
	case SessionEffortLean:
		return "pi-core tools; short turns for simple daily asks"
	case SessionEffortFull:
		return "full tool surface; richer workflow for complex work"
	default:
		return "balanced tools and prompt (the shipped default)"
	}
}
