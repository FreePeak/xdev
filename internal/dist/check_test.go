package dist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// checkDir points the whole check surface (record file, and with XDEV_AGENT_DIR
// set, every state path) at a temp directory. HOME and XDG_CONFIG_HOME move too,
// so the job-installed tests below never touch the real LaunchAgents dir.
func checkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	return dir
}

func TestCheckRecordRoundTrip(t *testing.T) {
	checkDir(t)
	if rec := ReadCheck(); !rec.CheckedAt.IsZero() {
		t.Fatalf("a fresh state dir reports a check: %+v", rec)
	}
	want := CheckRecord{
		CheckedAt: time.Now().Truncate(time.Second),
		Channel:   ChannelCanary,
		Version:   "v1.2.3",
		Running:   "v1.0.0",
	}
	if err := WriteCheck(want); err != nil {
		t.Fatal(err)
	}
	got := ReadCheck()
	if got.Channel != want.Channel || got.Version != want.Version || got.Running != want.Running {
		t.Errorf("record = %+v, want %+v", got, want)
	}
	if !got.CheckedAt.Equal(want.CheckedAt) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, want.CheckedAt)
	}
	// The file is the interface between the job and the session start, so its
	// shape is worth pinning: one JSON object, the throttle field readable.
	raw, err := os.ReadFile(CheckPath())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("record is not valid JSON: %v\n%s", err, raw)
	}
	if _, ok := fields["checkedAt"]; !ok {
		t.Errorf("record has no checkedAt field: %s", raw)
	}
	if fi, err := os.Stat(CheckPath()); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("record mode = %v, want private to the user", fi.Mode())
	}
	// Writing again replaces the record rather than appending to it, and leaves
	// no temp file behind in the state dir.
	if err := WriteCheck(CheckRecord{CheckedAt: want.CheckedAt.Add(time.Hour), Version: "v1.2.4"}); err != nil {
		t.Fatal(err)
	}
	if got := ReadCheck(); got.Version != "v1.2.4" || got.Channel != "" {
		t.Errorf("second write did not replace the record: %+v", got)
	}
	ents, err := os.ReadDir(filepath.Dir(CheckPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "."+CheckFileName+"-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestReadCheckIgnoresGarbage(t *testing.T) {
	checkDir(t)
	if err := os.WriteFile(CheckPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := ReadCheck(); !rec.CheckedAt.IsZero() {
		t.Errorf("a malformed record read as %+v, want the zero record", rec)
	}
}

func TestNoticeFor(t *testing.T) {
	now := time.Date(2026, 3, 21, 12, 0, 0, 0, time.UTC)
	fresh := CheckRecord{CheckedAt: now.Add(-time.Hour), Channel: ChannelStable, Version: "v1.2.0", Running: "v1.0.0"}

	tests := []struct {
		name    string
		rec     CheckRecord
		version string
		want    string // "" = no notice; otherwise a substring to match
	}{
		{"never checked", CheckRecord{Running: "v1.0.0"}, "v1.0.0", ""},
		{"check found nothing", CheckRecord{CheckedAt: now.Add(-time.Hour)}, "v1.0.0", ""},
		{"failed check", CheckRecord{CheckedAt: now.Add(-time.Hour), Error: "dial tcp: no route"}, "v1.0.0", ""},
		{"up to date", fresh, "v1.2.0", ""},
		{"ahead of the record", fresh, "v1.3.0", ""},
		{"stale clock", CheckRecord{CheckedAt: now.Add(time.Hour), Version: "v1.2.0"}, "v1.0.0", ""},
		{"older than a week", CheckRecord{CheckedAt: now.Add(-8 * 24 * time.Hour), Version: "v1.2.0"}, "v1.0.0", ""},
		{"no running version", fresh, "", ""},
		{"newer release", fresh, "v1.0.0", "xdev v1.2.0 is available (you have v1.0.0, channel stable)"},
		{"canary channel", CheckRecord{CheckedAt: now.Add(-time.Hour), Channel: ChannelCanary, Version: "v1.3.0-rc.1"}, "v1.0.0", "channel canary"},
		{"unknown running version", CheckRecord{CheckedAt: now.Add(-time.Hour), Version: "v1.2.0"}, "dev", "you have dev"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := noticeFor(tc.rec, tc.version, now)
			if tc.want == "" {
				if got != "" {
					t.Errorf("noticeFor = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("noticeFor = %q, want it to contain %q", got, tc.want)
			}
			// The notice is notify-only: it must name the command, not run it.
			if !strings.Contains(got, "run `xdev update`") {
				t.Errorf("notice does not tell the user how to install: %q", got)
			}
		})
	}
}

// countSpawns replaces the detached launcher with a counter that also writes a
// fresh record — the contract the real child keeps, since the throttle reads
// that file rather than any in-process marker.
func countSpawns(t *testing.T) *[]string {
	t.Helper()
	prev := spawnCheck
	var seen []string
	spawnCheck = func(version string) error {
		seen = append(seen, version)
		return WriteCheck(CheckRecord{CheckedAt: time.Now(), Version: "v1.0.0", Running: version})
	}
	t.Cleanup(func() { spawnCheck = prev })
	return &seen
}

func TestMaybeCheckThrottle(t *testing.T) {
	checkDir(t)
	t.Setenv("XDEV_UPDATE_CHECK", "1") // force the terminal test away from isatty
	spawns := countSpawns(t)

	MaybeCheck("v1.0.0") // never checked → due
	if len(*spawns) != 1 {
		t.Fatalf("first launch spawned %d checks, want 1", len(*spawns))
	}
	MaybeCheck("v1.0.0") // this window is paid for
	if len(*spawns) != 1 {
		t.Errorf("a fresh record did not throttle the check: %d spawns", len(*spawns))
	}
	if err := WriteCheck(CheckRecord{CheckedAt: time.Now().Add(-checkEvery - time.Minute), Version: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	MaybeCheck("v1.0.0") // stale → due again
	if len(*spawns) != 2 {
		t.Errorf("a record older than %v did not re-arm the check: %d spawns", checkEvery, len(*spawns))
	}
}

func TestMaybeCheckRefusals(t *testing.T) {
	t.Run("unstamped build", func(t *testing.T) {
		checkDir(t)
		t.Setenv("XDEV_UPDATE_CHECK", "1")
		spawns := countSpawns(t)
		MaybeCheck("dev") // not a release version: nothing would replace it
		if len(*spawns) != 0 {
			t.Errorf("spawned %d checks for an unstamped build", len(*spawns))
		}
	})
	t.Run("kill switch", func(t *testing.T) {
		checkDir(t)
		t.Setenv("XDEV_UPDATE_CHECK", "off")
		spawns := countSpawns(t)
		MaybeCheck("v1.0.0")
		if len(*spawns) != 0 {
			t.Errorf("XDEV_UPDATE_CHECK=off still spawned %d checks", len(*spawns))
		}
	})
	t.Run("job owns the checking", func(t *testing.T) {
		dir := checkDir(t)
		t.Setenv("XDEV_UPDATE_CHECK", "1")
		spawns := countSpawns(t)
		// Lay the schedule file where this platform's JobSchedulePath looks
		// (the same two paths jobInstall writes), then expect no self-check.
		path := launchdPlistPath()
		if runtime.GOOS != "darwin" {
			path = systemdUnitPath(jobTimerUnit())
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !JobInstalled() {
			t.Fatalf("JobInstalled() false with %s in place (state dir %s)", path, dir)
		}
		MaybeCheck("v1.0.0")
		if len(*spawns) != 0 {
			t.Errorf("spawned %d checks while a job owns the schedule", len(*spawns))
		}
	})
}

func TestReportCheck(t *testing.T) {
	checkDir(t)
	var out strings.Builder
	ReportCheck(&out)
	if !strings.Contains(out.String(), "no update check has run yet") {
		t.Fatalf("empty report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "job:         none") {
		t.Errorf("report does not say no job is installed:\n%s", out.String())
	}

	if err := WriteCheck(CheckRecord{CheckedAt: time.Now().Add(-3 * time.Hour), Channel: ChannelStable, Version: "v1.2.0", Running: "v1.0.0", Error: "timeout"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	ReportCheck(&out)
	got := out.String()
	for _, want := range []string{"last check:", "3.0h ago", "channel:     stable", "latest:      v1.2.0", "failed:      timeout", CheckPath()} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
}
