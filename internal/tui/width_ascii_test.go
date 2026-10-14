package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// width() answers ASCII with a byte scan, so the one thing that can go wrong is
// the scan disagreeing with the table it replaced — a word measured one cell
// short wraps a line one cell too long, and every border under it lands wrong.
// The two must agree on every string a renderer can hand it, ASCII or not.
func TestWidthMatchesRunewidth(t *testing.T) {
	for _, s := range []string{
		"", "a", "abc", "a b", "  ", "\t", "a\tb",
		"hello world, this is prose.",
		"\x01", "a\x01b", "\x00", "a\x7fb", "\x7f",
		"é", "aé", "café", "e\u0301", "\u200b", "a\u200bb",
		"漢", "漢字テスト", "日本語のテキストです",
		"👍", "a👍b", "🇻🇳",
		"\x1b[31m", "x\x1b[31m", "│", "❯ ", "▍", "…", "·", "─",
		"a\x00b\x01c\x7f", "mixed é 漢 👍 text",
		strings.Repeat("x", 1000),
		strings.Repeat("漢", 500),
	} {
		if got, want := width(s), runewidth.StringWidth(s); got != want {
			t.Errorf("width(%q) = %d, runewidth says %d", s, got, want)
		}
	}
}

// The fast path is the reason the function exists, so it must actually be taken
// for ASCII and declined for anything else — a scan that silently fell back
// would leave the cost where it was.
func TestWidthFastPathSelection(t *testing.T) {
	if n, ok := asciiWidth("plain ascii prose"); !ok || n != len("plain ascii prose") {
		t.Fatalf("asciiWidth(ascii) = %d, %v", n, ok)
	}
	if _, ok := asciiWidth("héllo"); ok {
		t.Fatal("asciiWidth took the fast path on a non-ASCII string")
	}
	if _, ok := asciiWidth("漢"); ok {
		t.Fatal("asciiWidth took the fast path on a wide rune")
	}
	// A control byte is zero cells, not one: the count must not treat it as
	// printable just because it is ASCII.
	if n, ok := asciiWidth("a\x01b"); !ok || n != 2 {
		t.Fatalf("asciiWidth(%q) = %d, %v; want 2, true", "a\x01b", n, ok)
	}
}
