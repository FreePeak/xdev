package dist

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The job renderers are tested on every platform: the units are text, and the
// failure they prevent — a schedule that checks the wrong thing, or leaks a
// credential — is not specific to the OS that runs it.

func TestLaunchdPlist(t *testing.T) {
	got := launchdPlist("/tmp/xdev space/xdev", "v1.2.3")

	for _, want := range []string{
		"<string>update</string><string>--check</string>",
		"<string>--timeout</string><string>120s</string>",
		"<key>StartCalendarInterval</key>",
		"<key>Hour</key><integer>9</integer>",
		"<key>Hour</key><integer>21</integer>",
		"xdev v1.2.3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q:\n%s", want, got)
		}
	}
	// The path carries a space; escaping it is what keeps the plist loadable.
	if !strings.Contains(got, xmlEscape("/tmp/xdev space/xdev")) {
		t.Errorf("plist does not escape the binary path:\n%s", got)
	}
	// Twice a day is ONE array of two dicts. Two StartCalendarInterval keys
	// would be a duplicate key in a plist dict — invalid, and the silent
	// failure is that only one of the two runs.
	if n := strings.Count(got, "<key>StartCalendarInterval</key>"); n != 1 {
		t.Errorf("StartCalendarInterval appears %d times, want 1", n)
	}
	if n := strings.Count(got, "<key>Minute</key>"); n != 2 {
		t.Errorf("%d Minute keys, want 2 (one per run)", n)
	}
	// The job installs nothing.
	if strings.Contains(got, "<string>--force</string>") || strings.Count(got, "<string>update</string>") != 1 {
		t.Errorf("job should run exactly one update command:\n%s", got)
	}
}

func TestLaunchdPlistCarriesKnobsNotSecrets(t *testing.T) {
	t.Setenv("XDEV_UPDATE_API", "https://mirror.example/api")
	t.Setenv("XDEV_CHANNEL", "canary")
	t.Setenv("GITHUB_TOKEN", "ghp_supersecretvalue")
	got := launchdPlist("/usr/local/bin/xdev", "v1.0.0")

	if !strings.Contains(got, "<key>XDEV_UPDATE_API</key><string>https://mirror.example/api</string>") {
		t.Errorf("plist lost the mirror knob:\n%s", got)
	}
	if !strings.Contains(got, "<key>XDEV_CHANNEL</key><string>canary</string>") {
		t.Errorf("plist lost the channel knob:\n%s", got)
	}
	// A credential copied into a plist outlives the shell that had it, and the
	// update path retries anonymously anyway. This is the file most likely to
	// be synced or screenshotted, so the assertion is worth the line.
	if strings.Contains(got, "ghp_supersecretvalue") || strings.Contains(got, "GITHUB_TOKEN") {
		t.Errorf("plist leaked GITHUB_TOKEN:\n%s", got)
	}
}

func TestSystemdUnits(t *testing.T) {
	service, timer := systemdUnits("/tmp/xdev space/xdev", "v1.2.3")

	if !strings.Contains(service, `ExecStart="/tmp/xdev space/xdev" update --check --timeout 120s`) {
		t.Errorf("service ExecStart wrong:\n%s", service)
	}
	if !strings.Contains(service, "Type=oneshot") {
		t.Errorf("service is not oneshot:\n%s", service)
	}
	if !strings.Contains(timer, "OnCalendar=*-*-* 09:00:00") || !strings.Contains(timer, "OnCalendar=*-*-* 21:00:00") {
		t.Errorf("timer does not run twice a day:\n%s", timer)
	}
	// A laptop suspended at 09:00 must still check when it wakes; without this
	// the timer slides and the "twice a day" claim is false on real machines.
	if !strings.Contains(timer, "Persistent=true") {
		t.Errorf("timer is not catch-up:\n%s", timer)
	}
	if !strings.Contains(timer, "Unit="+jobServiceUnit()) {
		t.Errorf("timer does not name its service:\n%s", timer)
	}
	if strings.Contains(service, "GITHUB_TOKEN") {
		t.Errorf("unit leaked GITHUB_TOKEN:\n%s", service)
	}
}

func TestSystemdUnitsCarryKnobs(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", "/tmp/state dir")
	service, _ := systemdUnits("/usr/local/bin/xdev", "v1.0.0")
	if !strings.Contains(service, `Environment=XDEV_AGENT_DIR="/tmp/state dir"`) {
		t.Errorf("unit lost the state-dir knob (the job would write the record elsewhere):\n%s", service)
	}
}

func TestJobRemoveWithoutJob(t *testing.T) {
	checkDir(t)
	out, errw := &strings.Builder{}, &strings.Builder{}
	if code := jobRemove(out, errw); code != 0 {
		t.Fatalf("removing a job that was never installed exited %d (stderr %s)", code, errw.String())
	}
	if !strings.Contains(out.String(), "no xdev update-check job installed") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestJobMainVerbs(t *testing.T) {
	checkDir(t)
	// Unknown verb and no verb both print usage and fail, without touching the
	// scheduler: `xdev update job instal` is a typo a user will make.
	for _, args := range [][]string{{"instal"}, {}} {
		out, errw := &strings.Builder{}, &strings.Builder{}
		if code := jobMain(args, "v1.0.0", out, errw); code != 2 {
			t.Errorf("job %v exit = %d, want 2", args, code)
		}
		if !strings.Contains(errw.String(), "usage: xdev update job") {
			t.Errorf("job %v did not print usage: %s", args, errw.String())
		}
	}
	out, errw := &strings.Builder{}, &strings.Builder{}
	if code := jobMain([]string{"status"}, "v1.0.0", out, errw); code != 0 {
		t.Fatalf("job status exit = %d (stderr %s)", code, errw.String())
	}
	if !strings.Contains(out.String(), "no update check has run yet") {
		t.Errorf("job status output: %s", out.String())
	}
	if errw.Len() > 0 {
		t.Errorf("job status wrote stderr: %s", errw.String())
	}
}

// TestJobPathsArePerUser guards the one thing that would make `job remove`
// destructive: resolving the schedule path somewhere other than the current
// user's own directory.
func TestJobPathsArePerUser(t *testing.T) {
	dir := checkDir(t)
	switch runtime.GOOS {
	case "darwin":
		if p := launchdPlistPath(); !strings.HasPrefix(p, filepath.Join(dir, "Library", "LaunchAgents")) || !strings.HasSuffix(p, ".plist") {
			t.Errorf("launchd path %q is outside the sandboxed home", p)
		}
		if _, ok := JobSchedulePath(); ok {
			t.Errorf("a job looks installed with an empty home")
		}
	case "linux":
		want := filepath.Join(dir, "config", "systemd", "user", jobTimerUnit())
		if got := systemdUnitPath(jobTimerUnit()); got != want {
			t.Errorf("systemd timer path = %q, want %q", got, want)
		}
		if _, err := os.Stat(want); err == nil {
			t.Errorf("the test found a real timer at %s", want)
		}
	}
}

func TestCronHint(t *testing.T) {
	checkDir(t)
	out, errw := &strings.Builder{}, &strings.Builder{}
	cronHint("/usr/local/bin/xdev", out, errw, "no scheduler integration for plan9")
	got := out.String()
	if !strings.Contains(got, "0 9,21 * * * /usr/local/bin/xdev update --check --timeout 120s") {
		t.Errorf("crontab line wrong:\n%s", got)
	}
	if !strings.Contains(got, CheckPath()) {
		t.Errorf("hint does not name the record the line writes:\n%s", got)
	}
	if !strings.Contains(errw.String(), "will not rewrite your crontab") {
		t.Errorf("hint does not say why it stopped:\n%s", errw.String())
	}
}
