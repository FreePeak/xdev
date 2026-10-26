package browser

// Auto-connect: the user's own Chrome, without an extension or a restart
// (Chrome 144+, the same path chrome-devtools-mcp calls --autoConnect).
//
// Chrome can opt its *default* profile into remote debugging from
// chrome://inspect/#remote-debugging. When that switch is on, Chrome starts
// its DevTools HTTP server on an ephemeral loopback port and writes the port
// plus the browser target path into the profile directory:
//
//	<profile>/DevToolsActivePort
//	  9222
//	  /devtools/browser/<guid>
//
// The handler that writes that file is the same one that serves /json/list,
// so once the port is known the existing CDP client needs nothing new: the
// discovery base is http://127.0.0.1:<port>. Chrome still asks the person to
// allow the connection in a browser dialog; that prompt is the consent
// boundary, not a flag xdev sets.
//
// Two failure modes are handled here because a stale file is the normal case:
// Chrome leaves DevToolsActivePort behind when it crashes, so a port is only
// used after /json/version answers as Chrome. Every profile candidate that
// fails to answer falls through to the next, and then to the extension bridge
// or Chrome's default port.
//
// This file is deliberately one-way: it never launches Chrome, never edits
// the profile, and never touches Local State. It only reads.
//
// ponytail: a loopback probe runs per resolution (there is no cache). When the
// port is up that is one HTTP round trip; when it is down, connection-refused
// is immediate. A cache would need invalidation on Chrome restart, which is
// exactly when a stale answer hurts most.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// envProfileDir names a Chrome profile directory to examine instead of
	// the per-OS defaults. It is also how the CLI's --profile flag reaches
	// discovery, and how tests point at a fixture profile.
	envProfileDir = "XDEV_CHROME_PROFILE_DIR"
	// devToolsPortFile is Chrome's well-known discovery file.
	devToolsPortFile = "DevToolsActivePort"
	// localStateFile holds the chrome://inspect switch, outside the profile
	// proper (so it is still readable while Chrome runs).
	localStateFile = "Local State"
	// InspectURL is where the person enables remote debugging. Exported so
	// CLI guidance and tool errors name exactly one URL.
	InspectURL = "chrome://inspect/#remote-debugging"

	// autoConnectProbeTimeout bounds the liveness check, not a CDP call: a
	// local Chrome answers in microseconds, so a second is already generous
	// and a wedged port must not stall the whole tool.
	autoConnectProbeTimeout = 750 * time.Millisecond
	// maxVersionBytes caps /json/version; it is a tiny JSON object.
	maxVersionBytes = 64 << 10
)

// AutoConnectInfo is the state of the auto-connect path for one profile. It
// is what `xdev chrome status|connect` prints and what the tool's error
// messages branch on: "Chrome is off" and "Chrome is on but the port is
// stale" need different advice.
type AutoConnectInfo struct {
	// Root is the profile directory examined.
	Root string
	// PortFile is <Root>/DevToolsActivePort.
	PortFile string
	// ProfileFound reports that Root exists (it may still carry no port).
	ProfileFound bool
	// UserEnabled is Chrome's chrome://inspect switch (nil: no Local State,
	// or a Chrome too old to record it).
	UserEnabled *bool
	// Port and BrowserPath are the parsed DevToolsActivePort values; Port is
	// zero when the file is missing or malformed.
	Port        int
	BrowserPath string
	// Endpoint is the HTTP discovery base for Port.
	Endpoint string
	// Reachable reports that /json/version answered as a Chrome browser.
	Reachable bool
	// Err explains why the profile is not reachable (missing file, stale
	// port, permission denied, not Chrome).
	Err string
}

// Ready reports that Chrome is running with remote debugging on and the
// endpoint answered just now.
func (i AutoConnectInfo) Ready() bool { return i.Reachable }

// Enabled reports Chrome's own switch, as far as it can be read.
func (i AutoConnectInfo) Enabled() bool { return i.UserEnabled != nil && *i.UserEnabled }

// Stale reports a DevToolsActivePort that exists but does not answer: Chrome
// crashed or quit without cleaning up, so there is a port to report and no
// browser behind it.
func (i AutoConnectInfo) Stale() bool { return i.Port > 0 && !i.Reachable }

// chromeProfileRoots lists the profile directories to examine, best first.
// An explicit directory (config key or env) is the only candidate: a typo
// must be reported, not silently replaced by the default profile.
func chromeProfileRoots(explicit string) []string {
	if dir := strings.TrimSpace(explicit); dir != "" {
		if abs, err := expandProfileDir(dir); err == nil {
			return []string{abs}
		}
		return []string{dir}
	}
	if dir := strings.TrimSpace(os.Getenv(envProfileDir)); dir != "" {
		if abs, err := expandProfileDir(dir); err == nil {
			return []string{abs}
		}
		return []string{dir}
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join(home, "Library", "Application Support")
		return []string{
			filepath.Join(base, "Google", "Chrome"),
			filepath.Join(base, "Chromium"),
			filepath.Join(base, "BraveSoftware", "Brave-Browser"),
			filepath.Join(base, "Microsoft Edge"),
		}
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return nil
		}
		return []string{
			filepath.Join(base, "Google", "Chrome", "User Data"),
			filepath.Join(base, "Chromium", "User Data"),
			filepath.Join(base, "BraveSoftware", "Brave-Browser", "User Data"),
			filepath.Join(base, "Microsoft", "Edge", "User Data"),
		}
	default:
		base := filepath.Join(home, ".config")
		return []string{
			filepath.Join(base, "google-chrome"),
			filepath.Join(base, "chromium"),
			filepath.Join(base, "BraveSoftware", "Brave-Browser"),
			filepath.Join(base, "microsoft-edge"),
		}
	}
}

// expandProfileDir resolves ~ and makes the path absolute so the value can
// be written to config and compared with what Chrome sees.
func expandProfileDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", fmt.Errorf("empty profile directory")
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	return filepath.Abs(dir)
}

// ReadDevToolsActivePort parses <root>/DevToolsActivePort: the port on the
// first line, the browser target path on the second. Both are required — the
// path is what Chrome's own clients dial, and a file without it is a
// truncated write, not something to guess at.
func ReadDevToolsActivePort(root string) (port int, browserPath string, err error) {
	path := filepath.Join(root, devToolsPortFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, "", err
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		return 0, "", fmt.Errorf("%s: want a port and a browser path, got %q", path, strings.TrimSpace(string(raw)))
	}
	port, err = strconv.Atoi(lines[0])
	if err != nil || port <= 0 || port > 65535 {
		return 0, "", fmt.Errorf("%s: invalid port %q", path, lines[0])
	}
	browserPath = lines[1]
	if !strings.HasPrefix(browserPath, "/") {
		return 0, "", fmt.Errorf("%s: invalid browser path %q", path, browserPath)
	}
	return port, browserPath, nil
}

// userRemoteDebugging reads Chrome's chrome://inspect switch out of Local
// State. A missing or unreadable file reports nil ("unknown"), which is not
// the same as off: macOS TCC can deny the read even when the switch is on.
func userRemoteDebugging(root string) *bool {
	raw, err := os.ReadFile(filepath.Join(root, localStateFile))
	if err != nil {
		return nil
	}
	var doc struct {
		DevTools struct {
			RemoteDebugging struct {
				UserEnabled *bool `json:"user-enabled"`
			} `json:"remote_debugging"`
		} `json:"devtools"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc.DevTools.RemoteDebugging.UserEnabled
}

// probeDevTools checks that endpoint is a Chrome DevTools HTTP server, not
// merely an open port. /json/version names the browser and carries the
// browser websocket URL, so a 200 with that field is the proof.
func probeDevTools(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/version", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVersionBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s/json/version: %s", endpoint, resp.Status)
	}
	var v struct {
		Browser              string `json:"Browser"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("%s/json/version is not CDP: %w", endpoint, err)
	}
	if v.WebSocketDebuggerURL == "" {
		return fmt.Errorf("%s/json/version has no webSocketDebuggerUrl — not a Chrome DevTools endpoint", endpoint)
	}
	return nil
}

// ProbeAutoConnectAt inspects one profile directory and reports what it
// found, including why it is not usable.
func ProbeAutoConnectAt(root string) AutoConnectInfo {
	info := AutoConnectInfo{Root: root, PortFile: filepath.Join(root, devToolsPortFile)}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		info.Err = fmt.Sprintf("profile directory not found: %s", root)
		return info
	}
	info.ProfileFound = true
	info.UserEnabled = userRemoteDebugging(root)
	port, browserPath, err := ReadDevToolsActivePort(root)
	if err != nil {
		info.Err = err.Error()
		return info
	}
	info.Port = port
	info.BrowserPath = browserPath
	info.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx, cancel := context.WithTimeout(context.Background(), autoConnectProbeTimeout)
	defer cancel()
	if err := probeDevTools(ctx, info.Endpoint); err != nil {
		info.Err = err.Error()
		return info
	}
	info.Reachable = true
	info.Err = ""
	return info
}

// ProbeAutoConnect examines the configured (or default) Chrome profile. The
// first reachable profile wins; otherwise the first profile that actually
// has a port file is reported, so a stale file is named instead of a generic
// "Chrome is not running".
func ProbeAutoConnect(profileDir string) AutoConnectInfo {
	roots := chromeProfileRoots(profileDir)
	var fallback AutoConnectInfo
	var missing []string
	for _, root := range roots {
		info := ProbeAutoConnectAt(root)
		if info.Reachable {
			return info
		}
		if !info.ProfileFound {
			missing = append(missing, root)
			continue
		}
		if fallback.Root == "" || (fallback.Port == 0 && info.Port > 0) {
			fallback = info
		}
	}
	if fallback.Root != "" {
		return fallback
	}
	if len(roots) > 0 {
		info := ProbeAutoConnectAt(roots[0])
		if info.Err == "" {
			info.Err = fmt.Sprintf("no Chrome profile found (looked in %s); set $%s or browser.chromeProfileDir to the profile directory",
				strings.Join(roots, ", "), envProfileDir)
		}
		return info
	}
	return AutoConnectInfo{
		Err: fmt.Sprintf("no Chrome profile found; set $%s or browser.chromeProfileDir to the profile directory", envProfileDir),
	}
}

// autoConnectEndpoint returns the discovery base of a reachable real Chrome,
// or false when none answers.
func autoConnectEndpoint(profileDir string) (string, bool) {
	info := ProbeAutoConnect(profileDir)
	return info.Endpoint, info.Ready()
}

// autoConnectErr explains an unreachable auto-connect endpoint in terms of
// the step the person can actually take. The cause is kept: a permission
// error (macOS TCC on the profile directory) reads differently from a stale
// port, and both differ from a switched-off Chrome.
func autoConnectErr(profileDir string, cause error) error {
	info := ProbeAutoConnect(profileDir)
	switch {
	case !info.ProfileFound:
		return fmt.Errorf("browser: auto-connect: %s (browser.autoConnect: false launches a browser xdev owns instead)", info.Err)
	case info.UserEnabled != nil && !*info.UserEnabled:
		return fmt.Errorf("browser: Chrome remote debugging is off — open %s, turn it on, allow the connection prompt and keep Chrome running; browser.autoConnect: false turns auto-connect off (cause: %v)", InspectURL, cause)
	case info.Stale():
		return fmt.Errorf("browser: auto-connect found a stale %s (%s: %s) — Chrome is closed or was restarted; keep Chrome running with remote debugging enabled at %s, then retry (browser.autoConnect: false turns auto-connect off)", info.PortFile, info.Root, info.Err, InspectURL)
	case info.Port > 0:
		return fmt.Errorf("browser: auto-connect could not use Chrome's endpoint %s (%s); allow xdev's connection prompt at %s, or check $%s (cause: %v)", info.Endpoint, info.Err, InspectURL, envProfileDir, cause)
	default:
		return fmt.Errorf("browser: auto-connect: Chrome is not reachable on the default profile — open %s in Chrome 144+, enable remote debugging, allow the prompt and keep Chrome running; set browser.chromeProfileDir or $%s for a non-default profile, or browser.autoConnect: false to turn this off (cause: %v)", InspectURL, envProfileDir, cause)
	}
}

// autoConnectDialErr names the consent step; a refused page-websocket right
// after a reachable endpoint is almost always the browser dialog, not a
// network fault.
func autoConnectDialErr(cause error) error {
	return fmt.Errorf("browser: Chrome accepted the discovery request but not the debugging connection — allow xdev in the prompt Chrome opened (re-open %s and retry): %w", InspectURL, cause)
}
