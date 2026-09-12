package memory

import (
	"strings"
	"testing"
)

func TestLessonsParseRoundTrip(t *testing.T) {
	b := newTestBackend(t)
	if err := b.SaveLesson("stage explicit paths", "xdev repo"); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveLesson("no backticks in commit -m", ""); err != nil {
		t.Fatal(err)
	}
	lessons := b.Lessons()
	if len(lessons) != 2 {
		t.Fatalf("lessons = %+v, want 2", lessons)
	}
	if lessons[0].Text != "stage explicit paths" || lessons[0].Context != "xdev repo" {
		t.Errorf("lesson[0] = %+v", lessons[0])
	}
	if lessons[0].Date == "" {
		t.Errorf("lesson[0] has no date: %+v", lessons[0])
	}
	if lessons[1].Text != "no backticks in commit -m" || lessons[1].Context != "" {
		t.Errorf("lesson[1] = %+v", lessons[1])
	}

	// A hand edit must not make the log unreadable.
	if err := b.WriteSummary("# Memory\n\n## Facts\n"); err != nil {
		t.Fatal(err)
	}
	summary, _ := b.Paths()
	if got := readFileOrEmpty(summary); !strings.Contains(got, "## Facts") {
		t.Errorf("summary = %q", got)
	}
}

func TestScratchpadIsBoundedAndTrimmed(t *testing.T) {
	b := newTestBackend(t)
	if got := b.Scratchpad(); got != "" {
		t.Fatalf("fresh scratchpad = %q, want empty", got)
	}
	if err := b.SetScratchpad("  note one  \n"); err != nil {
		t.Fatal(err)
	}
	if got := b.Scratchpad(); got != "note one" {
		t.Errorf("scratchpad = %q, want %q", got, "note one")
	}
	if got := b.ScratchpadPath(); !strings.HasSuffix(got, ScratchpadName) {
		t.Errorf("scratchpad path = %q", got)
	}

	// Over-cap text is rejected, not truncated.
	big := strings.Repeat("x", DefaultScratchpadCap)
	if err := b.SetScratchpad(big); err == nil {
		t.Fatal("over-cap scratchpad write must fail")
	}
	if got := b.Scratchpad(); got != "note one" {
		t.Errorf("rejected write changed the file: %q", got)
	}

	if err := b.ClearScratchpad(); err != nil {
		t.Fatal(err)
	}
	if got := b.Scratchpad(); got != "" {
		t.Errorf("after clear scratchpad = %q", got)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	b := newTestBackend(t)
	if err := b.WriteSummary("# Memory\n\n## Decisions\n- stdlib first"); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveLesson("always stage explicit paths", "xdev repo"); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveLesson("mind the pipeline exit status", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.SetScratchpad("half-finished thought"); err != nil {
		t.Fatal(err)
	}
	want, err := b.Export()
	if err != nil {
		t.Fatal(err)
	}
	if want.Version != BundleVersion || want.Summary == "" || want.Lessons == "" || want.Scratchpad == "" {
		t.Fatalf("bundle = %+v", want)
	}

	if err := b.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := b.ClearScratchpad(); err != nil {
		t.Fatal(err)
	}
	if err := b.Import(want, false); err != nil {
		t.Fatal(err)
	}
	got, err := b.Export()
	if err != nil {
		t.Fatal(err)
	}
	got.ExportedAt, want.ExportedAt = "", ""
	if got != want {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if n := len(b.Lessons()); n != 2 {
		t.Errorf("lessons after import = %d, want 2", n)
	}
}

func TestImportMergeIsIdempotent(t *testing.T) {
	b := newTestBackend(t)
	if err := b.WriteSummary("# Memory\n\n## Existing\n- keep me"); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveLesson("existing lesson", ""); err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{
		Version: BundleVersion,
		Summary: "# Memory\n\n## Imported\n- replace me",
		Lessons: "\n- 2026-01-01 — existing lesson\n\n- 2026-01-02 — brand new\n",
	}
	if err := b.Import(bundle, true); err != nil {
		t.Fatal(err)
	}
	if n := len(b.Lessons()); n != 2 {
		t.Fatalf("lessons after merge = %d, want 2 (duplicate skipped)", n)
	}
	if err := b.Import(bundle, true); err != nil {
		t.Fatal(err)
	}
	if n := len(b.Lessons()); n != 2 {
		t.Errorf("re-import added lessons: %d", n)
	}
	summary, _ := b.Paths()
	if got := readFileOrEmpty(summary); !strings.Contains(got, "keep me") {
		t.Errorf("merge clobbered the summary: %q", got)
	}
}

func TestImportRejectsBadBudgetsWithoutTouchingTheStore(t *testing.T) {
	b := newTestBackend(t)
	if err := b.WriteSummary("# Memory\n\n## Keep\n- intact"); err != nil {
		t.Fatal(err)
	}
	if err := b.Import(Bundle{Version: BundleVersion + 7, Summary: "# nope"}, false); err == nil {
		t.Fatal("unsupported bundle version must fail")
	}
	if err := b.Import(Bundle{Version: BundleVersion, Scratchpad: strings.Repeat("y", DefaultScratchpadCap+1)}, false); err == nil {
		t.Fatal("over-cap bundle scratchpad must fail")
	}
	summary, _ := b.Paths()
	if got := readFileOrEmpty(summary); !strings.Contains(got, "intact") {
		t.Errorf("a rejected import modified the store: %q", got)
	}
}

func TestInfoReportsOnDiskState(t *testing.T) {
	b := newTestBackend(t)
	if err := b.WriteSummary("# Memory\n\n## Facts\n- one\n"); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveLesson("lesson a", ""); err != nil {
		t.Fatal(err)
	}
	info := b.Info()
	if info.Dir != b.Dir || info.LessonCount != 1 {
		t.Errorf("info = %+v", info)
	}
	if info.Summary.Bytes == 0 || info.Summary.Lines != 4 {
		t.Errorf("summary info = %+v (want 4 lines)", info.Summary)
	}
	if info.Lessons.Bytes == 0 || info.Lessons.Path == "" {
		t.Errorf("lessons info = %+v", info.Lessons)
	}
	if info.ScratchpadCap != DefaultScratchpadCap {
		t.Errorf("scratchpad cap = %d", info.ScratchpadCap)
	}

	var off *Backend
	if got := off.Info(); got.Dir != "" || got.LessonCount != 0 {
		t.Errorf("off info = %+v", got)
	}
}
