package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// webURLStart now skips every position whose byte is not 'h'/'H' before it
// checks a boundary or folds a case, which is a pure reordering of the same
// test: the answer must be identical at every position of every string. The
// scheme can only start at a byte the literal starts at, so a miss is free.
// This pins the equivalence on the shapes the detector is specified against,
// including the ones that make the old scan's boundary walk expensive (a
// combining mark before a scheme, a scheme inside a word, uppercase).
func TestWebURLStartMatchesLinearScan(t *testing.T) {
	reference := func(s string) int {
		for i := 0; i+7 <= len(s); i++ {
			if !urlBoundary(s, i) {
				continue
			}
			if i+8 <= len(s) && asciiEqualFold(s[i:i+8], "https://") {
				return i
			}
			if asciiEqualFold(s[i:i+7], "http://") {
				return i
			}
		}
		return -1
	}
	fixtures := []string{
		"",
		"plain text with no link at all, just words and punctuation; and more words",
		"see https://example.com/path for details",
		"see http://example.com",
		"HTTPS://example.com/path",
		"HTTP://example.com",
		"abchttps://example.com",
		"•https://example.com/path",
		"éhttps://example.com",
		"e\u0301https://example.com",
		"h",
		"ht",
		"htt",
		"http",
		"http:",
		"http:/",
		"hthttp://example.com",
		"hhhhttps://example.com",
		"(https://example.com/a)",
		"x\nhttps://example.com",
		strings.Repeat("h", 200) + "https://example.com",
		strings.Repeat("word ", 500) + "https://example.com/tail",
		"h" + strings.Repeat("é", 100) + "https://example.com",
	}
	for _, s := range fixtures {
		if got, want := webURLStart(s), reference(s); got != want {
			t.Errorf("webURLStart(%q) = %d, linear scan says %d", truncateForMessage(s), got, want)
		}
	}
}

func truncateForMessage(s string) string {
	if len(s) > 60 {
		return s[:30] + "…" + s[len(s)-30:]
	}
	return s
}

// The scan still has to agree with the detector the tests above it specify:
// every URL the renderer paints comes through webURLStart.
func TestWebURLFastPathStillDetects(t *testing.T) {
	text := "before " + strings.Repeat("noise ", 200) + "https://example.com/end and after"
	runs := appendWebURLRuns(nil, text, tcell.StyleDefault, tcell.StyleDefault)
	found := ""
	for _, r := range runs {
		if r.link != "" {
			found = r.link
		}
	}
	if found != "https://example.com/end" {
		t.Fatalf("link = %q, want the URL after the noise", found)
	}
}
