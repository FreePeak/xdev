// Package main implements `xdev mcps` — the CLI equivalent of the
// in-session `/mcps` slash command: lists configured MCP servers
// and whether they are enabled (M6 #7).
package main

import (
	"fmt"
	"os"

	"github.com/FreePeak/xdev/internal/mcpclient"
)

func runMcps(args []string) int {
	path := mcpConfigPath()
	cfg, err := mcpclient.LoadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev: mcp config:", err)
		return 1
	}
	if len(cfg.Servers) == 0 {
		fmt.Println("no MCP servers configured")
		return 0
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	for _, n := range names {
		sc := cfg.Servers[n]
		state := "enabled"
		if sc.Disabled {
			state = "disabled"
		}
		if sc.Enabled != nil && !*sc.Enabled {
			state = "disabled"
		}
		transport := "stdio"
		if sc.URL != "" {
			transport = "http"
		}
		fmt.Printf("  %s  %s  %s\n", n, state, transport)
	}
	return 0
}
