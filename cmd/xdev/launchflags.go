package main

// Launch-flag seams (issue #33). The flags themselves are parsed in main();
// what lives here is the state and the pure resolution helpers the run modes
// share. The package-level values exist for the same reason noRulesFlag and
// handoffMode do: their consumers — session path resolution, the tool
// registry, the approval policy — are reached from free functions that never
// see the parsed options.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

// launchFlags is the issue #33 launch surface: parsed once in main, read by
// the run modes and the helpers they call. It is process-wide for the same
// reason noRulesFlag is — the consumers include free functions (session path
// resolution, the tool registry constructor, the approval policy, the
// prompt's skill block) that never see the parsed options.
type launchFlags struct {
	// SessionDir is --session-dir: session storage and lookup for this run
	// resolve under it instead of the install data dir.
	SessionDir string
	// NoSession is --no-session (ephemeral: nothing is written to disk);
	// NoTitle is --no-title (skip the mechanical title stamp).
	NoSession bool
	NoTitle   bool
	// Thinking is --thinking; ThinkingDisplay is --hide-thinking /
	// --print-thoughts (nil follows settings.showThinking).
	Thinking        string
	ThinkingDisplay *bool
	// Tools (allowlist) and NoTools (disable every built-in) filter the
	// active tool set; NoLSP drops the lsp tool entirely.
	Tools   []string
	NoTools bool
	NoLSP   bool
	// AutoApprove is --auto-approve: the approval MODE becomes yolo.
	// Explicit per-tool denies and bash patterns still apply — the flag
	// removes prompts, it does not overrule a written rule.
	AutoApprove bool
	// MaxTime is --max-time (0 = uncapped); NoExtensions is
	// --no-extensions.
	MaxTime      time.Duration
	NoExtensions bool
	// Skills (glob patterns) and NoSkills are --skills / --no-skills: what
	// skill discovery is allowed to advertise.
	Skills   []string
	NoSkills bool
	// Advisor is --advisor: force the advisor runtime on.
	Advisor bool
}

var launch launchFlags

// sessionDataDir is the data dir session storage and lookup resolve against:
// the --session-dir override when set, else the install data dir. Sessions
// keep their <root>/sessions/<bucket>/ layout under either.
func sessionDataDir() string {
	if launch.SessionDir != "" {
		return launch.SessionDir
	}
	return config.DataDir()
}

// parseCSV splits a comma-separated flag value, dropping blanks.
func parseCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyThinkingFlag resolves --thinking against the effort the model role
// pinned. The ladder this build has is minimal|low|medium|high
// (config.EffortTokens is the carrier every adapter translates); the omp
// vocabulary maps onto it as:
//
//	off              → no thinking requested
//	xhigh, max       → high (the top rung; there is no wider budget)
//	auto, ""         → keep the role's resolved effort (provider default)
//
// ponytail: xhigh/max clamp to high instead of widening EffortTokens — a
// bigger budget needs an adapter-side vocabulary, not a flag.
func applyThinkingFlag(flagValue, roleEffort string) (string, error) {
	switch flagValue {
	case "", "auto":
		return roleEffort, nil
	case "off":
		return "", nil
	case "minimal", "low", "medium", "high":
		return flagValue, nil
	case "xhigh", "max":
		return "high", nil
	}
	return "", fmt.Errorf("thinking must be off|minimal|low|medium|high|xhigh|max|auto, got %q", flagValue)
}

// resolveThinkingDisplay folds --hide-thinking and --print-thoughts onto the
// settings default (nil = follow settings). Both drive the one showThinking
// seam: print mode renders thinking blocks through printHooks, the TUI
// through App.SetShowThinking. --hide-thinking wins when both are passed, so
// a wrapper script that adds --print-thoughts cannot undo a hide.
func resolveThinkingDisplay(hide, printThoughts bool) *bool {
	switch {
	case hide:
		v := false
		return &v
	case printThoughts:
		v := true
		return &v
	}
	return nil
}

// parseMaxTime accepts the two forms omp's --max-time takes: a bare number is
// seconds ("600"), anything else is a Go duration ("10m", "1h30m").
func parseMaxTime(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("max-time must be positive, got %q", v)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("max-time %q: %w", v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("max-time must be positive, got %q", v)
	}
	return d, nil
}

// withMaxTime bounds ctx by --max-time (d <= 0 leaves it untouched). The
// cancel func is always safe to call.
func withMaxTime(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, d)
}

// showThinkingOn resolves the reasoning display for this run: the settings
// default, overridden by --hide-thinking / --print-thoughts. Both consumers —
// print mode's stderr stream and the TUI transcript — read it, so the two
// flags cannot drift apart (display only: the model still thinks).
func showThinkingOn(settings *config.Settings) bool {
	if launch.ThinkingDisplay != nil {
		return *launch.ThinkingDisplay
	}
	if settings == nil {
		return true
	}
	return settings.ShowThinkingOn()
}

// boundedCtx is withMaxTime for a call site that owns its own cancellation
// (the RPC turn): one cancel func releases both the turn and the deadline.
func boundedCtx(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if d <= 0 {
		return ctx, cancel
	}
	tctx, tcancel := context.WithTimeout(ctx, d)
	return tctx, func() { tcancel(); cancel() }
}

// skillAllowed reports whether a discovered skill may be advertised under
// --skills (glob patterns, e.g. "git-*,docker") and --no-skills.
func skillAllowed(name string) bool {
	if launch.NoSkills {
		return false
	}
	if len(launch.Skills) == 0 {
		return true
	}
	for _, pat := range launch.Skills {
		if ok, err := path.Match(pat, name); err == nil && ok {
			return true
		}
	}
	return false
}

// applyToolFilter narrows the registry to --tools <a,b,c> / --no-tools and
// returns the flag names that matched no registered tool, so the launch can
// report a filter that narrowed less than it was asked to. A no-op when
// neither flag was passed.
// ponytail: the filter runs once, inside newToolRegistry, so MCP tools
// (attachMCP) and extension tools (attachExtensions) register afterwards and
// stay available, and the task tool's child list is its own scope. Upgrading
// means filtering again from those two attach points instead of at
// construction.
func applyToolFilter(reg *tool.Registry, allow []string, noTools bool) []string {
	if reg == nil || (len(allow) == 0 && !noTools) {
		return nil
	}
	names := reg.Names()
	if noTools {
		reg.Remove(names...)
		return nil
	}
	keep := make(map[string]bool, len(allow))
	for _, n := range allow {
		keep[n] = true
	}
	var drop []string
	for _, n := range names {
		if keep[n] {
			delete(keep, n)
			continue
		}
		drop = append(drop, n)
	}
	reg.Remove(drop...)
	var unknown []string
	for _, n := range allow {
		if keep[n] {
			unknown = append(unknown, n)
			delete(keep, n)
		}
	}
	return unknown
}

// printModelCatalog renders the resolved model catalog: the default ref, every
// provider with the models actually reachable (pinned plus live discovery),
// and the configured roles. It is the whole job of --models.
func printModelCatalog(w io.Writer, cfg *config.Config, settings *config.Settings) {
	if cfg == nil {
		fmt.Fprintln(w, "no models.yml")
		return
	}
	def := ""
	if settings != nil && settings.DefaultModel != "" {
		def = settings.DefaultModel
	}
	if def == "" {
		def = cfg.DefaultModelRef()
	}
	if def != "" {
		fmt.Fprintf(w, "default: %s\n", def)
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := cfg.Providers[name]
		if pc == nil {
			continue
		}
		api := pc.API
		if api == "" {
			api = "openai-completions"
		}
		fmt.Fprintf(w, "%s (%s", name, api)
		if pc.BaseURL != "" {
			fmt.Fprintf(w, ", %s", pc.BaseURL)
		}
		fmt.Fprintln(w, ")")
		models := providerModels(name, pc)
		if len(models) == 0 {
			fmt.Fprintln(w, "  (no models declared)")
			continue
		}
		for _, m := range models {
			line := "  " + m.ID
			if m.ContextWindow > 0 {
				line += fmt.Sprintf("  ctx=%d", m.ContextWindow)
			}
			if m.Reasoning {
				line += " reasoning"
			}
			fmt.Fprintln(w, line)
		}
	}
	if settings == nil || len(settings.ModelRoles) == 0 {
		return
	}
	roles := make([]string, 0, len(settings.ModelRoles))
	for r := range settings.ModelRoles {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	fmt.Fprintln(w, "roles:")
	for _, r := range roles {
		ref := settings.ModelRoles[r]
		if effort := settings.ModelRolesEffort[r]; effort != "" {
			ref += ":" + effort
		}
		fmt.Fprintf(w, "  %s -> %s\n", r, ref)
	}
}

// chdirTo implements --cwd: every downstream resolution (the dotenv chain,
// project settings, session bucketing, tool roots) reads the process cwd, so
// the override has to land before any of them run. The directory must exist;
// a typo'd --cwd is a startup error, never a silent fallback to the launch
// directory.
func chdirTo(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("cwd %q: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("cwd %q: not a directory", dir)
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("cwd %q: %w", dir, err)
	}
	return nil
}
