// Command xdev is the CLI entry: print mode (MVP), tui (M4), rpc (M6).
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/FreePeak/xdev/internal/ai"
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
// with the run modes (they need defaultModel).
var loadedSettings *config.Settings

// appliedLimit is the process memory limit actually set, for the usage
// footer (the closure runs before the value exists).
var appliedLimit int64

// subcommands are the first-arg names that select a mode instead of a
// prompt. One entry per subcommand keeps merges (and reviews) trivial.
var subcommands = map[string]bool{
	"mcps": true,
	"print": true, "tui": true, "rpc": true, "acp": true, "config": true,
	"lsp-config": true, "say": true, "plugin": true, "join": true,
	"login": true, "logout": true, "version": true, "serve": true,
	"stats": true, "memory": true, "share": true,
	"update": true, "setup": true, "bench": true,
	// Wave 9 CLI suite (#34).
	"models": true, "search": true, "commit": true, "compress": true,
	"cleanse": true, "gallery": true, "render": true, "gc": true,
	"usage": true, "ps": true, "token": true, "completions": true,
	"connect":  true,
	"worktree": true, "wt": true,
	// Repository hook trust (#241): the review and the decision.
	"trust": true, "distrust": true,
	// `xdev help` prints usage; without this entry the word falls through
	// to print mode and is sent to the model as a prompt.
	"help": true,
	// omp names these services at the top level; xdev ships them under
	// `serve`, so the names dispatch there rather than reaching the model as
	// a prompt (#104).
	"auth-broker": true, "auth-gateway": true, "browser-relay": true,
	// omp's `install` is its plugin installer.
	"install": true,
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

// rootUsage is the root usage body (the Fprintf format: one %s for the
// version). A package-level constant so usage_test.go can assert that
// every subcommands entry is documented and that no merge marker
// survives in the text users read.
const rootUsage = `xdev %s — lightweight coding agent (Go)

  xdev mcps                  list configured MCP servers and their status
  xdev                         interactive TUI (bare invocation, TTY)
  xdev [flags] "prompt"        one-shot print run
  xdev print [flags] "prompt"  same as above
  xdev tui                     interactive TUI (Grok-CLI look)
  xdev rpc                     JSONL-over-stdio RPC server (embedders)
  xdev acp                     ACP server on stdio (editors)
  xdev join "<link>"           mirror a shared session (collab guest)
  xdev config <sub>            settings: list | get K | set K V | reset K | path
  xdev config init-xdg [--data D --state D --cache D]  relocate the roots to XDG
  xdev trust|distrust      review and allow (or withhold) a repository's hooks (--list)
  xdev models [query]          resolved model catalog (--refresh re-discovers)
  xdev connect [provider]      connect a catalog provider (--list shows them)
  xdev login | logout          Claude Pro/Max and Codex OAuth (PKCE browser flow)
  xdev lsp-config [list|validate]  language servers, resolved binaries
  xdev memory <sub>            show | stats | lessons | add | edit | export | import | scratchpad | clear
  xdev stats [--summary|--json|--serve]  usage over the local session store
  xdev usage [--provider P]    provider accounts/limits + observed usage
  xdev token <sub>             list | show | rotate the per-install service tokens
  xdev search <query>          search local sessions (--regex, --here, --json)
  xdev gallery|render [id]     list sessions, render one to HTML (--html)
  xdev compress [--dry-run]    compact a session through the compaction ladder
  xdev cleanse [--dry-run]     redact secrets from a transcript (writes .bak)
  xdev gc [--yes]              storage GC: orphaned blobs/artifacts/subagents
  xdev commit [--apply]        commit message from the staged diff
  xdev worktree|wt <sub>       git worktrees: list | add | remove | prune
  xdev plugin <sub>            plugins: list | search | install | remove | info
  xdev share [id|path]         serve an E2E-encrypted view-only snapshot
  xdev serve <svc>             auth-broker | auth-gateway | browser-relay (each also works bare)
  xdev install <name>          alias of "plugin install"
  xdev say [--voice V] [--rate N] [--dry-run] "text"  speak text aloud (local TTS)
  xdev update [--channel C]    check for and install updates (stable | canary)
  xdev update job <verb>       the twice-daily release check: install | remove | status
  xdev setup                   onboarding: data dir, starter config, next steps
  xdev bench [--turns N]       TTFT + decode p50/p95 through the provider seam
  xdev completions <shell>     bash | zsh | fish completion script
  xdev version                 print the version
  xdev help                    print this usage and exit

Flags:
`

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
	logFile := fs.String("log", "", "write TUI screen transcript to <path> after each frame (off by default)")
	prewalkFlag := fs.Bool("prewalk", false, "one-shot model handoff: switch to the prewalk target after the first successful edit/write once a plan todo list exists")
	planFlag := fs.Bool("plan", false, "plan mode: read-only research; the run proposes a plan before implementing")
	prewalkInto := fs.String("prewalk-into", "", "prewalk target: model ref (default: prewalk.into, else the session model)")
	noRules := fs.Bool("no-rules", false, "disable rules discovery (.omp/rules, RULES.md, third-party rulebooks)")
	hookFlag := repeatable{}
	fs.Var(&hookFlag, "hook", "hook to run: event=command, or the name of a discovered hook (repeatable)")
	trustedExtension := repeatable{}
	fs.Var(&trustedExtension, "trusted-extension", "extension whose hooks/ directory is trusted and loaded (repeatable)")
	planYolo := fs.Bool("plan-yolo", false, "plan mode with the first proposal auto-approved (implies -plan)")
	planYoloInto := fs.String("plan-yolo-into", "", "with -plan-yolo: model ref to switch to after the first approved proposal (default: stay)")
	profileName := fs.String("profile", "", "named profile: relocate the user base to <base>/profiles/<name> (or set XDEV_PROFILE)")
	aliasName := fs.String("alias", "", "agent identity name for this session: other sessions address it by this name")
	modeFlag := fs.String("mode", "", "run mode: print | tui | rpc | acp (alternative to the subcommand)")
	handoffFlag := fs.Bool("handoff", false, "replace the resumed context with a handoff document before the run continues (compaction method `handoff`)")
	// --- issue #33 launch flags. Grouped with the run-shaping flags above;
	// each one's consumer is noted where the value lands below.
	cwdFlag := fs.String("cwd", "", "directory to start in (overrides the launch cwd)")
	sessionDir := fs.String("session-dir", "", "session storage and lookup root for this run (default: the install data dir; sessions live under <dir>/sessions)")
	noSession := fs.Bool("no-session", false, "don't save the session (ephemeral: nothing is written to disk)")
	noTitle := fs.Bool("no-title", false, "skip the session title entirely (the mechanical stamp and the ai-title pass)")
	modelsPatterns := repeatable{}
	fs.Var(&modelsPatterns, "models", "comma-separated model patterns for Ctrl+P cycling (the catalog listing is the `models` subcommand)")
	thinkingFlag := fs.String("thinking", "", "thinking level: off | minimal | low | medium | high | xhigh | max | auto (xhigh/max clamp to high; default: the `thinking` settings key, then the model's inline `:effort`)")
	hideThinking := fs.Bool("hide-thinking", false, "hide thinking blocks in TUI output (display only; model thinking is unaffected)")
	printThoughts := fs.Bool("print-thoughts", false, "include thinking blocks in print-mode output")
	toolsFlag := fs.String("tools", "", "comma-separated tools to enable (default: all)")
	noTools := fs.Bool("no-tools", false, "disable all built-in tools")
	noLSP := fs.Bool("no-lsp", false, "disable the lsp tool (no language server is started)")
	autoApprove := fs.Bool("auto-approve", false, "auto-approve every tool call (approval mode yolo; explicit per-tool denies and bash patterns still apply)")
	fs.BoolVar(autoApprove, "yolo", false, "alias for --auto-approve")
	approvalModeFlag := fs.String("approval-mode", "", "approval mode for this run: always-ask | write | yolo (overrides tools.approvalMode)")
	advisorFlag := fs.Bool("advisor", false, "enable the advisor runtime (a background reviewer; needs advisorModel)")
	maxTimeFlag := fs.String("max-time", "", "stop the run after this duration (600 = 600s, 10m, 1h)")
	noExtensions := fs.Bool("no-extensions", false, "disable extension discovery (no extension tool, command, or policy hook loads)")
	skillsFlag := fs.String("skills", "", "comma-separated glob patterns filtering which skills are advertised (e.g. git-*,docker)")
	noSkills := fs.Bool("no-skills", false, "disable skills discovery (nothing is advertised to the model)")
	// --- omp CLI parity (docs/parity-delta.md): the aliases and flags the
	// baseline accepts, each with a real consumer below.
	printModeFlag := fs.Bool("print", false, "force headless print mode (alias: -p)")
	fs.BoolVar(printModeFlag, "p", false, "alias for --print")
	fs.BoolVar(continueLast, "c", false, "alias for --continue")
	fs.StringVar(resumePrefix, "r", "", "alias for --resume (session id prefix)")
	fs.StringVar(resumePrefix, "session", "", "resume a session by id prefix (alias: --session)")
	noPrewalk := fs.Bool("no-prewalk", false, "force the prewalk handoff off even when the prewalk.enabled setting turns it on")
	retryForever := fs.Bool("retry-forever", false, "keep the retry ladder running forever once every failover target is down (retry.infinite for this run)")
	providerFlag := fs.String("provider", "", "force the provider when the model ref does not name one")
	addDirs := repeatable{}
	fs.Var(&addDirs, "add-dir", "extra workspace root beyond the launch cwd: joins context-file discovery and is named in the prompt (repeatable)")
	allowHome := fs.Bool("allow-home", false, "start in $HOME without the temp-dir switch (default: switch, like omp)")
	noPTY := fs.Bool("no-pty", false, "accepted for omp parity: xdev's bash is pipe-based and never allocates a PTY")
	extensionPaths := repeatable{}
	fs.Var(&extensionPaths, "extension", "load an explicit extension by path (repeatable)")
	fs.Var(&extensionPaths, "e", "alias for --extension")
	pluginDirs := repeatable{}
	fs.Var(&pluginDirs, "plugin-dir", "extra plugin discovery root (repeatable)")
	versionFlag := fs.Bool("version", false, "print the version and exit (alias: -v)")
	fs.BoolVar(versionFlag, "v", false, "alias for --version")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, rootUsage, version)
		fs.PrintDefaults()
	}
	// NOTE: Go's flag package stops at the first positional arg, so flags
	// must precede the subcommand: `xdev -continue tui`, not `xdev tui -continue`.
	if err := fs.Parse(os.Args[1:]); err != nil {
		// -h/--help is a successful query, not a usage error: exit 0 like
		// omp so `xdev -h && …` composes. A real parse error stays 2.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if *versionFlag {
		fmt.Printf("xdev %s\n", version)
		os.Exit(0)
	}
	if *verbose {
		logx.Enable(logx.LevelDebug)
	}

	// --- issue #33 launch flags: fail-fast validation, then the chdir.
	// --cwd has to land before everything downstream: the dotenv chain,
	// project settings, session bucketing and the tool roots all resolve
	// against the process cwd.
	maxTime, err := parseMaxTime(*maxTimeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		os.Exit(2)
	}
	if *cwdFlag != "" {
		if err := chdirTo(*cwdFlag); err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(2)
		}
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
	// a stable value from the very first request. Handing it to the wire
	// layer is what actually attaches it (#102: it minted and stopped).
	if id := config.InstallID(); id != "" {
		ai.SetInstallIdentity(id)
		if *verbose {
			logx.Debugf("install-id: %s (%s)", id, config.InstallIDPath())
		}
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
	// #114: this repository's own config files may set the interface and the
	// caps, and nothing with authority in it. Every key the boundary refused
	// is named here, so a user never believes a clone configured something it
	// did not. Before the screen opens, so the line is readable in every mode.
	for _, notice := range config.RepoTrustNotices(settings) {
		fmt.Fprintln(os.Stderr, notice)
	}
	cliKeyValue = *apiKeyValue
	loadedSettings = settings
	// M12 F2: user-declared extra SKILL.md roots (inert until wired).
	skills.SetCustomDirectories(settings.Skills.CustomDirectories)
	noRulesFlag = *noRules
	handoffMode = *handoffFlag
	appliedLimit = memlimit.ApplyFrom(settings.MemoryLimit)
	// issue #33: the launch surface the modes and their helpers read.
	launch = launchFlags{
		SessionDir:      *sessionDir,
		NoSession:       *noSession,
		NoTitle:         *noTitle,
		Thinking:        *thinkingFlag,
		ThinkingDisplay: resolveThinkingDisplay(*hideThinking, *printThoughts),
		Tools:           parseCSV(*toolsFlag),
		NoTools:         *noTools,
		NoLSP:           *noLSP,
		AutoApprove:     *autoApprove,
		ApprovalMode:    *approvalModeFlag,
		MaxTime:         maxTime,
		NoExtensions:    *noExtensions,
		Skills:          parseCSV(*skillsFlag),
		NoSkills:        *noSkills,
		Advisor:         *advisorFlag,
		NoPrewalk:       *noPrewalk,
		Models:          splitPatterns(modelsPatterns),
		Provider:        *providerFlag,
		ExtraDirs:       addDirs,
		AllowHome:       *allowHome,
		NoPTY:           *noPTY,
		Extensions:      extensionPaths,
		PluginDirs:      pluginDirs,
		LogFile:       *logFile,
	}
	// --- launch-flag overrides onto the layered settings. Each one is a
	// documented flag, so each must win over the file: approval mode and
	// the prewalk off-switch.
	if mode := approvalModeOverride(); mode != "" {
		settings.ApprovalMode = mode
	}
	// --plugin-dir roots join plugin discovery (commands/skills/agents/hooks).
	if len(launch.PluginDirs) > 0 {
		marketplace.SetExtraRoots(launch.PluginDirs)
	}
	if launch.NoPrewalk {
		settings.Prewalk.Enabled = false
	} else if launch.Prewalk {
		settings.Prewalk.Enabled = true
	}
	// -retry-forever is the one-run form of retry.infinite. It rides the
	// layered settings rather than a launchFlags field: every mode reads
	// this object through lastSettings(), so one write covers TUI, print,
	// RPC and ACP.
	if *retryForever {
		infiniteFlag := true
		settings.Retry.Infinite = &infiniteFlag
	}
	// --models patterns enable Ctrl+P cycling; the catalog print stays on
	// the `models` subcommand (omp keeps the same split).
	if len(launch.Models) > 0 {
		settings.Models.Cycle = launch.Models
	}
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

	// NOTE: the --models flag is the Ctrl+P cycling pattern list (omp
	// parity); the resolved catalog listing is the `models` subcommand.

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
	// -p/--print forces headless print mode regardless of how the mode was
	// selected (bare invocation, subcommand, or a --mode value).
	if *printModeFlag {
		mode = "print"
	}
	// --- --allow-home (omp parity): a RUN started in $HOME switches to a
	// temp dir so session state and stray tool output do not land in the
	// home directory. --allow-home opts out, and utility subcommands
	// (config, stats, search, completions, …) keep the real cwd — moving
	// those would silently re-bucket what they report. Gated on the run
	// modes for the same reason: `xdev config list` from $HOME must answer
	// about $HOME.
	switch mode {
	case "print", "tui", "rpc", "acp", "join":
		if dir, switched := homeSwitchDir(mustGetwd()); switched {
			if err := os.Chdir(dir); err != nil {
				fmt.Fprintln(os.Stderr, "xdev:", err)
				os.Exit(2)
			}
			fmt.Fprintf(os.Stderr, "xdev: starting in %s (home directory; --allow-home to stay)\n", dir)
		}
	}
	// The three service names work bare as well as under `serve` (omp parity).
	if mode == "serve" || serve.IsService(mode) {
		if mode != "serve" {
			args = append([]string{mode}, args...)
		}
		os.Exit(serve.Dispatch(args, version))
	}
	if mode == "install" {
		// omp's `install <name>` is `plugin install <name>`.
		os.Exit(marketplace.Run(append([]string{"install"}, args...), os.Stdout, os.Stderr, settings.Plugins.Marketplaces))
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

	if mode == "mcps" {
		os.Exit(runMcps(args))
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
	if mode == "connect" {
		os.Exit(runConnect(args))
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
	if mode == "trust" || mode == "distrust" {
		os.Exit(runTrust(mode, args))
	}

	switch mode {
	case "version":
		fmt.Printf("xdev %s\n", version)
	case "help":
		fs.Usage()
		os.Exit(0)
	case "print":
		// Positional handling, omp parity: a leading `--` is the separator,
		// every remaining argument joins into one prompt (omp -p "A" "B"
		// sends both), and a lone --help/-h asks for usage rather than
		// being sent to the model.
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		for _, a := range args {
			if a == "--help" || a == "-h" {
				fs.Usage()
				os.Exit(0)
			}
		}
		prompt := strings.Join(args, " ")
		if startupIsInteractive(prompt, *printModeFlag, stdinIsTerminal()) {
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
		if prompt == "" && !*continueLast && *resumePrefix == "" && *forkID == "" && *fromClaude == "" && *fromCodex == "" {
			// Headless with no session flag: read the prompt from stdin.
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
		// A clean return is not necessarily a successful run: an aborted one
		// (--max-time, a refused approval, a provider failure) comes back as
		// (1, nil). Dropping the code made every aborted script run exit 0.
		if code != 0 {
			os.Exit(code)
		}
	}
}

// stdinIsTerminal reports whether stdin is an interactive TTY (as opposed
// to a pipe or file feeding a prompt). os.ModeCharDevice is not enough:
// /dev/null IS a char device, and `xdev </dev/null` must fall through to the
// stdin/usage path instead of trying to open a TUI on it.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// startupIsInteractive is the routing decision of main: with no prompt, a
// terminal asks for the TUI - with or without a session flag. The old rule
// required "no session flags" as well, so the very form the TUI-exit hint
// prints (`xdev --resume <id>`) fell into print mode with an empty prompt:
// it looked like a hang, and on a short prefix it appended an UNPROMPTED
// agent turn to the session being resumed. Keep it a pure function so the
// table below covers the rule; main passes stdinIsTerminal().
func startupIsInteractive(prompt string, forcePrint, tty bool) bool {
	return prompt == "" && !forcePrint && tty
}

