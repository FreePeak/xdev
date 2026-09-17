package dist

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/FreePeak/xdev/internal/config"
)

// Background version checks: the read half of "look for a new release twice a
// day". A check writes its answer to one small file under the state dir; every
// session start reads that file and says one line when a newer release is
// waiting. The process that triggers a check never waits for it and never shows
// its result — the result belongs to the next launch, which is the point of a
// notice.
//
// The record is the whole interface between the two halves, so the check itself
// can be driven by whatever the machine already has: the launchd plist or
// systemd user timer that `xdev update job install` writes, a crontab line, or
// — with no job installed — the first interactive launch of each 12h window,
// which spawns a detached `xdev update --check`. Nothing here installs
// anything: a check reports, `xdev update` installs.

const (
	// checkEvery is the minimum spacing between two checks: twice a day.
	checkEvery = 12 * time.Hour
	// CheckFileName is the record's name under the state dir.
	CheckFileName = "update-check.json"
	// noticeMaxAge bounds how long "a newer release exists" stays worth
	// repeating. Past it the record means nobody is checking anymore, and a
	// notice about a three-week-old release is noise, not news.
	noticeMaxAge = 7 * 24 * time.Hour
	// checkSpawnTimeout bounds one detached check: long enough for a slow
	// mirror, short enough that an orphan cannot outlive its purpose.
	checkSpawnTimeout = 2 * time.Minute
)

// CheckRecord is one update check. CheckedAt is the field the throttle reads,
// so it is recorded even when the check failed — a host with no route to GitHub
// must not re-try on every launch.
type CheckRecord struct {
	CheckedAt time.Time `json:"checkedAt"`
	Channel   string    `json:"channel,omitempty"`
	Version   string    `json:"version,omitempty"` // newest release the check resolved ("" = none or failed)
	Running   string    `json:"running,omitempty"` // the version that asked
	Error     string    `json:"error,omitempty"`   // why there is no answer, if there is none
}

// CheckPath is the update-check record: <state-dir>/update-check.json. StateDir
// already honours XDEV_AGENT_DIR, so a test points the whole check surface at a
// temp directory by setting it, exactly like every other xdev path.
func CheckPath() string {
	return filepath.Join(config.StateDir(), CheckFileName)
}

// WriteCheck records one check, atomically: temp file + rename, so a reader in
// another process sees either the old record or the new one, never half.
func WriteCheck(rec CheckRecord) error {
	path := CheckPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+CheckFileName+"-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // no-op after the rename; discards a partial write
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	// The record names versions and nothing secret; 0600 because the state dir
	// is private and a 0644 file there would be the odd one out.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	return os.Rename(tmp.Name(), path)
}

// ReadCheck returns the last recorded check. A missing or malformed file is the
// zero record: the record is a cache, and a cache that cannot be read means "no
// news", not a problem the user has to hear about.
func ReadCheck() CheckRecord {
	var rec CheckRecord
	raw, err := os.ReadFile(CheckPath())
	if err != nil {
		return rec
	}
	if json.Unmarshal(raw, &rec) != nil {
		return CheckRecord{}
	}
	return rec
}

// Notice is the line a session start shows when the last check found a newer
// release; "" when there is nothing to say — never checked, a check too old to
// repeat, up to date, or the check failed.
//
// It compares against the version passed in rather than editing the record, so
// installing a release retires its notice at once.
func Notice(version string) string {
	return noticeFor(ReadCheck(), version, time.Now())
}

func noticeFor(rec CheckRecord, version string, now time.Time) string {
	if rec.Version == "" || rec.CheckedAt.IsZero() {
		return ""
	}
	if age := now.Sub(rec.CheckedAt); age < 0 || age > noticeMaxAge {
		return ""
	}
	if strings.TrimSpace(version) == "" || Compare(version, rec.Version) >= 0 {
		return ""
	}
	ch := rec.Channel
	if ch == "" {
		ch = ChannelStable
	}
	return fmt.Sprintf("· xdev %s is available (you have %s, channel %s) — run `xdev update`",
		rec.Version, displayVersion(version), ch)
}

// MaybeCheck starts a background check when one is due: the last record is older
// than checkEvery (or absent), no scheduled job owns the checking, and this is a
// run a human is looking at. Called from the interactive surfaces only — an
// editor that launches xdev over rpc/acp, or a CI job piping `xdev print` into a
// script, has not asked to poll GitHub every time it starts.
//
// The spawn is detached and writes the record itself: this call never waits, and
// a failure is a shrug, because a version notice is not why anyone launched xdev.
func MaybeCheck(version string) {
	if !CheckEnabled() || !Valid(version) || JobInstalled() {
		return // and a schedule already owns the answer
	}
	if rec := ReadCheck(); !rec.CheckedAt.IsZero() && time.Since(rec.CheckedAt) < checkEvery {
		return // this window has already been paid for
	}
	_ = spawnCheck(version)
}

// CheckEnabled reports whether background checks may run at all: the kill switch
// is unset and this process is attached to a terminal — stdout is where the
// notice would be read.
//
// `xdev update --check` writes the record on its own terms and is never gated
// here: an operator who typed the command, or who installed a job, has asked.
func CheckEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("XDEV_UPDATE_CHECK"))) {
	case "off", "0", "false", "no", "none", "disabled":
		return false
	case "1", "true", "yes", "on", "always":
		return true
	}
	// Undecided: only a run with a console behind it may poll on its own. A
	// terminal on *either* stream counts — `xdev print "…" | tee log` has a
	// human at the keyboard, and only a pipe on both (CI, a script) is silent.
	return term.IsTerminal(int(os.Stdin.Fd())) || term.IsTerminal(int(os.Stdout.Fd()))
}

// spawnCheck runs `<this binary> update --check` in its own session. A variable
// so the check test counts spawns and asserts the throttle without reaching for
// the network.
var spawnCheck = func(version string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running binary: %w", err)
	}
	args := []string{"update", "--check", "--timeout", fmt.Sprint(int(checkSpawnTimeout.Seconds())) + "s"}
	if ch := strings.TrimSpace(os.Getenv("XDEV_CHANNEL")); ch != "" {
		args = append(args, "--channel", ch)
	}
	return detachedCommand(exe, args).Start()
}

// ReportCheck prints the recorded check and who owns the checking — the body of
// `xdev update job status`, which is what a user runs to learn why they keep (or
// stop) seeing the notice.
func ReportCheck(out io.Writer) {
	rec := ReadCheck()
	if rec.CheckedAt.IsZero() {
		fmt.Fprintf(out, "no update check has run yet\n  record: %s\n", CheckPath())
	} else {
		fmt.Fprintf(out, "last check:  %s (%s)\n", rec.CheckedAt.Format(time.RFC3339), humanAge(time.Since(rec.CheckedAt)))
		fmt.Fprintf(out, "channel:     %s\n", orUnknown(rec.Channel))
		fmt.Fprintf(out, "checked by:  %s\n", orUnknown(rec.Running))
		fmt.Fprintf(out, "latest:      %s\n", orUnknown(rec.Version))
		if rec.Error != "" {
			fmt.Fprintf(out, "failed:      %s\n", rec.Error)
		}
		fmt.Fprintf(out, "record:      %s\n", CheckPath())
	}
	if p, ok := JobSchedulePath(); ok {
		fmt.Fprintf(out, "job:         installed — %s (%s)\n", jobName(), p)
	} else {
		fmt.Fprintf(out, "job:         none — xdev checks on the first terminal launch of each %dh window\n", int(checkEvery.Hours()))
	}
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unknown)"
	}
	return s
}

// humanAge keeps a status line readable; the exact timestamp prints beside it,
// so precision here buys nothing. The phrase carries its own " ago" — the
// sub-minute case is the exception, since "just now ago" is not English.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh ago", d.Hours())
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
