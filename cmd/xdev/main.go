// Command xdev is the CLI entry: print mode (MVP), tui (M4), rpc (M6).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/memlimit"
)

var version = "0.1.0-dev"

func main() {
	limit := memlimit.Apply()

	fs := flag.NewFlagSet("xdev", flag.ContinueOnError)
	model := fs.String("model", "", "model to use (provider/model)")
	continueLast := fs.Bool("continue", false, "continue the most recent session in this directory")
	systemPrompt := fs.String("system-prompt", "", "replace the built-in system prompt")
	appendSystemPrompt := fs.String("append-system-prompt", "", "append to the system prompt")
	themeName := fs.String("theme", "", "TUI theme: groknight | grokday (default: auto)")
	maxTurns := fs.Int("max-turns", 0, "max agent turns per run (0 = default 200)")
	maxTokens := fs.Int("max-tokens", 0, "assistant output token cap (0 = provider default)")
	verbose := fs.Bool("verbose", false, "log to stderr")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `xdev %s — lightweight coding agent (Go)

  xdev                         interactive TUI (bare invocation, TTY)
  xdev [flags] "prompt"        one-shot print run
  xdev print [flags] "prompt"  same as above
  xdev tui                     interactive TUI (Grok-CLI look)

Flags:
`, version)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory limit: %d bytes (XDEV_MEMLIMIT to override)\n", limit)
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *verbose {
		logx.Enable(logx.LevelDebug)
	}

	args := fs.Args()
	mode := "print"
	if len(args) > 0 && (args[0] == "print" || args[0] == "tui" || args[0] == "version") {
		mode, args = args[0], args[1:]
	}
	if mode == "tui" {
		code, err := runTUI(printOptions{
			Model:        *model,
			ContinueLast: *continueLast,
			SystemPrompt: *systemPrompt,
			AppendSystem: *appendSystemPrompt,
			MaxTokens:    *maxTokens,
			MaxTurns:     *maxTurns,
		}, *themeName)
		if err != nil {
			fmt.Fprintln(os.Stderr, "xdev:", err)
			os.Exit(2)
		}
		os.Exit(code)
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
			SystemPrompt: *systemPrompt,
			AppendSystem: *appendSystemPrompt,
			MaxTurns:     *maxTurns,
			MaxTokens:    *maxTokens,
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
