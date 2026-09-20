package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/mcpclient"
)

// errMcpLoad is a sentinel for the config-loader error path.
var errMcpLoad = errors.New("mcp load boom")

// testProbeSeam installs a probe seam that returns the given rows
// (and optional error) for every call until the test tears down.
func testProbeSeam(rows []mcpclient.ServerStatus, err error) {
	probeSeam = func(cfg *mcpclient.Config) ([]mcpclient.ServerStatus, error) {
		return rows, err
	}
}

// clearProbeSeam removes the probe seam so mcpsCommand calls
// mcpclient.ServerHealthProbe again.
func clearProbeSeam() {
	probeSeam = nil
}

// TestMcpsCommandNoConfig verifies /mcps with no configured
// servers renders a "no MCP servers configured" notice and is
// consumed (never sent as a prompt).
func TestMcpsCommandNoConfig(t *testing.T) {
	mcpsSeam = func() (*mcpclient.Config, error) {
		return &mcpclient.Config{Servers: map[string]*mcpclient.ServerConfig{}}, nil
	}

	f := &fakeAPI{}
	if !dispatch(f, "/mcps") {
		t.Fatal("/mcps not consumed")
	}
	if len(f.blocks) != 1 || !strings.Contains(f.blocks[0], "no MCP servers configured") {
		t.Fatalf("blocks = %q", f.blocks)
	}
	if len(f.sent) != 0 {
		t.Fatalf("/mcps must never reach the model: %q", f.sent)
	}
}

// TestMcpsCommandEmptyOnError verifies /mcps surfaces a config
// loader error rather than silently succeeding.
func TestMcpsCommandEmptyOnError(t *testing.T) {
	mcpsSeam = func() (*mcpclient.Config, error) {
		return nil, errMcpLoad
	}

	f := &fakeAPI{}
	if err := mcpsCommand(f, ""); err == nil {
		t.Fatal("/mcps with loader error must error")
	}
}

// TestMcpsCommandListsServers verifies /mcps renders each
// server's name, state, and transport type.
func TestMcpsCommandListsServers(t *testing.T) {
	mcpsSeam = func() (*mcpclient.Config, error) {
		return &mcpclient.Config{
			Servers: map[string]*mcpclient.ServerConfig{
				"browser":         {URL: "http://localhost:3000"},
				"filesystem":      {},
				"disabled-server": {Disabled: true},
			},
		}, nil
	}
	testProbeSeam([]mcpclient.ServerStatus{
		{Name: "browser", State: "online", Transport: "http"},
		{Name: "filesystem", State: "online", Transport: "stdio"},
		{Name: "disabled-server", State: "disabled", Transport: "stdio"},
	}, nil)
	defer clearProbeSeam()

	f := &fakeAPI{}
	if !dispatch(f, "/mcps") {
		t.Fatal("/mcps not consumed")
	}
	if len(f.blocks) != 1 {
		t.Fatalf("blocks = %q", f.blocks)
	}
	text := f.blocks[0]
	for _, want := range []string{"MCP servers:", "browser", "filesystem", "disabled-server", "http", "stdio", "online"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if len(f.sent) != 0 {
		t.Fatalf("/mcps must never reach the model: %q", f.sent)
	}
}

// TestMcpsCommandListsServerStates verifies /mcps renders each
// server with the correct enablement state and transport type —
// an unreachable remote server reads "unreachable", separate from
// enablement (config is separate from reachability).
func TestMcpsCommandListsServerStates(t *testing.T) {
	mcpsSeam = func() (*mcpclient.Config, error) {
		return &mcpclient.Config{
			Servers: map[string]*mcpclient.ServerConfig{
				"leankg":        {URL: "http://localhost:9699/mcp"},
				"atlassian":     {Command: "uvx", Args: []string{"mcp-atlassian"}},
				"broken-remote": {URL: "http://localhost:9999/nope", Enabled: boolPtr(false)},
			},
		}, nil
	}
	testProbeSeam([]mcpclient.ServerStatus{
		{Name: "leankg", State: "online", Transport: "http"},
		{Name: "atlassian", State: "online", Transport: "stdio"},
		{Name: "broken-remote", State: "disabled", Transport: "http"},
	}, nil)
	defer clearProbeSeam()

	f := &fakeAPI{}
	if !dispatch(f, "/mcps") {
		t.Fatal("/mcps not consumed")
	}
	if len(f.blocks) != 1 {
		t.Fatalf("blocks = %v", f.blocks)
	}
	text := f.blocks[0]
	for _, want := range []string{
		"MCP servers:", "leankg", "atlassian", "broken-remote",
		"disabled", "http", "stdio", "online",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if len(f.sent) != 0 {
		t.Fatalf("/mcps must never reach the model: %q", f.sent)
	}
}

// TestMcpsCommandShowsUnreachable verifies a server that is
// enabled in config but unreachable at probe time renders
// "unreachable" rather than "enabled".
func TestMcpsCommandShowsUnreachable(t *testing.T) {
	mcpsSeam = func() (*mcpclient.Config, error) {
		return &mcpclient.Config{
			Servers: map[string]*mcpclient.ServerConfig{
				"dead-server": {URL: "http://localhost:1"},
			},
		}, nil
	}
	testProbeSeam([]mcpclient.ServerStatus{
		{Name: "dead-server", State: "unreachable", Transport: "http"},
	}, nil)
	defer clearProbeSeam()

	f := &fakeAPI{}
	if !dispatch(f, "/mcps") {
		t.Fatal("/mcps not consumed")
	}
	text := f.blocks[0]
	if !strings.Contains(text, "unreachable") {
		t.Fatalf("expected unreachable in:\n%s", text)
	}
	if strings.Contains(text, "dead-server  enabled") {
		t.Fatalf("enabled should be overlaid by unreachable:\n%s", text)
	}
	if len(f.sent) != 0 {
		t.Fatalf("/mcps must never reach the model: %q", f.sent)
	}
}

// TestMcpsCommandProbeError verifies a probe error surfaces as
// a notice without dropping the server listing.
func TestMcpsCommandProbeError(t *testing.T) {
	mcpsSeam = func() (*mcpclient.Config, error) {
		return &mcpclient.Config{
			Servers: map[string]*mcpclient.ServerConfig{
				"leankg": {URL: "http://localhost:9699/mcp"},
			},
		}, nil
	}
	testProbeSeam(nil, errors.New("probe boom"))
	defer clearProbeSeam()

	f := &fakeAPI{}
	if !dispatch(f, "/mcps") {
		t.Fatal("/mcps not consumed")
	}
	// The probe error is its own block; the server listing follows.
	if len(f.blocks) < 2 {
		t.Fatalf("expected probe-error block + listing, got %v", f.blocks)
	}
	if !strings.Contains(f.blocks[0], "probe boom") {
		t.Fatalf("probe error not surfaced in first block:\n%s", f.blocks[0])
	}
	if !strings.Contains(f.blocks[1], "leankg") {
		t.Fatalf("server listing lost on probe error:\n%s", f.blocks[1])
	}
	if len(f.sent) != 0 {
		t.Fatalf("/mcps must never reach the model: %q", f.sent)
	}
}

func boolPtr(b bool) *bool { return &b }
