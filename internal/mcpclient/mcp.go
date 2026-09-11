// Package mcpclient connects xdev to Model Context Protocol servers
// (M6 #7). MCP is deliberately optional and off unless configured: it is
// a context-poisoning vector, and CLI tools beat MCP when an equivalent
// exists (PRD §2). Servers are declared in <dataDir>/mcp.yml, per-server
// enabled/disabled, with tool filtering for servers whose tools duplicate
// built-ins.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/tool"
)

// ServerConfig is one entry of mcp.yml.
type ServerConfig struct {
	// Command + Args launch a stdio server (mutually exclusive with URL).
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	// URL points at a streamable-HTTP server.
	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	// Disabled keeps the entry but skips connecting.
	Disabled bool `yaml:"disabled,omitempty"`
	// ExcludeTools drops the named tools after listing (filter noisy or
	// duplicate servers, e.g. browser automation when a built-in exists).
	ExcludeTools []string `yaml:"excludeTools,omitempty"`
	// IncludeTools, when non-empty, keeps only these tools.
	IncludeTools []string `yaml:"includeTools,omitempty"`
	// InitTimeoutSec bounds connect+list per server (default 5: a server
	// that starts but never answers `initialize` must not stall startup).
	InitTimeoutSec int `yaml:"initTimeoutSec,omitempty"`
}

// Config is the parsed mcp.yml.
type Config struct {
	Servers map[string]*ServerConfig `yaml:"servers"`
}

// LoadConfig reads mcp.yml from path; a missing file is not an error
// (MCP simply stays off).
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("mcp: parse %s: %w", path, err)
	}
	return &c, nil
}

// Manager owns the live sessions and their exported tools.
type Manager struct {
	mu       sync.Mutex
	closed   bool
	sessions map[string]*mcp.ClientSession
	tools    []*remoteTool
}

// NewManager returns an empty manager.
func NewManager() *Manager {
	return &Manager{sessions: map[string]*mcp.ClientSession{}}
}

// Connect brings up every enabled server, tolerating individual failures
// (a broken MCP server must never block the agent). Returns the number of
// servers connected and a per-server error list for reporting.
func (m *Manager) Connect(ctx context.Context, cfg *Config) (connected int, errs []string) {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sc := cfg.Servers[name]
		if sc == nil || sc.Disabled {
			continue
		}
		timeout := 15 * time.Second
		if sc.InitTimeoutSec > 0 {
			timeout = time.Duration(sc.InitTimeoutSec) * time.Second
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		sess, tools, err := connect(cctx, name, sc)
		cancel()
		if err != nil {
			logx.Errorf("mcp: server %q: %v", name, err)
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		m.mu.Lock()
		if m.closed { // Close raced the connect (async attach at shutdown)
			m.mu.Unlock()
			if err := sess.Close(); err != nil {
				logx.Debugf("mcp: close late session %q: %v", name, err)
			}
			continue
		}
		m.sessions[name] = sess
		m.tools = append(m.tools, tools...)
		m.mu.Unlock()
		connected++
		logx.Debugf("mcp: server %q connected (%d tools)", name, len(tools))
	}
	return connected, errs
}

// connect dials one server and lists its tools.
func connect(ctx context.Context, name string, sc *ServerConfig) (*mcp.ClientSession, []*remoteTool, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "xdev", Version: "1"}, nil)
	var transport mcp.Transport
	switch {
	case sc.Command != "":
		cmd := exec.Command(sc.Command, sc.Args...)
		cmd.Env = os.Environ()
		for k, v := range sc.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		transport = &mcp.CommandTransport{Command: cmd}
	case sc.URL != "":
		transport = &mcp.StreamableClientTransport{Endpoint: sc.URL}
	default:
		return nil, nil, fmt.Errorf("mcp: server needs command or url")
	}
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, err
	}

	exclude := map[string]bool{}
	for _, n := range sc.ExcludeTools {
		exclude[n] = true
	}
	include := map[string]bool{}
	for _, n := range sc.IncludeTools {
		include[n] = true
	}
	var out []*remoteTool
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			sess.Close()
			return nil, nil, err
		}
		if exclude[t.Name] || (len(include) > 0 && !include[t.Name]) {
			continue
		}
		out = append(out, &remoteTool{server: name, sess: sess, spec: t})
	}
	return sess, out, nil
}

// Tools returns the registered remote tools as xdev tools.
func (m *Manager) Tools() []tool.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]tool.Tool, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, t)
	}
	return out
}

// Close tears every session down (process transports are killed).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for name, s := range m.sessions {
		if err := s.Close(); err != nil {
			logx.Debugf("mcp: close %q: %v", name, err)
		}
	}
	m.sessions = map[string]*mcp.ClientSession{}
	m.tools = nil
}

// remoteTool adapts one MCP tool to xdev's tool.Tool. Names are
// namespaced ("<server>_<tool>") so remote and built-in tools can never
// collide in one registry.
type remoteTool struct {
	server string
	sess   *mcp.ClientSession
	spec   *mcp.Tool
}

func (t *remoteTool) Name() string { return t.server + "_" + t.spec.Name }

func (t *remoteTool) Description() string {
	d := t.spec.Description
	if d == "" {
		d = "MCP tool " + t.spec.Name + " from server " + t.server
	}
	return d
}

func (t *remoteTool) Parameters() json.RawMessage {
	if t.spec.InputSchema == nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	raw, err := json.Marshal(t.spec.InputSchema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

// mcpToolTimeout bounds one remote tool call: a hung MCP server must not
// wedge the agent's tool worker forever.
const mcpToolTimeout = 120 * time.Second

func (t *remoteTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var arguments any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return tool.Result{Text: "mcp: malformed arguments: " + err.Error(), IsError: true}, nil
		}
	}
	cctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
	defer cancel()
	res, err := t.sess.CallTool(cctx, &mcp.CallToolParams{Name: t.spec.Name, Arguments: arguments})
	if err != nil {
		return tool.Result{Text: fmt.Sprintf("mcp %s/%s: %v", t.server, t.spec.Name, err), IsError: true}, nil
	}
	return tool.Result{
		Text:    renderContent(res),
		Details: map[string]any{"server": t.server, "tool": t.spec.Name},
		IsError: res.IsError,
	}, nil
}

// mcpOutputHeadTail bounds one remote tool result (PRD §3.6: bounded
// everything). MCP servers are third-party and can return megabytes; the
// same head+tail discipline the bash sink uses applies here.
const mcpOutputHeadTail = 32 << 10

// renderContent flattens MCP content blocks into model-readable text,
// windowed through the shared OutputSink so nothing unbounded ever
// reaches the prompt, the session JSONL, or memory.
func renderContent(res *mcp.CallToolResult) string {
	sink := tool.NewOutputSink(mcpOutputHeadTail, mcpOutputHeadTail)
	var b strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text)
			if !strings.HasSuffix(v.Text, "\n") {
				b.WriteString("\n")
			}
		case *mcp.ImageContent:
			// Images are opaque to the text channel; note them instead of
			// silently dropping the result.
			fmt.Fprintf(&b, "[image %s, %d bytes]\n", v.MIMEType, len(v.Data))
		default:
			if raw, err := json.Marshal(c); err == nil {
				b.Write(raw)
				b.WriteString("\n")
			}
		}
	}
	if res.StructuredContent != nil && b.Len() == 0 {
		raw, _ := json.Marshal(res.StructuredContent)
		b.Write(raw)
	}
	_, _ = sink.Write([]byte(b.String()))
	out, truncated := sink.Result()
	out = strings.TrimRight(out, "\n")
	if out == "" {
		out = "(no output)"
	}
	if truncated {
		out += "\n[mcp output truncated at " + strconv.FormatUint(sink.Total(), 10) + " bytes total]"
	}
	return out
}

// Register adds every remote tool to a registry under its namespaced name.
func Register(reg *tool.Registry, tools []tool.Tool) {
	for _, t := range tools {
		reg.Register(t)
	}
}
