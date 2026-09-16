package dist

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// The twice-a-day version-check job: `xdev update job install|remove|status`
// owns one scheduled task that runs
//
//	xdev update --check
//
// at 09:00 and 21:00 local time. The task needs no console, no credentials and
// no parent: it resolves the channel's newest release over HTTPS, installs
// nothing, and writes <state-dir>/update-check.json — the file session starts
// read (check.go). With a job installed they never reach for the network.
//
// Two schedulers are implemented because they are the two a developer's machine
// already has: launchd (macOS) and a systemd user timer (Linux). Anywhere else
// `job install` prints the crontab line and leaves the editing to the user —
// rewriting someone's crontab from a CLI is not a trade worth making.

// checkHours are the two local times a day the job runs. Fixed hours rather
// than a rolling 12h interval: "9 in the morning and 9 at night" is what a
// person means by twice a day, and a missed run is caught up (launchd fires the
// interval on the next login, systemd with Persistent=true) rather than sliding
// another 12 hours.
var checkHours = [2]int{9, 21}

// jobMain implements `xdev update job <verb>`.
func jobMain(args []string, version string, out, errw io.Writer) int {
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		fs := flag.NewFlagSet("update job", flag.ContinueOnError)
		fs.SetOutput(errw)
		if err := fs.Parse(args); err != nil {
			return 2
		}
		args = fs.Args()
	}
	if len(args) == 0 {
		fmt.Fprint(errw, jobUsage)
		return 2
	}
	verb, extra := args[0], args[1:]
	if len(extra) > 0 {
		fmt.Fprintf(errw, "xdev: unexpected argument %q\n\n%s\n", extra[0], jobUsage)
		return 2
	}
	switch verb {
	case "install":
		return jobInstall(version, out, errw)
	case "remove":
		return jobRemove(out, errw)
	case "status":
		ReportCheck(out)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(out, jobUsage)
		return 0
	default:
		fmt.Fprintf(errw, "xdev: unknown job verb %q\n\n%s\n", verb, jobUsage)
		return 2
	}
}

const jobUsage = `usage: xdev update job <install|remove|status>

install  schedule "xdev update --check" twice a day (09:00, 21:00) on the
         platform's own runner: a launchd agent (macOS) or a systemd user
         timer (Linux). The job only checks — it installs nothing.
remove   stop the job and delete the file xdev wrote.
status   report the last check, what it found, and whether a job owns it.

An xdev session start reads the record the job writes; with no job installed,
the first interactive launch of each 12h window performs the check itself.
XDEV_UPDATE_CHECK=off silences the notice either way.`

// jobBinary is the executable the job runs. A variable so the test can render a
// unit against a temp path instead of whatever binary ran the test.
var jobBinary = func() (string, error) { return ResolveTarget("") }

func jobInstall(version string, out, errw io.Writer) int {
	exe, err := jobBinary()
	if err != nil {
		fmt.Fprintln(errw, "xdev:", err)
		return 1
	}
	switch runtime.GOOS {
	case "darwin":
		path := launchdPlistPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(errw, "xdev:", err)
			return 1
		}
		if err := os.WriteFile(path, []byte(launchdPlist(exe, version)), 0o644); err != nil {
			fmt.Fprintln(errw, "xdev:", err)
			return 1
		}
		fmt.Fprintf(out, "wrote %s\n", path)
		uid := "gui/" + strconv.Itoa(os.Getuid())
		// A stale load of the same label would fail the bootstrap below, so
		// boot it out first; "never loaded" is the common case and is not news.
		runQuiet("launchctl", "bootout", uid, path)
		if err := run("launchctl", "bootstrap", uid, path); err != nil {
			fmt.Fprintf(errw, "xdev: launchctl bootstrap failed: %v\n  hint: launchctl load %s\n", err, path)
			return 1
		}
		jobInstalledMessage(exe, out)
		return 0
	case "linux":
		if !hasSystemctl() {
			cronHint(exe, out, errw, "no systemctl on this host")
			return 0
		}
		dir := systemdUnitDir()
		if strings.TrimSpace(dir) == "" {
			fmt.Fprintln(errw, "xdev: cannot locate a config directory for systemd user units")
			return 1
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Fprintln(errw, "xdev:", err)
			return 1
		}
		service, timer := systemdUnits(exe, version)
		units := []struct{ name, body string }{{jobServiceUnit(), service}, {jobTimerUnit(), timer}}
		for _, u := range units {
			path := filepath.Join(dir, u.name)
			if err := os.WriteFile(path, []byte(u.body), 0o644); err != nil {
				fmt.Fprintln(errw, "xdev:", err)
				return 1
			}
			fmt.Fprintf(out, "wrote %s\n", path)
		}
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			fmt.Fprintf(errw, "xdev: %v\n  hint: systemctl --user daemon-reload && systemctl --user enable --now %s\n", err, jobTimerUnit())
			return 1
		}
		if err := run("systemctl", "--user", "enable", "--now", jobTimerUnit()); err != nil {
			fmt.Fprintf(errw, "xdev: enabling %s failed: %v\n  hint: systemctl --user enable --now %s\n", jobTimerUnit(), err, jobTimerUnit())
			return 1
		}
		jobInstalledMessage(exe, out)
		return 0
	default:
		cronHint(exe, out, errw, "no scheduler integration for "+runtime.GOOS)
		return 0
	}
}

func jobInstalledMessage(exe string, out io.Writer) {
	fmt.Fprintf(out, "job installed: %s — %s update --check at %02d:00 and %02d:00\n",
		jobName(), exe, checkHours[0], checkHours[1])
	fmt.Fprintf(out, "  record: %s\n", CheckPath())
	fmt.Fprintf(out, "  stop it with: xdev update job remove\n")
}

// jobRemove is idempotent the way `rm` is: an absent job is not an error, so a
// teardown script need not know whether one was ever installed.
func jobRemove(out, errw io.Writer) int {
	switch runtime.GOOS {
	case "darwin":
		path := launchdPlistPath()
		if fileExists(path) {
			uid := "gui/" + strconv.Itoa(os.Getuid())
			runQuiet("launchctl", "bootout", uid, path)
			runQuiet("launchctl", "unload", path) // older launchctl spellings
			return removeJobFile(path, errw, out)
		}
	case "linux":
		timer := systemdUnitPath(jobTimerUnit())
		if fileExists(timer) {
			runQuiet("systemctl", "--user", "disable", "--now", jobTimerUnit())
			for _, unit := range []string{jobTimerUnit(), jobServiceUnit()} {
				if p := systemdUnitPath(unit); fileExists(p) {
					if err := os.Remove(p); err != nil {
						fmt.Fprintf(errw, "xdev: remove %s: %v\n", p, err)
					} else {
						fmt.Fprintf(out, "removed %s\n", p)
					}
				}
			}
			runQuiet("systemctl", "--user", "daemon-reload")
			return 0
		}
	}
	fmt.Fprintln(out, "no xdev update-check job installed")
	return 0
}

func removeJobFile(path string, errw, out io.Writer) int {
	if err := os.Remove(path); err != nil {
		fmt.Fprintln(errw, "xdev:", err)
		return 1
	}
	fmt.Fprintf(out, "removed %s\n", path)
	return 0
}

// cronHint is the honest answer where xdev has no integration: the exact line,
// and no attempt to edit the user's crontab for them.
func cronHint(exe string, out, errw io.Writer, why string) {
	fmt.Fprintf(errw, "xdev: %s — xdev will not rewrite your crontab for you.\n", why)
	fmt.Fprintf(out, "add this line yourself (`crontab -e`) for the same twice-daily check:\n")
	fmt.Fprintf(out, "\n0 %d,%d * * * %s update --check --timeout %ds\n\n",
		checkHours[0], checkHours[1], quote(exe), int(checkSpawnTimeout.Seconds()))
	fmt.Fprintf(out, "it writes %s, which is what xdev session starts read.\n", CheckPath())
}

func quote(s string) string {
	if strings.ContainsAny(s, " \t\"'\\") {
		return strconv.Quote(s)
	}
	return s
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasSystemctl() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// runQuiet is for the calls whose failure is expected (unloading a job that was
// never loaded) — the file check decides, not the exit status.
func runQuiet(name string, args ...string) { _ = exec.Command(name, args...).Run() }

// --- schedule identities ---------------------------------------------------

func jobName() string {
	if runtime.GOOS == "darwin" {
		return "com.freepeak.xdev.update-check"
	}
	return "xdev-update-check"
}

func jobServiceUnit() string { return jobName() + ".service" }
func jobTimerUnit() string   { return jobName() + ".timer" }

// JobSchedulePath returns the schedule file xdev owns on this platform and
// whether it is in place. A job xdev did not write is not detected: the question
// the answer serves is "did xdev install one", which is what gates the
// launch-time check.
func JobSchedulePath() (string, bool) {
	var path string
	switch runtime.GOOS {
	case "darwin":
		path = launchdPlistPath()
	case "linux":
		path = systemdUnitPath(jobTimerUnit())
	default:
		return "", false
	}
	return path, fileExists(path)
}

// JobInstalled reports whether a check job xdev installed is in place; while it
// is, session starts do not check on their own (see MaybeCheck).
func JobInstalled() bool {
	_, ok := JobSchedulePath()
	return ok
}

func launchdPlistPath() string {
	return filepath.Join(launchdAgentsDir(), jobName()+".plist")
}

func launchdAgentsDir() string { return filepath.Join(homeDir(), "Library", "LaunchAgents") }

// homeDir is the user's home: launchd scans $HOME/Library/LaunchAgents for this
// user and systemd --user reads $HOME/.config, whether or not the data dir was
// relocated by XDEV_AGENT_DIR or `config init-xdg`.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

func systemdUnitDir() string {
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, "systemd", "user")
	}
	h := homeDir()
	if h == "" {
		return ""
	}
	return filepath.Join(h, ".config", "systemd", "user")
}

func systemdUnitPath(unit string) string { return filepath.Join(systemdUnitDir(), unit) }

// --- the two unit renderers ------------------------------------------------

// launchdPlist renders the agent: one StartCalendarInterval array carrying the
// two runs a day. The knobs the updater reads travel with the job, because
// launchd sees no rc files; GITHUB_TOKEN deliberately does not — a copy of a
// credential in a plist outlives the shell that had it, and update.go already
// retries an auth failure anonymously.
func launchdPlist(exe, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- written by "xdev update job install" (xdev %s); remove with "xdev update job remove" -->
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array>
    <string>%s</string>
    <string>update</string><string>--check</string>
    <string>--timeout</string><string>%ds</string>
  </array>
`, displayVersion(version), jobName(), xmlEscape(exe), int(checkSpawnTimeout.Seconds()))
	if env := launchdEnv(); env != "" {
		fmt.Fprintf(&b, "  <key>EnvironmentVariables</key>\n  <dict>\n%s  </dict>\n", env)
	}
	// One StartCalendarInterval holding two dicts is launchd's spelling for
	// "twice a day"; repeating the key would be a plist with a duplicate.
	fmt.Fprint(&b, "  <key>StartCalendarInterval</key>\n  <array>\n")
	for _, h := range checkHours {
		fmt.Fprintf(&b, "    <dict><key>Hour</key><integer>%d</integer><key>Minute</key><integer>0</integer></dict>\n", h)
	}
	fmt.Fprint(&b, "  </array>\n")
	fmt.Fprint(&b, `  <key>StandardOutPath</key><string>/dev/null</string>
  <key>StandardErrorPath</key><string>/dev/null</string>
  <key>Nice</key><integer>10</integer>
</dict>
</plist>
`)
	return b.String()
}

func launchdEnv() string {
	var b strings.Builder
	for _, kv := range jobEnvPairs() {
		fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", kv[0], xmlEscape(kv[1]))
	}
	return b.String()
}

// jobEnvPairs carries the knobs the updater reads so a scheduled check follows
// the same mirror, repo and channel as an interactive one. Anything the operator
// did not set is omitted, so the job inherits the built-in default rather than a
// frozen copy of it.
func jobEnvPairs() [][2]string {
	var pairs [][2]string
	for _, key := range []string{"XDEV_UPDATE_REPO", "XDEV_UPDATE_API", "XDEV_CHANNEL", "XDEV_AGENT_DIR", "XDEV_PROFILE"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			pairs = append(pairs, [2]string{key, v})
		}
	}
	return pairs
}

// systemdUnits renders the service/timer pair: one oneshot service and the two
// OnCalendar lines that make it twice a day.
func systemdUnits(exe, version string) (service, timer string) {
	service = fmt.Sprintf(`# written by "xdev update job install" (xdev %s); remove with "xdev update job remove"
[Unit]
Description=xdev release check

[Service]
Type=oneshot
ExecStart=%s update --check --timeout %ds
Nice=10
`, displayVersion(version), quote(exe), int(checkSpawnTimeout.Seconds()))
	for _, kv := range jobEnvPairs() {
		service += fmt.Sprintf("Environment=%s=%s\n", kv[0], quote(kv[1]))
	}
	times := make([]string, len(checkHours))
	for i, h := range checkHours {
		times[i] = fmt.Sprintf("*-*-* %02d:00:00", h)
	}
	timer = fmt.Sprintf(`# written by "xdev update job install" (xdev %s)
[Unit]
Description=Check for a new xdev release twice a day

[Timer]
OnCalendar=%s
OnCalendar=%s
Persistent=true
Unit=%s

[Install]
WantedBy=timers.target
`, displayVersion(version), times[0], times[1], jobServiceUnit())
	return service, timer
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
