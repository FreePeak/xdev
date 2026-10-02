package tui

import (
	"strings"
	"testing"
)

func TestSanitizeOutputDropsFFFD(t *testing.T) {
	// A thinking body that carried U+FFFD (from a prior corrupt stream)
	// must render without the replacement character. English, Mandarin
	// and Vietnamese diacritics stay intact.
	in := "plan: hello 你好 Xin chào\uFFFD junk"
	got := sanitizeOutput(in)
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("sanitizeOutput kept U+FFFD: %q", got)
	}
	for _, keep := range []string{"hello", "你好", "Xin chào", "junk"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("sanitizeOutput dropped %q from %q", keep, got)
		}
	}
}
