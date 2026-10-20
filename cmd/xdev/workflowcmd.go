package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/opencode"
)

const workflowUsage = `xdev workflow — named workflow presets

Usage:
  xdev workflow list                 list the presets your config declares
  xdev workflow show <name>          print one preset, including its context budget
  xdev workflow run <name> [text]    create an opencode session from a preset and prompt it
  xdev workflow sessions [dir]       roster: the opencode sessions xdev can see

xdev ships no presets of its own. Everything listed here comes from
workflow.presets in your config (~/.xdev/agent/config.yml, or the XDG root
after 'xdev config init-xdg').

xdev attaches to the opencode you already run — it starts nothing, stops
nothing, and changes nothing in opencode's own config.
`

// runWorkflow implements the `xdev workflow` subcommand.
//
// It exists so presets are inspectable without the TUI. A preset that can only
// be seen by launching the TUI is a preset nobody audits — and the whole point
// of the context block is that it is auditable before a run costs anything.
func runWorkflow(args []string, stdout, stderr *os.File, settings *config.Settings) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, workflowUsage)
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return workflowList(stdout, settings)
	case "show":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "xdev workflow show: needs a preset name")
			return 2
		}
		return workflowShow(stdout, settings, args[1])
	case "run":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "xdev workflow run: needs a preset name")
			return 2
		}
		text := strings.Join(args[2:], " ")
		return workflowRun(stdout, stderr, settings, args[1], text)
	case "sessions":
		return workflowSessions(stdout, stderr, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(stdout, workflowUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "xdev workflow: unknown subcommand %q\n\n%s", args[0], workflowUsage)
		return 2
	}
}

func workflowList(stdout *os.File, settings *config.Settings) int {
	names := settings.PresetNames()
	if len(names) == 0 {
		fmt.Fprintln(stdout, "No workflow presets declared.")
		fmt.Fprintln(stdout, "Add them under workflow.presets in your xdev config.")
		return 0
	}
	for _, name := range names {
		p := settings.Preset(name)
		if p == nil {
			continue
		}
		desc := p.Description
		if desc == "" {
			desc = "(no description)"
		}
		if p.Backend == "" {
			fmt.Fprintf(stdout, "%-20s %s  [local]\n", p.Name, desc)
			continue
		}
		fmt.Fprintf(stdout, "%-20s %s  [%s / agent %s]\n", p.Name, desc, p.Backend, p.Agent)
	}
	return 0
}

// workflowShow prints the declared context budget, because that is the field
// the user cannot otherwise check before paying for a run.
func workflowShow(stdout *os.File, settings *config.Settings, name string) int {
	p := settings.Preset(name)
	if p == nil {
		fmt.Fprintf(stdout, "xdev workflow: no preset named %q\n", name)
		if names := settings.PresetNames(); len(names) > 0 {
			fmt.Fprintf(stdout, "declared: %s\n", strings.Join(names, ", "))
		}
		return 1
	}
	fmt.Fprintf(stdout, "name        %s\n", p.Name)
	if p.Description != "" {
		fmt.Fprintf(stdout, "description %s\n", p.Description)
	}
	if p.Backend == "" {
		fmt.Fprintf(stdout, "backend     (local — drives nothing)\n")
	} else {
		fmt.Fprintf(stdout, "backend     %s\n", p.Backend)
		fmt.Fprintf(stdout, "agent       %s\n", p.Agent)
	}
	if p.Directory != "" {
		fmt.Fprintf(stdout, "directory   %s\n", p.Directory)
	}
	if p.Prompt != "" {
		fmt.Fprintf(stdout, "prompt      %s\n", oneLine(p.Prompt))
	}
	if p.Context == nil {
		fmt.Fprintf(stdout, "context     (none declared)\n")
		return 0
	}
	if m := p.Context.Model; m != nil {
		variant := m.Variant
		if variant == "" {
			variant = "(default)"
		}
		fmt.Fprintf(stdout, "model       %s/%s variant %s\n", m.ProviderID, m.ModelID, variant)
	} else {
		fmt.Fprintf(stdout, "model       opencode's own\n")
	}
	if len(p.Context.Permissions) == 0 {
		fmt.Fprintf(stdout, "permissions opencode's own\n")
	} else {
		fmt.Fprintf(stdout, "permissions\n")
		for _, r := range p.Context.Permissions {
			fmt.Fprintf(stdout, "  %-8s %-20s %s\n", r.Action, r.Resource, r.Effect)
		}
	}
	return 0
}

// workflowRun creates a session from a preset and sends its prompt. It prints
// the new session id and exits — the session itself is watched through the
// TUI, not through this command.
func workflowRun(stdout, stderr *os.File, settings *config.Settings, name, text string) int {
	p := settings.Preset(name)
	if p == nil {
		fmt.Fprintf(stderr, "xdev workflow: no preset named %q\n", name)
		return 1
	}
	if p.Backend == "" {
		fmt.Fprintf(stderr, "xdev workflow: preset %q is local and drives nothing; run it from the TUI\n", name)
		return 2
	}
	prompt := strings.TrimSpace(text)
	if prompt == "" {
		prompt = p.Prompt
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintf(stderr, "xdev workflow: preset %q has no prompt and none was given on the command line\n", name)
		return 2
	}

	// Discovery, not a hardcoded endpoint: opencode may not be running, and
	// ErrNoServer means exactly that (never a guessed URL).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := opencode.New()
	if _, err := c.Discover(ctx); err != nil {
		fmt.Fprintln(stderr, "xdev workflow:", err)
		fmt.Fprintln(stderr, "  start opencode's server with `opencode service start`, then retry")
		return 1
	}

	req := opencode.CreateRequest{Agent: p.Agent, Title: oneLine(prompt)}
	if p.Directory != "" {
		req.Directory = p.Directory
	}
	if m := p.Context.Model; m != nil {
		req.Model = &opencode.ModelRef{ID: m.ModelID, ProviderID: m.ProviderID, Variant: m.Variant}
	}
	if rules := p.Context.Permissions; len(rules) > 0 {
		perms := make([]opencode.PermissionRule, 0, len(rules))
		for _, r := range rules {
			perms = append(perms, opencode.PermissionRule{
				Action: r.Action, Resource: r.Resource, Effect: r.Effect,
			})
		}
		req.Permissions = perms
	}

	sess, err := c.CreateSession(ctx, req)
	if err != nil {
		fmt.Fprintln(stderr, "xdev workflow: create session:", err)
		return 1
	}
	if err := c.Prompt(ctx, sess.ID, prompt); err != nil {
		fmt.Fprintf(stderr, "xdev workflow: prompt %s: %v\n", sess.ID, err)
		return 1
	}
	fmt.Fprintf(stdout, "session %s  agent %s\n", sess.ID, sess.Agent)
	fmt.Fprintf(stdout, "  watch it from the TUI, or `xdev workflow show %s` for its budget\n", name)
	return 0
}

// workflowSessions prints the roster: every opencode session xdev can see,
// with the status a dock row needs.
//
// It reads Session.Info only. No message is fetched, so this is cheap enough
// to run on a timer, and it changes nothing on opencode's side.
func workflowSessions(stdout, stderr *os.File, args []string) int {
	directory := ""
	if len(args) > 0 {
		directory = args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := opencode.New()
	if _, err := c.Discover(ctx); err != nil {
		fmt.Fprintln(stderr, "xdev workflow sessions:", err)
		fmt.Fprintln(stderr, "  start opencode's server with `opencode service start`, then retry")
		return 1
	}
	rows, err := c.ListSessions(ctx, directory)
	if err != nil {
		fmt.Fprintln(stderr, "xdev workflow sessions:", err)
		return 1
	}
	if len(rows) == 0 {
		if directory != "" {
			fmt.Fprintf(stdout, "No opencode sessions in %s.\n", directory)
		} else {
			fmt.Fprintln(stdout, "No opencode sessions.")
		}
		return 0
	}
	for _, r := range rows {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		model := r.Model.ProviderID + "/" + r.Model.ID
		if r.Model.Variant != "" {
			model += ":" + r.Model.Variant
		}
		outcome := r.Outcome
		if outcome == "" {
			outcome = "-"
		}
		fmt.Fprintf(stdout, "%-26s %-38s %-6s %-26s $%.4f in=%-9d out=%-7d %s\n",
			r.ID, oneLine(title), r.Agent, model, r.Cost,
			r.Tokens.Input, r.Tokens.Output, outcome)
	}
	fmt.Fprintf(stdout, "\n%d session(s)\n", len(rows))
	return 0
}
