package tui

import (
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

// BlockKind classifies one scrollback block.
type BlockKind int

const (
	KindUser BlockKind = iota
	KindAssistant
	KindThinking
	KindTool     // tool call (name + args summary), status-driven
	KindToolDone // tool result line
	KindSystem   // harness notices (errors, session info)
)

// Block is one scrollback entry. Text is the SOURCE; lines are re-wrapped
// on every draw (reflow on resize for free) with a small per-width cache.
type Block struct {
	Kind     BlockKind
	Text     string
	ToolName string
	Status   string        // tool blocks: "running", "ok", "error"
	Dur      string        // tool result blocks: formatted duration
	Err      bool          // tool result blocks: error result
	stream   bool          // assistant still receiving deltas (dim cursor at tail)
	Ts       time.Time     // block timestamp (user/assistant, drawn right)
	thinkDur time.Duration // thinking: frozen at EndThinking
}

// Width returns the display width of s in cells.
func width(s string) int { return runewidth.StringWidth(s) }

// truncateCells shortens s to at most maxW display cells, appending ell
// (a single-width "…" by convention) when it had to cut.
func truncateCells(s string, maxW int, ell string) string {
	if width(s) <= maxW {
		return s
	}
	return runewidth.Truncate(s, maxW, ell)
}

// fitWidth returns s padded (or truncated) to exactly n display cells, so a
// column of box rows share one right edge.
func fitWidth(s string, n int) string {
	if width(s) > n {
		s = truncateCells(s, n, "…")
	}
	return s + strings.Repeat(" ", max(0, n-width(s)))
}

// wrap breaks s into visual lines of at most maxW cells, preserving empty
// lines. A maxW <= 0 yields one line per source line.
func wrap(s string, maxW int) []string {
	if maxW <= 0 {
		return strings.Split(s, "\n")
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		line := strings.TrimRight(para, " \t")
		if width(line) <= maxW {
			out = append(out, line)
			continue
		}
		// Word wrap; hard-break words longer than maxW.
		for width(line) > maxW {
			cut := maxW
			for cut > 1 && width(line[:cut]) > maxW {
				cut--
			}
			if sp := strings.LastIndexAny(line[:cut], " \t"); sp > 0 {
				cut = sp
			}
			out = append(out, strings.TrimRight(line[:cut], " \t"))
			line = strings.TrimLeft(line[cut:], " ")
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// toolSummary renders the one-line tool-call summary: name(args-preview).
func toolSummary(b *Block, maxW int) string {
	preview := strings.Join(strings.Fields(b.Text), " ")
	if maxW > 3 && len(preview) > maxW*2 {
		preview = preview[:maxW*2] + "…"
	}
	s := "⟨" + b.ToolName + "⟩"
	if preview != "" {
		s += " " + preview
	}
	switch b.Status {
	case "running":
		s += " …"
	case "error":
		s += " [error]"
	}
	return s
}

// blockAccent picks the rail/accent slot for a block.
func (b *Block) accentSlot() string {
	switch b.Kind {
	case KindUser:
		return "accent_user"
	case KindThinking:
		return "accent_thinking"
	case KindTool, KindToolDone:
		return "accent_tool"
	case KindSystem:
		if strings.Contains(strings.ToLower(b.Text), "error") {
			return "accent_error"
		}
		return "accent_success"
	default:
		return "accent_assistant"
	}
}
