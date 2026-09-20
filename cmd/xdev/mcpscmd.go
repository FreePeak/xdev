// Package main implements `xdev mcps` — the CLI equivalent of the
// in-session `/mcps` slash command: lists configured MCP servers
// and whether they are enabled, reachable, and auto-startable (M6 #7).
package main

import (
	"context"
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
	ctx := context.Background()
	statuses, err := mcpclient.ServerHealthProbe(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev: mcp health:", err)
	}
	for _, st := range statuses {
		auto := ""
		sc := cfg.Servers[st.Name]
		if sc != nil && sc.AutoStart != nil {
			auto = " (auto-start)"
		}
		fmt.Printf("  %s  %-11s  %s%s\n", st.Name, st.State, st.Transport, auto)
	}
	return 0
}
