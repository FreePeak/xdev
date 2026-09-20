package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/mcpclient"
)


// errMcpLoad is a sentinel for the config-loader error path.
var errMcpLoad = errors.New("mcp load boom")

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
				"browser":        {URL: "http://localhost:3000"},
				"filesystem":     {},
				"disabled-server": {Disabled: true},
			},
		}, nil
	}

	f := &fakeAPI{}
	if !dispatch(f, "/mcps") {
		t.Fatal("/mcps not consumed")
	}
	if len(f.blocks) != 1 {
		t.Fatalf("blocks = %q", f.blocks)
	}
	text := f.blocks[0]
	for _, want := range []string{"MCP servers:", "browser", "filesystem", "disabled-server", "http", "stdio"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if len(f.sent) != 0 {
		t.Fatalf("/mcps must never reach the model: %q", f.sent)
	}
}
