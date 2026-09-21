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
		sc := cfg.Servers[st.Name]
		auto := ""
		if sc != nil && sc.AutoStart != nil {
			auto = " (auto-start)"
		}
		fmt.Printf("  %s  %-11s  %s%s\n", st.Name, st.State, st.Transport, auto)
		if st.State == "unreachable" || st.State == "error" {
			if sc != nil {
				if sc.URL != "" {
					fmt.Printf("    url: %s\n", sc.URL)
				} else if sc.Command != "" {
					fmt.Printf("    command: %s\n", sc.Command)
				}
			}
			if st.ErrorDetail != "" {
				fmt.Printf("    error: %s\n", st.ErrorDetail)
			}
			if st.ErrorTime != "" {
				fmt.Printf("    since: %s\n", st.ErrorTime)
			}
			fmt.Printf("    fix: xdev mcps fix %s\n", st.Name)
		}
	}
	return 0
}