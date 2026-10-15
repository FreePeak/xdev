package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runTmux: `xdev tmux` boots the TUI inside a named tmux session so it
// survives terminal close, and supports re-attaching from 2+ terminals.
//
// Usage:
//   xdev tmux                # new detached session named "xdev" (or attach if one exists)
//   xdev tmux <name>         # use a custom session name
//   xdev tmux attach [name]  # attach an existing session
//   xdev tmux list           # list sessions
//   xdev tmux kill [name]    # kill a session
func runTmux(args []string) int {
	tm, err := exec.LookPath("tmux")
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev tmux: tmux not installed. install with `brew install tmux` (or apt)")
		return 1
	}
	session := "xdev"
	if len(args) > 0 && args[0] != "attach" && args[0] != "kill" && args[0] != "list" && args[0] != "new" {
		session = args[0]
		args = args[1:]
	}
	switch {
	case len(args) > 0 && args[0] == "list":
		cmd := exec.Command(tm, "ls")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return runExit(cmd.Run())
	case len(args) > 0 && args[0] == "kill":
		name := session
		if len(args) > 1 { name = args[1] }
		return runExit(exec.Command(tm, "kill-session", "-t", name).Run())
	case len(args) > 0 && (args[0] == "attach" || args[0] == "a"):
		name := session
		if len(args) > 1 { name = args[1] }
		cmd := exec.Command(tm, "attach", "-t", name)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return runExit(cmd.Run())
	default:
		// new: create detached if missing, then attach.
		// check exists
		check := exec.Command(tm, "has-session", "-t", session)
		if err := check.Run(); err != nil {
			cmd := exec.Command(tm, "new-session", "-d", "-s", session, "xdev tui")
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintln(os.Stderr, "xdev tmux:", strings.TrimSpace(string(out)), err)
				return 1
			}
		}
		fmt.Printf("tmux session %q ready.\n", session)
		fmt.Printf("  attach: tmux attach -t %s   (or: xdev tmux attach %s)\n", session, session)
		fmt.Printf("  many windows can `xdev tmux attach %s` simultaneously.\n", session)
		return 0
	}
}

func runExit(err error) int {
	if err != nil { return 1 }
	return 0
}
