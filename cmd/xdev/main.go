// Command xdev is the CLI entry: print mode (MVP), tui (M4), rpc (M6).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/memlimit"
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

func main() {
	fs := flag.NewFlagSet("xdev", flag.ContinueOnError)
	configOverlays := repeatable{}
	fs.Var(&configOverlays, "config", "extra config file layered over the defaults (repeatable)")
	model := fs.String("model", "", "model to use (provider/model)")
	continueLast := fs.Bool("continue", false, "continue the most recent session in this directory")
	resumePrefix := fs.String("resume", "", "resume a session by id prefix (e.g. -resume 01a0)")
	systemPrompt := fs.String("system-prompt", "", "replace the built-in system prompt")
	appendSystemPrompt := fs.String("append-system-prompt", "", "append to the system prompt")
	themeName := fs.String("theme", "", "TUI theme: groknight | grokday (default: auto)")
	maxTurns := fs.Int("max-turns", 0, "max agent turns per run (0 = default 200)")
	maxTokens := fs.Int("max-tokens", 0, "assistant output token cap (0 = provider default)")
	apiKeyValue := fs.String("api-key", "", "credential for this run only (never persisted)")
	verbose := fs.Bool("verbose", false, "log to stderr")
	prewalkFlag := fs.Bool("prewalk", false, "one-shot model handoff: switch to the prewalk target after the first successful edit/write")
	planFlag := fs.Bool("plan", false, "plan mode: read-only research; the run proposes a plan before implementing")
	prewalkInto := fs.String("prewalk-into", "@smol", "prewalk target: model ref or @role (default @smol)")
	noRules := fs.Bool("no-rules", false, "disable rules discovery (.omp/rules, RULES.md, third-party rulebooks)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `xdev %s — lightweight coding agent (Go)

  xdev                         interactive TUI (bare invocation, TTY)
  xdev [flags] "prompt"        one-shot print run
  xdev print [flags] "prompt"  same as above
  xdev tui                     interactive TUI (Grok-CLI look)
  xdev config <sub>            settings: list | get K | set K V | reset K | path

Flags:
`, version)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory limit: %d bytes (XDEV_MEMLIMIT to override)\n", appliedLimit)
	}
	// NOTE: Go's flag package stops at the first positional arg, so flags
	// must precede the subcommand: `xdev -continue tui`, not `xdev tui -continue`.
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *verbose {
		logx.Enable(logx.LevelDebug)
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
	noRulesFlag = *noRules
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

	args := fs.Args()
	mode := "print"
	if len(args) > 0 && (args[0] == "print" || args[0] == "tui" || args[0] == "rpc" || args[0] == "config" || args[0] == "login" || args[0] == "logout" || args[0] == "version") {
		mode, args = args[0], args[1:]
	}
	if mode == "tui" {
		code, err := runTUI(printOptions{
			Model:        *model,
			ContinueLast: *continueLast,
			ResumePrefix: *resumePrefix,
			SystemPrompt: *systemPrompt,
			AppendSystem: *appendSystemPrompt,
			MaxTokens:    *maxTokens,
			MaxTurns:     *maxTurns,
			Prewalk:      *prewalkFlag,
			PrewalkInto:  *prewalkInto,
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
			MaxTurns:     *maxTurns,
			MaxTokens:    *maxTokens,
		}
		code, err := runRPC(opts)
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

	if mode == "config" {
		os.Exit(runConfig(args, settings))
	}

	switch mode {
	case "version":
		fmt.Printf("xdev %s\n", version)
	case "print":
		prompt := ""
		if len(args) > 0 {
			prompt = args[0]
		}
		if prompt == "" && !*continueLast {
			if stdinIsTerminal() {
				// Bare interactive invocation: open the TUI.
				code, err := runTUI(printOptions{
					Model:        *model,
					ContinueLast: *continueLast,
					SystemPrompt: *systemPrompt,
					AppendSystem: *appendSystemPrompt,
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
		if prompt == "" && !*continueLast {
			fs.Usage()
			os.Exit(2)
		}
		opts := printOptions{
			Model:        *model,
			ContinueLast: *continueLast,
			ResumePrefix: *resumePrefix,
			SystemPrompt: *systemPrompt,
			AppendSystem: *appendSystemPrompt,
			MaxTurns:     *maxTurns,
			MaxTokens:    *maxTokens,
			Prewalk:      *prewalkFlag,
			PrewalkInto:  *prewalkInto,
			Plan:         *planFlag,
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
