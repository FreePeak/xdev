package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rename rewrites the fixed-width title slot in place (#107): line 1 is
// exactly TitleSlotWidth bytes before and after, later entries are
// byte-identical, and a reload shows the new title with the source recorded.

func TestRenameRewritesSlotInPlace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	st := OpenMem("/proj", "original title")
	if _, err := st.EnsureOnDisk(p, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(userMsg("m1", "", "hello")); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Capture the bytes after the title slot for the no-shift assertion.
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < TitleSlotWidth {
		t.Fatalf("file too short to hold a title slot: %d", len(before))
	}
	tailBefore := string(before[TitleSlotWidth:])

	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Rename("renamed by the user", TitleSourceManual); err != nil {
		t.Fatal(err)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// The slot stays fixed-width, so everything behind it is unmoved.
	if len(after) != len(before) {
		t.Fatalf("rename changed the file length: %d -> %d", len(before), len(after))
	}
	if string(after[TitleSlotWidth:]) != tailBefore {
		t.Fatal("rename shifted bytes behind the title slot")
	}
	title, source, ok := ParseTitleSlotSource(after[:TitleSlotWidth-1])
	if !ok || title != "renamed by the user" || source != TitleSourceManual {
		t.Fatalf("slot after rename = %q %q ok=%v", title, source, ok)
	}
	// A reload serves the new title.
	st3, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	if st3.Title() != "renamed by the user" {
		t.Fatalf("reloaded title = %q", st3.Title())
	}
}

// An empty title is refused; a store still in memory accepts the rename and
// just carries it to the eventual materialization.
func TestRenameEdgeCases(t *testing.T) {
	st := OpenMem("/proj", "")
	if err := st.Rename("  ", TitleSourceManual); err == nil {
		t.Fatal("an empty title must be refused")
	}
	if err := st.Rename("kept for later", TitleSourceManual); err != nil {
		t.Fatal(err)
	}
	if st.Title() != "kept for later" {
		t.Fatalf("in-memory rename lost: %q", st.Title())
	}
}

// A rename after append: the buffered writes flush before the slot rewrite,
// so nothing is lost and the order on disk is slot + entries.
func TestRenameAfterAppendFlushes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	st := OpenMem("/proj", "first")
	if _, err := st.EnsureOnDisk(p, Options{}); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"one", "two", "three"} {
		if err := st.Append(userMsg(string(rune('a'+i)), "", text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Rename("after appends", TitleSourceAuto); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two", "three", "after appends"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("lost %q in the flush-before-rewrite: file=%s", want, raw)
		}
	}
}
