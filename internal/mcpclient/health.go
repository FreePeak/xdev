// Health probing for auto-started MCP servers. xdev launches a server on
// demand when it is unreachable and the config carries an AutoStart
// recipe; this file owns the probe and the launch.
package mcpclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"
)

// probeHTTP GETs the health endpoint and reports whether the server
// answered 200. A connection failure is a clean "down" — the caller
// decides whether to start it.
func probeHTTP(rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false, fmt.Errorf("parse health url: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return false, fmt.Errorf("request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, nil // down, not an error
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// probeStdio checks whether the configured command exists on PATH.
func probeStdio(command string) (bool, error) {
	if command == "" {
		return false, nil
	}
	_, err := exec.LookPath(command)
	return err == nil, nil
}

// IsHealthy reports whether the server is reachable. For HTTP servers it
// probes HealthEndpoint(); for stdio servers it checks the command exists.
// An empty config (nil or no URL/command) is treated as unhealthy.
func (sc *ServerConfig) IsHealthy() (bool, error) {
	if sc == nil {
		return false, nil
	}
	if sc.URL != "" {
		return probeHTTP(sc.HealthEndpoint())
	}
	return probeStdio(sc.Command)
}

// StartAuto launches the server described by sc.AutoStart and waits up to
// HealthTimeoutSec for it to answer the health probe. The process is
// detached (its own session) so it outlives the xdev process that spawned
// it, matching internal/dist/proc_unix.go. Returns the launched *exec.Cmd
// so the caller can record it for cleanup; the caller is responsible for
// killing it on shutdown.
func (sc *ServerConfig) StartAuto() (*exec.Cmd, error) {
	if sc == nil || sc.AutoStart == nil {
		return nil, fmt.Errorf("autoStart: not configured")
	}
	if sc.AutoStart.Command == "" {
		return nil, fmt.Errorf("autoStart: command is empty")
	}
	cmd := exec.Command(sc.AutoStart.Command, sc.AutoStart.Args...)
	cmd.Dir = sc.AutoStart.Cwd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = setProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("autoStart: %w", err)
	}
	timeout := sc.AutoStart.HealthTimeoutSec
	if timeout <= 0 {
		timeout = 30
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for time.Now().Before(deadline) {
		healthy, err := sc.IsHealthy()
		if err != nil {
			return cmd, fmt.Errorf("autoStart: health probe: %w", err)
		}
		if healthy {
			return cmd, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return cmd, fmt.Errorf("autoStart: server did not answer health probe within %ds", timeout)
}
