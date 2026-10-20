package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// spillDir points a FileOffloader at a temp dir.
func spillDir(t *testing.T) (FileOffloader, string) {
	t.Helper()
	dir := t.TempDir()
	return FileOffloader{Dir: dir, MaxLines: offloadLines, MaxBytes: offloadBytes}, dir
}

// A result that fits is declined: the cheap majority never reaches the disk.
func TestFileOffloaderDeclinesAResultThatFits(t *testing.T) {
	f, dir := spillDir(t)
	stub, ok, err := f.Offload("bash", "c1", "one\ntwo\nthree")
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if ok {
		t.Fatalf("a small result was offloaded: %q", stub)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a small result wrote %d files", len(entries))
	}
}

// An oversized result keeps its head, says what it dropped, and names the file
// that holds the rest: the model keeps a way to the data the request did not
// carry.
func TestFileOffloaderSpillsTheTail(t *testing.T) {
	f, dir := spillDir(t)
	var b strings.Builder
	for range offloadLines + 500 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 15))
		b.WriteString("\n")
	}
	in := b.String()

	stub, ok, err := f.Offload("bash", "c1", in)
	if err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if !ok {
		t.Fatal("an oversized result was not offloaded")
	}
	if !strings.HasPrefix(stub, "line xxxxxxxxxxxxxxx\n") {
		t.Fatalf("the head was dropped: %q", stub[:min(len(stub), 200)])
	}
	marker := "[showing lines 1-2000 of 2500; full output saved to "
	i := strings.Index(stub, marker)
	if i < 0 {
		t.Fatalf("no marker in %q", stub[max(0, len(stub)-200):])
	}
	// The marker names a file that holds the WHOLE output, not the head.
	path := strings.TrimSuffix(stub[i+len(marker):], "]")
	if filepath.Dir(path) != dir {
		t.Fatalf("the spill landed in %q, want %q", filepath.Dir(path), dir)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the spilled output is unreadable: %v", err)
	}
	if string(full) != in {
		t.Fatalf("the spilled output is %d bytes, want the full %d", len(full), len(in))
	}
}

// The byte budget cuts the head short of the line cap: a result whose lines
// are large keeps fewer lines than the cap allows.
func TestFileOffloaderCutsOnBytes(t *testing.T) {
	f, _ := spillDir(t)
	line := strings.Repeat("y", 4096)
	var b strings.Builder
	for range offloadLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	stub, ok, err := f.Offload("bash", "c1", b.String())
	if err != nil || !ok {
		t.Fatalf("Offload: ok=%v err=%v", ok, err)
	}
	// 50 KiB / 4 KiB per line is 12 lines.
	if !strings.Contains(stub, "[showing lines 1-12 of 2000;") {
		t.Fatalf("the byte budget did not cut the head: %q", stub[max(0, len(stub)-120):])
	}
}

// One line past the byte cap is one line past the budget: nothing of it fits,
// and the marker says so rather than pretending a head was kept.
func TestFileOffloaderKeepsNothingOfAnOversizedLine(t *testing.T) {
	f, _ := spillDir(t)
	stub, ok, err := f.Offload("bash", "c1", strings.Repeat("z", offloadBytes+1))
	if err != nil || !ok {
		t.Fatalf("Offload: ok=%v err=%v", ok, err)
	}
	if !strings.HasPrefix(stub, "[showing 0 lines of 1;") {
		t.Fatalf("an oversized line was partially kept: %q", stub[:min(len(stub), 120)])
	}
}

// The same output spilled twice is one file: the marker keeps naming a path
// that exists, and a repeated command does not leave a trail of copies.
func TestFileOffloaderSpillsOncePerOutput(t *testing.T) {
	f, dir := spillDir(t)
	in := strings.Repeat("z\n", offloadLines+1)
	first, ok, err := f.Offload("bash", "c1", in)
	if err != nil || !ok {
		t.Fatalf("Offload: ok=%v err=%v", ok, err)
	}
	second, ok, err := f.Offload("bash", "c2", in)
	if err != nil || !ok {
		t.Fatalf("Offload: ok=%v err=%v", ok, err)
	}
	if first != second {
		t.Fatalf("the same output produced two stubs:\n%q\n%q", first, second)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the same output wrote %d files, want 1", len(entries))
	}
}

// A spill that fails is an error, never a stub claiming bytes that were not
// written (#115's data-loss rule): the caller keeps the result verbatim.
func TestFileOffloaderReportsAFailedSpill(t *testing.T) {
	// A regular file where the directory should be: the write cannot land.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := FileOffloader{Dir: blocker, MaxLines: offloadLines, MaxBytes: offloadBytes}
	stub, ok, err := f.Offload("bash", "c1", strings.Repeat("z", offloadBytes+1))
	if err == nil {
		t.Fatal("a failed spill reported no error")
	}
	if ok {
		t.Fatalf("a failed spill returned a stub: %q", stub)
	}
}

// The seam and the shipped backend work together: an oversized result in a
// turn comes back as the head plus the path, and a result that fits is left
// alone.
func TestOffloadResultBoundsTheConversation(t *testing.T) {
	f, _ := spillDir(t)
	a := &Agent{Offload: f}
	call := aiToolCall("bash", "{}")

	small := a.offloadResult(call, tool.Result{Text: "short"})
	if small.Text != "short" {
		t.Fatalf("a small result was rewritten: %q", small.Text)
	}

	big := strings.Repeat("out\n", offloadLines+1000)
	big += strings.Repeat("filler\n", offloadBytes/7)
	bound := a.offloadResult(call, tool.Result{Text: big})
	if len(bound.Text) >= len(big) {
		t.Fatalf("the result carries %d bytes, the tool produced %d", len(bound.Text), len(big))
	}
	if !strings.Contains(bound.Text, "full output saved to") {
		t.Fatalf("the result does not say where the rest went: %q", bound.Text[max(0, len(bound.Text)-120):])
	}
}
