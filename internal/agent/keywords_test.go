package agent

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

func TestScanMagicKeywords(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int // number of notices
	}{
		{"plain", "fix the bug in main.go", 0},
		{"ultrathink standalone", "ultrathink about this design", 1},
		{"mixed case does not fire", "Please UltraThink first", 0},
		{"mid-prose", "so, ultrathink: is this safe?", 1},
		{"attached word", "foo-ultrathink or ultrathinking is not prose", 0},
		{"path char", "run /ultrathink now", 0},
		{"digits", "ultrathink2 is not a keyword", 0},
		{"in fenced code", "```\nultrathink\n```", 0},
		{"in inline code", "use `ultrathink` carefully", 0},
		{"in html comment", "<!-- ultrathink --> real ask", 0},
		{"all three", "ultrathink, orchestrate, workflowz", 3},
		{"orchestrate alone", "please orchestrate this across agents", 1},
		{"workflowz alone", "workflowz the pipeline", 1},
		{"repeat is one notice", "ultrathink and ultrathink again", 1},
		// Punctuation may touch the word; a trailing period must not block.
		{"trailing period", "just ultrathink.", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanMagicKeywords(tc.text)
			if len(got) != tc.want {
				t.Fatalf("ScanMagicKeywords(%q) = %d notices, want %d: %v", tc.text, len(got), tc.want, got)
			}
		})
	}
}

func TestScanMagicKeywordsEmptyAndNil(t *testing.T) {
	if got := ScanMagicKeywords(""); got != nil && len(got) != 0 {
		t.Fatalf("empty text: %v", got)
	}
}

func TestMagicKeywordMessagesShape(t *testing.T) {
	msgs := MagicKeywordMessages("ultrathink this")
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d", len(msgs))
	}
	if msgs[0].Role != ai.RoleUser {
		t.Fatalf("notice role = %v, want user-attributed", msgs[0].Role)
	}
	if !strings.Contains(noticeText(msgs[0]), "highest effort") {
		t.Fatalf("notice text = %q", noticeText(msgs[0]))
	}
}

func noticeText(m ai.Message) string {
	var out string
	for _, b := range m.Content {
		if tb, ok := b.(ai.TextBlock); ok {
			out += tb.Text
		}
	}
	return out
}
