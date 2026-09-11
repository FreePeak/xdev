package agent

import (
	"regexp"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
)

// Magic keywords (M11 #12, research §8): standalone exact-lowercase prose
// words in a user prompt that inject a hidden, user-attributed notice for
// that turn. Matching rules (omp parity):
//   - exact lowercase only: "UltraThink" does not fire;
//   - standalone prose: punctuation may touch the word, letters/digits or
//     path characters may not (foo-ultrathink or /ultrathink do not fire);
//   - fenced code blocks, inline code, and HTML comments are ignored.

const (
	noticeUltrathink  = "The user asked you to think carefully about this request. Reason at your highest effort: examine the problem from first principles, consider edge cases and failure modes, and verify your conclusions before acting."
	noticeOrchestrate = "The user asked you to orchestrate this across multiple agents. Scope the work into independent slices, delegate each to a task subagent in parallel where they can run concurrently, and verify each phase's result before proceeding to the next. Do not serialize work that has no dependency."
	noticeWorkflowz   = "The user asked for a deterministic multi-subagent workflow. Define the steps explicitly, run each as a subagent with a clear input/output contract, and chain results deterministically rather than improvising between steps."
)

var magicKeywords = []struct {
	word   string
	notice string
}{
	{"ultrathink", noticeUltrathink},
	{"orchestrate", noticeOrchestrate},
	{"workflowz", noticeWorkflowz},
}

// ponytail: omp gates workflowz on eval+task tools being active; xdev's
// Run entry does not model the active toolset yet, so the notice fires on
// the keyword alone. Upgrade path: thread the tool registry into Run and
// gate this entry when task is absent.

// codeSpanRe strips fenced blocks, inline code, and HTML comments before
// keyword matching so prose keywords inside code never fire.
var (
	fenceRe   = regexp.MustCompile("(?s)```.*?```")
	inlineRe  = regexp.MustCompile("`[^`\\n]*`")
	commentRe = regexp.MustCompile("(?s)<!--.*?-->")
)

// ScanMagicKeywords returns the notices for keywords present in the user's
// text, in canonical order. Empty when nothing matches.
func ScanMagicKeywords(text string) []string {
	if !strings.ContainsAny(text, "ouw") {
		return nil // cheap prefilter: no keyword's first letter present
	}
	stripped := commentRe.ReplaceAllString(text, " ")
	stripped = fenceRe.ReplaceAllString(stripped, " ")
	stripped = inlineRe.ReplaceAllString(stripped, " ")
	var out []string
	for _, kw := range magicKeywords {
		// Standalone: the keyword bounded by non-word characters. \w covers
		// letters/digits/underscore; path characters (/ - .) are not \w,
		// so "foo-ultrathink" or "/ultrathink" must be excluded explicitly.
		// Matching is case-sensitive against the already-lowercase keyword
		// ("UltraThink" does not fire).
		re := regexp.MustCompile(`(^|[ \t\n,;:!?"'()\[\]{}])` + kw.word + `($|[ \t\n,;:!?"'()\[\]{}.])`)
		if re.MatchString(stripped) {
			out = append(out, kw.notice)
		}
	}
	return out
}

// MagicKeywordMessagesForTurns builds the hidden user-attributed notices
// for a keyword-bearing prompt. hasTask reports whether the task tool is
// active; workflowz is spec'd to fire only when eval+task are active
// (omp §8), so a missing task tool drops it. ultrathink and orchestrate
// fire regardless.
func MagicKeywordMessagesForTurns(text string, hasTask bool) []ai.Message {
	var out []ai.Message
	for _, n := range ScanMagicKeywords(text) {
		if n == noticeWorkflowz && !hasTask {
			continue
		}
		out = append(out, ai.Message{
			Role:        ai.RoleUser,
			Content:     []ai.Block{ai.TextBlock{Text: n}},
			Attribution: "user",
		})
	}
	return out
}

// MagicKeywordMessages builds the hidden user-attributed notices that
// accompany a keyword-bearing prompt for this turn.
func MagicKeywordMessages(text string) []ai.Message {
	notices := ScanMagicKeywords(text)
	msgs := make([]ai.Message, 0, len(notices))
	for _, n := range notices {
		msgs = append(msgs, ai.Message{
			Role:        ai.RoleUser,
			Content:     []ai.Block{ai.TextBlock{Text: n}},
			Attribution: "user",
		})
	}
	return msgs
}
