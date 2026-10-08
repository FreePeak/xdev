package config

import "strings"

// Session effort is the one dial that scales how hard a session works:
// tools advertised eagerly, optional prompt appendix, and the thinking
// default when `thinking` is still "auto". It is NOT the permission
// posture (/plan, approvalMode) and NOT the request-side reasoning budget
// (:effort / /thinking) — those stay their own knobs. A plain string key on
// Settings, like Thinking: "" means unset in this layer, default medium.
//
// The vocabulary is Claude Code's own effort ladder (low → max), adopted
// 2026-10-08: `/effort`, `--effort`, the `effort` settings key and the
// XDEV_EFFORT_LEVEL env var all name one of these five rungs, and a rung a
// model cannot reach is CLAMPED to the highest it can rather than rejected
// (config.ClampEffort — the same rule CC applies). The pre-ladder spellings
// (lean|standard|full, the omp names omp|complex|simple) stay as aliases so
// a stored `effort: full` keeps resolving; they are not part of the closed
// write vocabulary, which is what makes `/effort lean` a usage error and
// `/effort low` the spelling to use.
//
// deliberately NOT in repoSafeSettingsKeys: a cloned repository must not
// force how much of the tool surface and reasoning budget a user spends.

const (
	// SessionEffortLow is pi-core tools only — daily simple asks.
	SessionEffortLow = "low"
	// SessionEffortMedium is today's xdev default (partial deferred catalog).
	SessionEffortMedium = "medium"
	// SessionEffortHigh opens the omp-shaped surface for complex work.
	SessionEffortHigh = "high"
	// SessionEffortXHigh and SessionEffortMax are the top two rungs. They open
	// the same tool surface as high; what they buy is the wider reasoning
	// budget (config.EffortTokens) on a model that advertises the rung.
	SessionEffortXHigh = "xhigh"
	SessionEffortMax   = "max"
)

// SessionEffortDefault is the rung an unset `effort` key resolves to — the
// shipped default, and the same middle rung CC starts a model at.
const SessionEffortDefault = SessionEffortMedium

// SessionEffortLevels is the closed vocabulary /effort, --effort and the
// settings key share, ordered low→high.
var SessionEffortLevels = []string{
	SessionEffortLow, SessionEffortMedium, SessionEffortHigh, SessionEffortXHigh, SessionEffortMax,
}

// SessionEffortCycle is the Shift-Tab order: low → … → max → low.
// Every rung is safe to cycle; none widens permissions (that is /plan +
// approvalMode).
var SessionEffortCycle = []string{
	SessionEffortLow, SessionEffortMedium, SessionEffortHigh, SessionEffortXHigh, SessionEffortMax,
}

// IsSessionEffort reports whether v is one of the five writeable rungs (not
// an alias). /effort, --effort and config set use this so "full" is rejected
// with a usage error rather than silently stored under a different spelling.
func IsSessionEffort(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case SessionEffortLow, SessionEffortMedium, SessionEffortHigh, SessionEffortXHigh, SessionEffortMax:
		return true
	}
	return false
}

// sessionEffortAliases are the pre-ladder spellings a STORED value may still
// carry (the vocabulary this key shipped before the ladder was adopted, plus
// the omp names). They resolve to a rung through NormalizeSessionEffort but
// are not writeable — see IsSessionEffortValue.
var sessionEffortAliases = []string{
	"lean", "simple", "min", "minimal",
	"standard", "default", "normal", "auto",
	"full", "omp", "complex",
}

// IsSessionEffortValue reports whether v is a value this build can resolve to
// a rung: the writeable ladder or a pre-ladder alias. It is the LOAD-time
// check, and it exists because a rejected settings file is moved aside as
// *.broken-* with every other setting lost — a config written before the
// ladder existed (`effort: full`) must keep loading. The write paths
// (/effort, --effort, `xdev config set`) keep the closed IsSessionEffort
// vocabulary, so the spelling on disk stays canonical while an old one still
// reads.
func IsSessionEffortValue(v string) bool {
	if IsSessionEffort(v) {
		return true
	}
	for _, a := range sessionEffortAliases {
		if strings.EqualFold(strings.TrimSpace(v), a) {
			return true
		}
	}
	return false
}

// NormalizeSessionEffort folds aliases and empty onto a canonical rung.
// Unknown values fall back to the default (never low — a typo must not
// silently strip tools; never max — a typo must not silently widen).
//
// The pre-ladder names map onto their nearest rung rather than 404ing: a
// session store or config file written before the ladder existed keeps
// working, and `minimal`/`simple` (the lightest spellings) land on low.
func NormalizeSessionEffort(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case SessionEffortLow, "lean", "simple", "min", "minimal":
		return SessionEffortLow
	case SessionEffortHigh, "full", "omp", "complex":
		return SessionEffortHigh
	case SessionEffortXHigh:
		return SessionEffortXHigh
	case SessionEffortMax:
		return SessionEffortMax
	case SessionEffortMedium, "standard", "default", "normal", "auto", "":
		return SessionEffortDefault
	default:
		return SessionEffortDefault
	}
}

// SessionEffortThinking is the request-side reasoning level this effort
// implies when the user's `thinking` key is still "auto". medium leaves the
// model / :effort alone; the rungs below and above it pin their own level,
// which the model-aware fold (thinkingForModel) then clamps to what the
// model advertises.
func SessionEffortThinking(effort string) string {
	switch NormalizeSessionEffort(effort) {
	case SessionEffortLow:
		return "low"
	case SessionEffortHigh:
		return "high"
	case SessionEffortXHigh:
		return "xhigh"
	case SessionEffortMax:
		return "max"
	default:
		return "auto"
	}
}

// SessionEffortBlurb is the one line /effort and the cycle announce.
func SessionEffortBlurb(effort string) string {
	switch NormalizeSessionEffort(effort) {
	case SessionEffortLow:
		return "pi-core tools; short turns for simple daily asks"
	case SessionEffortHigh:
		return "full tool surface; richer workflow for complex work"
	case SessionEffortXHigh:
		return "full surface at a wider reasoning budget"
	case SessionEffortMax:
		return "full surface at the deepest reasoning budget"
	default:
		return "balanced tools and prompt (the shipped default)"
	}
}
