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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/rules"
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
	// ApprovalMode is --approval-mode <always-ask|write|yolo>: overrides
	// tools.approvalMode for this session. AutoApprove (--auto-approve /
	// --yolo) is the shorthand for yolo and wins when both are given.
	ApprovalMode string
	// NoPrewalk is --no-prewalk: force the handoff off even when the
	// prewalk.enabled setting turns it on. omp has the same escape hatch.
	NoPrewalk bool
	// Smol/Slow/PlanModel are --smol / --slow / --plan-model: per-run role
	// model overrides (omp spells the third --plan <model>; xdev keeps
	// --plan for read-only plan mode and exposes the role as --plan-model).
	Smol      string
	Slow      string
	PlanModel string
	// Models are the --models patterns for Ctrl+P model cycling (omp
	// parity). The catalog listing stays on the `models` subcommand.
	Models []string
	// Provider is --provider (legacy): force the provider when the model
	// ref does not name one.
	Provider string
	// ExtraDirs are --add-dir roots: extra workspace directories beyond the
	// launch cwd. They join path completion and context-file discovery, and
	// are named in the prompt so the model knows they are in scope.
	ExtraDirs []string
	// AllowHome is --allow-home: start in $HOME without the temp-dir switch
	// omp performs by default.
	AllowHome bool
	// NoPTY is --no-pty: accepted for omp parity. xdev's bash is pipe-based
	// and never allocates a PTY, so the flag restates the existing behavior
	// instead of changing it.
	NoPTY bool
	// Prewalk is --prewalk: force the handoff on for this run (the
	// prewalk.enabled setting is the persistent form; NoPrewalk wins over
	// both).
	Prewalk bool
	// Extensions are -e/--extension paths: explicit extension loads that do
	// not depend on discovery. PluginDirs are --plugin-dir roots added to
	// plugin discovery.
	Extensions []string
	PluginDirs []string
}

// absClean is filepath.Abs + Clean: the canonical form the workspace roots
// and the home check compare and walk with.
func absClean(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// roleOverride returns the per-run model override for a role name
// (smol|slow|plan), or "" when the flag was not given.
func roleOverride(role string) string {
	switch role {
	case "smol":
		return launch.Smol
	case "slow":
		return launch.Slow
	case "plan":
		return launch.PlanModel
	}
	return ""
}

// approvalModeOverride is the effective tools.approvalMode for this run:
// --auto-approve/--yolo force yolo, else a validated --approval-mode, else
// "" (the settings value stands).
func approvalModeOverride() string {
	if launch.AutoApprove {
		return "yolo"
	}
	switch launch.ApprovalMode {
	case "always-ask", "write", "yolo":
		return launch.ApprovalMode
	}
	return ""
}

// workspaceDirs is the launch cwd plus every --add-dir root, deduped and
// absolutized. Discovery walks these roots in order.
func workspaceDirs(cwd string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, d := range append([]string{cwd}, launch.ExtraDirs...) {
		if d == "" {
			continue
		}
		abs, err := absClean(d)
		if err != nil {
			continue
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// homeSwitchDir reports whether the launch cwd is the user's home directory
// and the run did not opt in with --allow-home. omp starts such runs in a
// temp dir so state is not dropped into $HOME; the caller prints the notice.
func homeSwitchDir(cwd string) (string, bool) {
	if launch.AllowHome || cwd == "" {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	abs, err := absClean(cwd)
	if err != nil {
		return "", false
	}
	homeAbs, err := absClean(home)
	if err != nil {
		return "", false
	}
	if abs != homeAbs {
		return "", false
	}
	dir, err := os.MkdirTemp("", "xdev-home-*")
	if err != nil {
		return "", false
	}
	return dir, true
}

var launch launchFlags

// Role model overrides (--smol / --slow / --plan-model). Process-wide for
// the same reason as roleOverride: the resolution path is reached from free
// functions that never see the parsed options.
var (
	smolModelFlag string
	slowModelFlag string
	planModelFlag string
)

// splitPatterns flattens the repeatable --models values, splitting each on
// commas so `--models a,b --models c` and `--models a,b,c` agree.
func splitPatterns(vals []string) []string {
	out := []string{}
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

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
	return "", fmt.Errorf("thinking must be %s, got %q", strings.Join(config.ThinkingLevels, "|"), flagValue)
}

// thinkingLevel folds the settings key under the flag: --thinking wins when it
// names a level, otherwise the persisted `thinking` layer decides ("auto" when
// neither says anything). The role's ":effort" is deliberately NOT consulted
// here — applyThinkingFlag takes it as the "auto" fallback, which keeps one
// precedence ladder: flag > settings > role effort.
func thinkingLevel(settings *config.Settings, flagValue string) string {
	if lv := strings.TrimSpace(flagValue); lv != "" && lv != "auto" {
		return lv // trimmed: a padded flag is still the level it names
	}
	return settings.ThinkingLevel()
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
	// Always cancellable, deadline or not. Returning the parent with a no-op
	// cancel when --max-time is unset made the TUI's cancel handle do
	// nothing: Esc/Ctrl+C during a running turn could never abort it, because
	// the only thing they call was that no-op (user-reported).
	ctx, cancel := context.WithCancel(parent)
	if d > 0 {
		ctx, cancel = context.WithTimeout(ctx, d)
	}
	return ctx, cancel
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

// ttsrConfig resolves the stream-rule config for this run: the settings
// `ttsr` group plus every DISCOVERED rule file that carries a `condition:`.
// The rules parser stores that field verbatim and nothing consumed it, so a
// rulebook rule like
//
// (condition: "SK-SECRET")
//
// was silently inert (parity finding T3 #39). A settings-declared rule of the
// same name wins, so the merge never overrides an explicit declaration.
func ttsrConfig(s *config.Settings) *config.TTSRSettings {
	if s == nil {
		return nil
	}
	base := s.TTSR
	if base == nil {
		// A rulebook condition with no `ttsr` group still needs an engine;
		// the shipped defaults (enabled, interrupt always) apply.
		on := true
		base = &config.TTSRSettings{Enabled: &on}
	}
	declared := map[string]bool{}
	for _, r := range base.Rules {
		declared[r.Name] = true
	}
	merged := *base
	merged.Rules = append([]config.TTSRRule(nil), base.Rules...)
	for _, dr := range rules.Active() {
		if dr.Condition == "" || declared[dr.Name] {
			continue
		}
		merged.Rules = append(merged.Rules, config.TTSRRule{
			Name:          dr.Name,
			Condition:     dr.Condition,
			InterruptMode: ttsrModeOrInherit(dr.InterruptMode),
			Message:       dr.Description,
		})
	}
	return &merged
}

// ttsrModeOrInherit keeps only the modes the engine understands; a typo in a
// rulebook inherits the group policy instead of reaching the matcher raw
// (settings rules are validated at load; discovered files are not).
func ttsrModeOrInherit(m string) string {
	switch m {
	case "always", "prose-only", "tool-only", "never":
		return m
	}
	return ""
}
