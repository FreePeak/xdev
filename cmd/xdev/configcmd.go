package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/theme"
)

// settingsFor loads the layered settings for this run (M9 #10): schema
// defaults ← ~/.xdev/agent/config.yml ← <cwd>/.xdev/config.yml ← -config
// overlays in order.
func settingsFor(overlays []string) (*config.Settings, error) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return config.LoadSettings(cwd, overlays)
}

// runConfig implements `xdev config list|get|set|reset|path`.
func runConfig(args []string, s *config.Settings) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		fmt.Fprintln(os.Stderr, "usage: xdev config list | get <key> | set <key> <value> | reset <key> | path")
		return 2
	}
	// `xdev config` edits the global layer; project files stay hand-written
	// (a project checking secrets into git is how leaks start).
	target := config.GlobalSettingsPath()
	switch sub {
	case "path":
		fmt.Println(target)
		return 0

	case "list":
		for _, line := range config.List(s, target) {
			fmt.Println(line)
		}
		return 0

	case "get":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "config get: <key> required")
			return 2
		}
		v, err := config.Get(target, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			return 1
		}
		if v == "" {
			v = fallbackValue(s, args[1])
		}
		fmt.Println(v)
		return 0

	case "set":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "config set: <key> <value> required")
			return 2
		}
		if err := validateKey(args[1], args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			return 1
		}
		if err := config.Set(target, args[1], args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			return 1
		}
		fmt.Printf("set %s in %s\n", args[1], target)
		return 0

	case "reset":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "config reset: <key> required")
			return 2
		}
		if err := config.DeleteKey(target, args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			return 1
		}
		fmt.Printf("reset %s\n", args[1])
		return 0
	}
	fmt.Fprintln(os.Stderr, "xdev config: unknown subcommand", sub)
	return 2
}

// validateKey rejects an obviously invalid value at set time rather than
// letting the next start fail on a file we just wrote.
func validateKey(key, value string) error {
	switch key {
	case "theme":
		// Delegate to the theme package so custom themes (e.g. ocean)
		// validate here exactly as they do in the TUI picker; "auto" is
		// always settable via Load's env polarity guess.
		avail := theme.AvailableThemes(theme.CustomDir())
		if value == "auto" || slices.Contains(avail, value) {
			return nil
		}
		return fmt.Errorf("unknown theme %q (available: %s)", value, strings.Join(avail, ", "))
	case "approvalMode":
		switch value {
		case "always-ask", "write", "yolo":
			return nil
		}
		return fmt.Errorf("approvalMode must be always-ask|write|yolo, got %q", value)
	case "memory":
		switch value {
		case "off", "local":
			return nil
		}
		return fmt.Errorf("memory must be off|local, got %q", value)
	case "showThinking", "advisor":
		switch value {
		case "true", "false":
			return nil
		}
		return fmt.Errorf("%s must be true|false, got %q", key, value)
	}
	return nil
}

func fallbackValue(s *config.Settings, key string) string {
	switch key {
	case "theme":
		return s.Theme
	case "approvalMode":
		return s.ApprovalMode
	case "maxTurns":
		return fmt.Sprint(s.MaxTurns)
	case "memoryLimit":
		return fmt.Sprint(s.MemoryLimit)
	case "defaultModel":
		return s.DefaultModel
	case "showThinking":
		return fmt.Sprint(s.ShowThinkingOn())
	case "advisor":
		return fmt.Sprint(s.Advisor)
	case "memory":
		if s.Memory == "" {
			return "off"
		}
		return s.Memory
	}
	return ""
}
