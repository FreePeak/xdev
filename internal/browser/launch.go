package browser

// Auto-launch, the answer to "browser: no Chrome DevTools endpoint at
// http://127.0.0.1:9222 … connection refused".
//
// Attach-first: when something already answers on the endpoint (a Chrome the
// user started with --remote-debugging-port) xdev attaches to it and this
// file is never reached. Only "connection refused" gets here — and the answer
// to that is a browser of xdev's own on a private profile, not a failure the
// model has to relay to a user who asked for a screenshot.
//
// The user's browser is still never touched: a launched browser runs on its
// own --user-data-dir and only ever listens on loopback.
//
// os/exec is aliased: the package already has an `exec` test helper.
import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// launchMaxWait is the ceiling on a cold auto-launch; launchPoll is how often
// its port is probed.
const (
	launchMaxWait = 15 * time.Second
	launchPoll    = 150 * time.Millisecond
)

// launchBudget bounds a cold auto-launch (starting the binary plus waiting
// for its port), clamped to the effective per-op limit.
func launchBudget(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d < launchMaxWait {
			return d
		}
	}
	return launchMaxWait
}

// envBinary names a browser binary the search below does not know.
const envBinary = "XDEV_BROWSER_BIN"

// launchCandidates are the Chrome-family binary names to try, in order;
// candidateDirs covers an install that is not on PATH.
var launchCandidates = []string{
	"google-chrome",
	"google-chrome-stable",
	"chromium",
	"chromium-browser",
	"chrome",
	"brave-browser",
}

// candidateDirs holds the per-OS install locations worth checking when the
// binary is not on PATH.
func candidateDirs() []string {
	switch runtime.GOOS {
	case "windows":
		var dirs []string
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
			if root == "" {
				continue
			}
			dirs = append(dirs,
				filepath.Join(root, "Google", "Chrome", "Application"),
				filepath.Join(root, "Chromium", "Application"),
			)
		}
		return dirs
	default:
		dirs := []string{"/usr/bin", "/usr/local/bin", "/snap/bin"}
		if runtime.GOOS == "darwin" {
			dirs = append(dirs,
				"/Applications/Google Chrome.app/Contents/MacOS",
				"/Applications/Chromium.app/Contents/MacOS",
				"/Applications/Brave Browser.app/Contents/MacOS",
			)
		}
		return dirs
	}
}

// findChrome resolves the browser binary to launch, or reports that none is
// installed. $XDEV_BROWSER_BIN wins, so a caller can name a build the search
// does not know.
func findChrome() (string, error) {
	if bin := strings.TrimSpace(os.Getenv(envBinary)); bin != "" {
		if p, err := osexec.LookPath(bin); err == nil {
			return p, nil
		}
		if st, err := os.Stat(bin); err == nil && !st.IsDir() {
			return bin, nil
		}
		return "", fmt.Errorf("$%s = %q is not an executable", envBinary, bin)
	}
	for _, name := range launchCandidates {
		if p, err := osexec.LookPath(name); err == nil {
			return p, nil
		}
	}
	// The macOS .app bundles carry a "Google Chrome" binary name, the PATH
	// search finds the CLI wrappers on other systems; both spellings are
	// probed in every candidate dir.
	win := runtime.GOOS == "windows"
	for _, dir := range candidateDirs() {
		for _, name := range append(append([]string{}, launchCandidates...), "Google Chrome", "Brave Browser", "Chromium") {
			if win {
				name += ".exe"
			}
			if p := filepath.Join(dir, name); isFile(p) {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("no Chrome/Chromium binary found; install one or set $%s to its path", envBinary)
}

// isFile reports a regular file at p (not a directory, not a dangling entry).
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// launchBrowser starts a throwaway browser on the endpoint's port and returns
// the process. A non-loopback endpoint is refused rather than silently
// launched locally: a browser there is one the user must start.
func launchBrowser(endpoint, profileDir string) (*os.Process, error) {
	port, ok := endpointPort(endpoint)
	if !ok {
		return nil, fmt.Errorf("xdev can only auto-launch a browser on a loopback endpoint; %s is not one — start Chrome yourself with --remote-debugging-port and point browser.cdpUrl at it", endpoint)
	}
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the browser profile dir: %w", err)
	}
	// A non-default --user-data-dir is required by Chrome 136+ for remote
	// debugging at all, and it is what keeps this profile out of the user's.
	cmd := osexec.Command(bin,
		"--remote-debugging-port="+strconv.Itoa(port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"about:blank",
	)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launching %s: %w", bin, err)
	}
	// The browser must outlive this call, so it is never waited on here; the
	// goroutine reaps it if it exits while xdev runs (otherwise the port
	// being taken by someone else kills it, and the zombie would linger).
	proc := cmd.Process
	go func() { _ = cmd.Wait() }()
	return proc, nil
}

// endpointPort extracts the port of a loopback endpoint. A non-loopback host
// or an unparsable endpoint reports false.
func endpointPort(endpoint string) (int, bool) {
	addr := strings.TrimSpace(endpoint)
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+len("://"):]
	}
	addr = strings.TrimRight(addr, "/")
	addr = strings.TrimSuffix(addr, "/json/list")
	host, port, ok := splitHostPort(addr)
	if !ok {
		return 0, false
	}
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0":
	default:
		return 0, false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return 0, false
	}
	return n, true
}

// splitHostPort is net.SplitHostPort with the brackets normalized, so the
// loopback check sees a plain host.
func splitHostPort(addr string) (host, port string, ok bool) {
	if strings.HasPrefix(addr, "[") { // [::1]:9222
		end := strings.Index(addr, "]")
		if end < 0 || end+1 >= len(addr) || addr[end+1] != ':' {
			return "", "", false
		}
		return addr[1:end], addr[end+2:], true
	}
	i := strings.LastIndex(addr, ":")
	if i < 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

// ensureLaunched starts a browser on the endpoint and waits for its discovery
// port to answer. A browser that never opens the port (someone else took it,
// or the binary ignored the flag) is killed, so a failed launch leaves
// nothing behind.
func ensureLaunched(ctx context.Context, endpoint, profileDir string) error {
	proc, err := launchBrowser(endpoint, profileDir)
	if err != nil {
		return err
	}
	wait := launchBudget(ctx)
	deadline := time.Now().Add(wait)
	for {
		if _, err := ListTargets(ctx, endpoint, true); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			_ = proc.Kill()
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			_ = proc.Kill()
			return fmt.Errorf("launched a browser for %s but its DevTools port did not answer within %s", endpoint, wait)
		}
		select {
		case <-ctx.Done():
			_ = proc.Kill()
			return ctx.Err()
		case <-time.After(launchPoll):
		}
	}
}
