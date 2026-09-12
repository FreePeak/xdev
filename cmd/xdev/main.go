// Command xdev is the CLI entry: print mode (MVP), tui (M4), rpc (M6).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/FreePeak/xdev/internal/collab"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/lsp"
	"github.com/FreePeak/xdev/internal/marketplace"
	"github.com/FreePeak/xdev/internal/memlimit"
	"github.com/FreePeak/xdev/internal/serve"
	"github.com/FreePeak/xdev/internal/share"
	"github.com/FreePeak/xdev/internal/skills"
	"github.com/FreePeak/xdev/internal/tts"
)

var version = "0.1.0-dev"

// repeatable implements flag.Value for overlay paths (`-config a.yml
// -config b.yml`), whose order is the merge order.
type repeatable []string

func (r *repeatable) String() string     { return strings.Join(*r, ",") }
func (r *repeatable) Set(v string) error { *r = append(*r, v); return nil }

// mustGetwd degrades to "." rather than failing startup for a vanished cwd.
func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// loadedSettings is the layered configuration main() resolved, shared
// with the run modes (they need modelRoles/defaultModel).
var loadedSettings *config.Settings

// appliedLimit is the process memory limit actually set, for the usage
// footer (the closure runs before the value exists).
var appliedLimit int64

// subcommands are the first-arg names that select a mode instead of a
// prompt. One entry per subcommand keeps merges (and reviews) trivial.
var subcommands = map[string]bool{
	"print": true, "tui": true, "rpc": true, "acp": true, "config": true,
	"lsp-config": true, "say": true, "plugin": true, "join": true,
	"login": true, "logout": true, "version": true, "serve": true,
	"stats": true, "memory": true, "share": true,
	"update": true, "setup": true, "bench": true,
	// Wave 9 CLI suite (#34).
	"models": true, "search": true, "commit": true, "compress": true,
	"cleanse": true, "gallery": true, "render": true, "gc": true,
	"usage": true, "ps": true, "token": true, "completions": true,
	"worktree": true, "wt": true,
}

// handoffMode is the one-shot -handoff request (main owns the flag; the run
// modes consume it): the session replaces its live context with a handoff
// document and continues from it (M5 #23).
var handoffMode bool

// handoffSaveDir resolves where handoff documents are mirrored: settings
// handoff.saveToDisk puts them under <dataDir>/handoffs/<shortid>.md. Empty
// means the committed compaction entry is the only copy.
func handoffSaveDir(s *config.Settings) string {
	if s == nil || !s.HandoffSaveToDisk() {
		return ""
	}
	return filepath.Join(config.DataDir(), "handoffs")
}

func main() {
	fs := flag.NewFlagSet("xdev", flag.ContinueOnError)
	configOverlays := repeatable{}
	fs.Var(&configOverlays, "config", "extra config file layered over the defaults (repeatable)")
	model := fs.String("model", "", "model to use (provider/model)")
	continueLast := fs.Bool("continue", false, "continue the most recent session in this directory")
	resumePrefix := fs.String("resume", "", "resume a session by id prefix (e.g. -resume 01a0)")
	fromClaude := fs.String("from-claude", "", "import a Claude Code transcript (id prefix or path) and continue it")
	fromCodex := fs.String("from-codex", "", "import a Codex transcript (id prefix or path) and continue it")
	exportFile := fs.String("export", "", "write the session as one self-contained HTML file and exit")
	forkID := fs.String("fork", "", "fork a session by id prefix or path and continue the fork")
	systemPrompt := fs.String("system-prompt", "", "replace the built-in system prompt")
	appendSystemPrompt := fs.String("append-system-prompt", "", "append to the system prompt")
	personality := fs.String("personality", "", "personality preset: default | friendly | pragmatic | none (default: settings.personality)")
	themeName := fs.String("theme", "", "TUI theme: groknight | grokday (default: auto)")
	maxTurns := fs.Int("max-turns", 0, "max agent turns per run (0 = default 200)")
	maxTokens := fs.Int("max-tokens", 0, "assistant output token cap (0 = provider default)")
	apiKeyValue := fs.String("api-key", "", "credential for this run only (never persisted)")
	verbose := fs.Bool("verbose", false, "log to stderr")
	prewalkFlag := fs.Bool("prewalk", false, "one-shot model handoff: switch to the prewalk target after the first successful edit/write once a plan todo list exists")
	planFlag := fs.Bool("plan", false, "plan mode: read-only research; the run proposes a plan before implementing")
	prewalkInto := fs.String("prewalk-into", "@smol", "prewalk target: model ref or @role (default @smol)")
	noRules := fs.Bool("no-rules", false, "disable rules discovery (.omp/rules, RULES.md, third-party rulebooks)")
	hookFlag := repeatable{}
	fs.Var(&hookFlag, "hook", "hook to run: event=command, or the name of a discovered hook (repeatable)")
	trustedExtension := repeatable{}
	fs.Var(&trustedExtension, "trusted-extension", "extension whose hooks/ directory is trusted and loaded (repeatable)")
	planYolo := fs.Bool("plan-yolo", false, "plan mode with the first proposal auto-approved (implies -plan)")
	planYoloInto := fs.String("plan-yolo-into", "", "with -plan-yolo: model ref or @role to switch to after the first approved proposal (default: stay)")
	profileName := fs.String("profile", "", "named profile: relocate the user base to <base>/profiles/<name> (or set XDEV_PROFILE)")
	aliasName := fs.String("alias", "", "agent identity name for this session: other sessions address it by this name")
	modeFlag := fs.String("mode", "", "run mode: print | tui | rpc | acp (alternative to the subcommand)")
	handoffFlag := fs.Bool("handoff", false, "replace the resumed context with a handoff document before the run continues (compaction method `handoff`)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `xdev %s — lightweight coding agent (Go)

  xdev                         interactive TUI (bare invocation, TTY)
  xdev [flags] "prompt"        one-shot print run
  xdev print [flags] "prompt"  same as above
  xdev tui                     interactive TUI (Grok-CLI look)
  xdev config <sub>            settings: list | get K | set K V | reset K | path
  xdev config init-xdg [--data D --state D --cache D]  relocate the roots to XDG
  xdev join "<link>"           mirror a shared session (collab guest)
  xdev lsp-config [list|validate]  language servers, resolved binaries
  xdev models [query]          resolved model catalog (--refresh re-discovers)
  xdev search <query>          search local sessions (--regex, --here, --json)
  xdev worktree|wt <sub>       git worktrees: list | add | remove | prune
  xdev commit [--apply]        commit message from the staged diff (@commit role)
  xdev compress [--dry-run]    compact a session through the compaction ladder
  xdev cleanse [--dry-run]     redact secrets from a transcript (writes .bak)
  xdev gallery|render [id]     list sessions, render one to HTML (--html)
  xdev gc [--yes]              storage GC: orphaned blobs/artifacts/subagents
  xdev usage [--provider P]    provider accounts/limits + observed usage
  xdev ps                      xdev processes on this host (--json)
  xdev token <sub>             list | show | rotate the per-install service tokens
  xdev completions <shell>     bash | zsh | fish completion script
 @both

Flags:
`, version)
		fs.PrintDefaults()
	}
	// NOTE: Go's flag package stops at the first positional arg, so flags
	// must precede the subcommand: `xdev -continue tui`, not `xdev tui -continue`.
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *verbose {
		logx.Enable(logx.LevelDebug)
	}

	// --- install identity and native-base relocation (M14 #63). Runs
	// before the dotenv chain and the settings layer: both resolve paths
	// under the active profile (XDEV_AGENT_DIR, else XDG, else ~/.xdev/agent).
	if err := config.SetProfile(*profileName); err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		os.Exit(2)
	}
	if err := config.SetAlias(*aliasName); err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		os.Exit(2)
	}
	// Mint the per-install id on first run (0600, O_EXCL, never rewritten)
	// so provider metadata — Codex installationId, Claude device_id — sees
	// a stable value from the very first request.
	if id := config.InstallID(); *verbose {
		logx.Debugf("install-id: %s (%s)", id, config.InstallIDPath())
	}

	// --- dotenv chain (M9 #10): process env → project .env → agent .env.
	// Runs before anything reads configuration so ${VAR} expansion in
	// models.yml and credential lookup see these values.
	for _, key := range config.LoadEnv(mustGetwd()) {
		logx.Debugf("env: %s set from .env", key)
	}

	// --- layered settings (M9 #10): defaults ← global ← project ← -config
	settings, err := settingsFor(configOverlays)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		os.Exit(2)
	}
	cliKeyValue = *apiKeyValue
	loadedSettings = settings
	// M12 F2: user-declared extra SKILL.md roots (inert until wired).
	skills.SetCustomDirectories(settings.Skills.CustomDirectories)
	noRulesFlag = *noRules
	handoffMode = *handoffFlag
	appliedLimit = memlimit.ApplyFrom(settings.MemoryLimit)
	// Flag-vs-settings precedence: an explicit flag always wins.
	if *themeName == "" {
		*themeName = settings.Theme
	}
	if *model == "" {
		*model = settings.DefaultModel
	}
	if *maxTurns == 0 {
		*maxTurns = settings.MaxTurns
	}
	if *personality == "" {
		*personality = settings.Personality
	}

	// --- --export <file> (issue #61 §4): a read-only fast path — resolve
	// the selected session (-resume/-continue/--from-*), write the HTML
	// export, and exit without starting a run.
	if *exportFile != "" {
		opts := printOptions{
			ContinueLast: *continueLast,
			ResumePrefix: *resumePrefix,
			FromClaude:   *fromClaude,
			FromCodex:    *fromCodex,
			ForkID:       *forkID,
		}
		if err := runExport(*exportFile, opts); err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(2)
		}
		os.Exit(0)
	}

	args := fs.Args()
	mode := "print"
	// Subcommand names select a mode; anything else is a prompt. Keep this a
	// set, not a chained || — every new subcommand adds one line (and merges
	// cleanly instead of colliding on a single long expression).
	if len(args) > 0 && subcommands[args[0]] {
		mode, args = args[0], args[1:]
	}
	if *modeFlag != "" {
		mode = *modeFlag
	}
	if mode == "serve" {
		os.Exit(serve.Dispatch(args, version))
	}
	if mode == "join" {
		// Collab guest: mirror a shared session (M14 #59).
		if err := collab.Run(args); err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	if mode == "tui" {
		code, err := runTUI(printOptions{
			Model:        *model,
			ContinueLast: *continueLast,
			ResumePrefix: *resumePrefix,
			FromClaude:   *fromClaude,
			FromCodex:    *fromCodex,
			ForkID:       *forkID,
			SystemPrompt: *systemPrompt,
			AppendSystem: *appendSystemPrompt,
			Personality:  *personality,
			MaxTokens:    *maxTokens,
			MaxTurns:     *maxTurns,
			Prewalk:      *prewalkFlag,
			PrewalkInto:  *prewalkInto,

			Hooks:             hookFlag,
			TrustedExtensions: trustedExtension,
		}, *themeName)
		if err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(2)
		}
		os.Exit(code)
	}

	if mode == "rpc" {
		opts := printOptions{
			Model:        *model,
			ContinueLast: *continueLast,
			ResumePrefix: *resumePrefix,
			SystemPrompt: *systemPrompt,
			AppendSystem: *appendSystemPrompt,
			Personality:  *personality,
			MaxTurns:     *maxTurns,
			MaxTokens:    *maxTokens,

			Hooks:             hookFlag,
			TrustedExtensions: trustedExtension,
		}
		code, err := runRPC(opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(code)
		}
		os.Exit(code)
	}

	if mode == "acp" {
		opts := printOptions{
			Model:        *model,
			SystemPrompt: *systemPrompt,
			AppendSystem: *appendSystemPrompt,
			Personality:  *personality,
			MaxTurns:     *maxTurns,
			MaxTokens:    *maxTokens,

			Hooks:             hookFlag,
			TrustedExtensions: trustedExtension,
		}
		code, err := runACP(opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(code)
		}
		os.Exit(code)
	}

	if mode == "login" || mode == "logout" {
		cfg, cerr := config.LoadModelsLayered()
		if cerr != nil {
			cfg = &config.Config{}
		}
		provider := ""
		if len(args) > 0 {
			provider = args[0]
		}
		if provider == "" {
			fmt.Fprintln(os.Stderr, "usage: xdev login <provider> | xdev logout <provider>")
			os.Exit(2)
		}
		if mode == "login" {
			os.Exit(runLogin(provider, cfg))
		}
		os.Exit(runLogout(provider))
	}

	if mode == "config" && len(args) > 0 && args[0] == "init-xdg" {
		os.Exit(config.InitXDGCommand(args[1:], os.Stdout, os.Stderr))
	}

	if mode == "config" {
		os.Exit(runConfig(args, settings))
	}

	if mode == "lsp-config" {
		os.Exit(lsp.ConfigCommand(args, os.Stdout, os.Stderr, settings))
	}
	// Distribution surface (M14 #64): update/setup/bench parse their own flags.
	if mode == "update" || mode == "setup" || mode == "bench" {
		os.Exit(runDist(mode, args, version))
	}

	if mode == "say" {
		os.Exit(tts.Say(args, os.Stdout, os.Stderr, settings.TTSConfig()))
	}

	if mode == "plugin" {
		os.Exit(marketplace.Run(args, os.Stdout, os.Stderr, settings.Plugins.Marketplaces))
	}

	if mode == "stats" {
		os.Exit(runStats(args))
	}

	if mode == "memory" {
		os.Exit(runMemoryCLI(args, settings))
	}

	if mode == "share" {
		if err := share.Run(args); err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(2)
		}
		os.Exit(0)
	}

	// --- Wave 9 CLI suite (issue #34). One dispatch line per subcommand:
	// each prints its own usage and exits on its own.
	if mode == "models" {
		os.Exit(runModels(args))
	}
	if mode == "search" {
		os.Exit(runSearch(args))
	}
	if mode == "worktree" || mode == "wt" {
		os.Exit(runWorktree(args))
	}
	if mode == "commit" {
		os.Exit(runCommit(args))
	}
	if mode == "compress" {
		os.Exit(runCompress(args))
	}
	if mode == "cleanse" {
		os.Exit(runCleanse(args))
	}
	if mode == "gallery" || mode == "render" {
		os.Exit(runGallery(args))
	}
	if mode == "gc" {
		os.Exit(runGC(args))
	}
	if mode == "usage" {
		os.Exit(runUsage(args))
	}
	if mode == "ps" {
		os.Exit(runPS(args))
	}
	if mode == "token" {
		os.Exit(runToken(args))
	}
	if mode == "completions" {
		// The script is generated from the root FlagSet parsed above, so
		// completions stay current with every flag registration.
		os.Exit(runCompletions(args, fs))
	}

	switch mode {
	case "version":
		fmt.Printf("xdev %s\n", version)
	case "print":
		prompt := ""
		if len(args) > 0 {
			prompt = args[0]
		}
		if prompt == "" && !*continueLast && *resumePrefix == "" && *forkID == "" && *fromClaude == "" && *fromCodex == "" {
			if stdinIsTerminal() {
				// Bare interactive invocation: open the TUI.
				code, err := runTUI(printOptions{
					Model:        *model,
					ContinueLast: *continueLast,
					ResumePrefix: *resumePrefix,
					FromClaude:   *fromClaude,
					FromCodex:    *fromCodex,
					ForkID:       *forkID,
					SystemPrompt: *systemPrompt,
					AppendSystem: *appendSystemPrompt,
					Personality:  *personality,
					MaxTokens:    *maxTokens,
				}, *themeName)
				if err != nil {
					fmt.Fprintln(os.Stderr, "xdev:", err)
					os.Exit(2)
				}
				os.Exit(code)
			}
			// Pipe usage: read the prompt from stdin.
			buf := make([]byte, 0, 4096)
			tmp := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(tmp)
				buf = append(buf, tmp[:n]...)
				if err != nil {
					break
				}
			}
			prompt = string(buf)
		}
		if prompt == "" && !*continueLast && *resumePrefix == "" && *forkID == "" && *fromClaude == "" && *fromCodex == "" {
			fs.Usage()
			os.Exit(2)
		}
		opts := printOptions{
			Model:        *model,
			ContinueLast: *continueLast,
			ResumePrefix: *resumePrefix,
			FromClaude:   *fromClaude,
			FromCodex:    *fromCodex,
			ForkID:       *forkID,
			SystemPrompt: *systemPrompt,
			AppendSystem: *appendSystemPrompt,
			Personality:  *personality,
			MaxTurns:     *maxTurns,
			MaxTokens:    *maxTokens,
			Prewalk:      *prewalkFlag,
			PrewalkInto:  *prewalkInto,
			Plan:         *planFlag,

			Hooks:             hookFlag,
			TrustedExtensions: trustedExtension,
			PlanYolo:          *planYolo,
			PlanYoloInto:      *planYoloInto,
		}
		if *planYoloInto != "" && !*planYolo {
			fmt.Fprintln(os.Stderr, "xdev: -plan-yolo-into has no effect without -plan-yolo")
		}
		code, err := runPrint(prompt, opts)
		if err != nil {
			logx.Debugf("print failed: %v", err)
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(code)
		}
	}
}

// stdinIsTerminal reports whether stdin is an interactive TTY (as opposed
// to a pipe or file feeding a prompt).
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
