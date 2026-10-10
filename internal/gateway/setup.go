package gateway

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Setup is the installer half of `xdev gateway`.
//
// Two artefacts, both outside the repository:
//
//   - the bot token, in <data dir>/gateway.token at 0600 — never config.yml,
//     never the repo (the same split internal/serve uses for its tokens);
//   - a supervisor unit, a launchd agent on macOS / a systemd user unit on
//     Linux, so the bridge survives a logout and restarts on crash.
//
// Everything the unit needs to find — the binary, the data dir, the profile —
// is written into the unit, because launchd and systemd user units see
// neither an interactive shell's rc files nor its PATH.

// ServiceName is the unit/systemd label.
func ServiceName() string { return "ai.xdev.gateway" }

// UnitPath is where the supervisor unit lives on this host.
func UnitPath() string {
	if runtime.GOOS == "linux" {
		return filepath.Join(systemdUserDir(), ServiceName()+".service")
	}
	return filepath.Join(homeDir(), "Library", "LaunchAgents", ServiceName()+".plist")
}

// StatusPath is the daemon's own liveness record, written once at startup
// (pid, started) so `xdev gateway status` and `xdev ps` can answer without
// asking launchd.
func StatusPath(dataDir string) string { return filepath.Join(dataDir, "gateway", "status.json") }

// LogPath is where a supervised run's stdout/stderr land.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "gateway", "gateway.log") }

// InstallOptions configures WriteUnit.
type InstallOptions struct {
	Exe     string
	DataDir string
	Args    []string
	Version string
}

// WriteUnit renders and installs the supervisor unit, then loads it.
func WriteUnit(opts InstallOptions) (string, error) {
	if runtime.GOOS == "windows" {
		return "", fmt.Errorf("gateway: no service supervisor on windows yet — run `xdev gateway` in a terminal")
	}
	path := UnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(LogPath(opts.DataDir)), 0o700); err != nil {
		return "", err
	}
	unit := RenderUnit(opts)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return "", err
	}
	if err := loadUnit(path); err != nil {
		return path, fmt.Errorf("wrote %s but the supervisor refused it: %w", path, err)
	}
	return path, nil
}

// RemoveUnit unloads and deletes the unit. Idempotent: an absent unit is not
// an error, so a teardown need not know whether one was ever installed.
func RemoveUnit() (string, error) {
	path := UnitPath()
	if _, err := os.Stat(path); err != nil {
		return path, nil
	}
	unloadUnit(path)
	if err := os.Remove(path); err != nil {
		return path, err
	}
	return path, nil
}

// UnitLoaded reports whether the supervisor currently has the unit loaded.
func UnitLoaded() bool {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+ServiceName()).Run() == nil
	case "linux":
		return exec.Command("systemctl", "--user", "is-active", "--quiet", ServiceName()+".service").Run() == nil
	}
	return false
}

func loadUnit(path string) error {
	switch runtime.GOOS {
	case "darwin":
		uid := "gui/" + strconv.Itoa(os.Getuid())
		// A stale load of the same label makes bootstrap fail, so boot it out
		// first; "was never loaded" is the common case and is not news.
		_ = exec.Command("launchctl", "bootout", uid, path).Run()
		out, err := exec.Command("launchctl", "bootstrap", uid, path).Output()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "linux":
		if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
			return err
		}
		out, err := exec.Command("systemctl", "--user", "enable", "--now", ServiceName()+".service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return fmt.Errorf("no supervisor integration for %s", runtime.GOOS)
}

func unloadUnit(path string) {
	switch runtime.GOOS {
	case "darwin":
		uid := "gui/" + strconv.Itoa(os.Getuid())
		_ = exec.Command("launchctl", "bootout", uid, path).Run()
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", ServiceName()+".service").Run()
	}
}

// StopUnit asks the supervisor to stop the running daemon.
func StopUnit() error {
	switch runtime.GOOS {
	case "darwin":
		uid := "gui/" + strconv.Itoa(os.Getuid())
		out, err := exec.Command("launchctl", "bootout", uid, UnitPath()).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "linux":
		out, err := exec.Command("systemctl", "--user", "stop", ServiceName()+".service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return fmt.Errorf("no supervisor integration for %s", runtime.GOOS)
}

// StartUnit asks the supervisor to start an installed daemon.
func StartUnit() error {
	switch runtime.GOOS {
	case "darwin":
		uid := "gui/" + strconv.Itoa(os.Getuid())
		out, err := exec.Command("launchctl", "bootstrap", uid, UnitPath()).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "linux":
		out, err := exec.Command("systemctl", "--user", "start", ServiceName()+".service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return fmt.Errorf("no supervisor integration for %s", runtime.GOOS)
}

// RenderUnit is WriteUnit's pure half (the tests pin the rendered text).
func RenderUnit(opts InstallOptions) string {
	switch runtime.GOOS {
	case "linux":
		return renderSystemdUnit(opts)
	default:
		return renderLaunchdPlist(opts)
	}
}

// renderLaunchdPlist writes a KeepAlive agent: launchd restarts the daemon if
// it exits, which is the whole point of running it in the background.
func renderLaunchdPlist(opts InstallOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- written by "xdev gateway setup" (xdev %s); remove with "xdev gateway uninstall" -->
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array>
`, displayVersion(opts.Version), ServiceName())
	fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(opts.Exe))
	fmt.Fprint(&b, "    <string>gateway</string>\n    <string>run</string>\n")
	for _, a := range opts.Args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	fmt.Fprint(&b, "  </array>\n")
	env := unitEnv(opts.DataDir)
	if env != "" {
		fmt.Fprintf(&b, "  <key>EnvironmentVariables</key>\n  <dict>\n%s  </dict>\n", env)
	}
	// KeepAlive restarts on any exit; RunAtLoad starts it at login. A Throttle
	// Interval keeps a crash loop from spawning a process per second.
	fmt.Fprint(&b, "  <key>RunAtLoad</key><true/>\n  <key>KeepAlive</key><true/>\n  <key>ThrottleInterval</key><integer>10</integer>\n")
	fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n", xmlEscape(LogPath(opts.DataDir)))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", xmlEscape(LogPath(opts.DataDir)))
	fmt.Fprint(&b, "</dict>\n</plist>\n")
	return b.String()
}

// renderSystemdUnit writes a user service that restarts on failure.
func renderSystemdUnit(opts InstallOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# written by "xdev gateway setup" (xdev %s); remove with "xdev gateway uninstall"
[Unit]
Description=xdev gateway (Telegram bridge)
After=network-online.target

[Service]
Type=simple
ExecStart=%s gateway run`, displayVersion(opts.Version), quote(opts.Exe))
	for _, a := range opts.Args {
		b.WriteString(" " + quote(a))
	}
	b.WriteString("\nRestart=on-failure\nRestartSec=10\n")
	for _, kv := range unitEnvPairs(opts.DataDir) {
		fmt.Fprintf(&b, "Environment=%s=%s\n", kv[0], quote(kv[1]))
	}
	fmt.Fprintf(&b, "StandardOutput=append:%s\nStandardError=append:%s\n", LogPath(opts.DataDir), LogPath(opts.DataDir))
	fmt.Fprint(&b, "\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

// unitEnvPairs is the environment the daemon must inherit: where the data dir
// and profile live. Empty values are omitted so a unit never pins a key to "".
func unitEnvPairs(dataDir string) [][2]string {
	pairs := [][2]string{{"XDEV_AGENT_DIR", dataDir}}
	for _, key := range []string{"XDEV_PROFILE", "TELEGRAM_ALLOWED_CHATS", "TELEGRAM_ALLOW_ALL_CHATS", "HINDSIGHT_URL", "HINDSIGHT_API_URL", "HINDSIGHT_PROJECT"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			pairs = append(pairs, [2]string{key, v})
		}
	}
	return pairs
}

func unitEnv(dataDir string) string {
	var b strings.Builder
	for _, kv := range unitEnvPairs(dataDir) {
		fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", kv[0], xmlEscape(kv[1]))
	}
	return b.String()
}

// --- small shared helpers ---------------------------------------------------

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

func systemdUserDir() string {
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, "systemd", "user")
	}
	return filepath.Join(homeDir(), ".config", "systemd", "user")
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func displayVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "dev"
	}
	return v
}

// WriteToken stores the bot token at 0600 (the file the daemon reads when
// TELEGRAM_BOT_TOKEN is unset). It is written to the data dir, never the repo.
func WriteToken(dataDir, token string) error {
	token = strings.TrimSpace(token)
	if !ValidTokenShape(token) {
		return fmt.Errorf("gateway: that does not look like a BotFather token (want <digits>:<35+ chars>)")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	tmp := TokenPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, TokenPath(dataDir))
}
