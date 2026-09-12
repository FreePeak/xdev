package lsp

import (
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// ConfigCommand implements `xdev lsp-config [list|validate]`: the configured
// language servers and the binary each resolves to. validate exits non-zero
// when an enabled server is unusable, so it works as a setup check.
func ConfigCommand(args []string, stdout, stderr io.Writer, s *config.Settings) int {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list", "validate":
	default:
		fmt.Fprintln(stderr, "xdev lsp-config: unknown subcommand", sub, "(want list|validate)")
		return 2
	}

	cfg := ConfigFromSettings(s)
	// A malformed duration silently falls back to the default in the session;
	// the setup check is where it must be visible.
	badDuration := ""
	if s != nil && s.LSP != nil {
		if raw := strings.TrimSpace(s.LSP.IdleTimeout); raw != "" {
			if d, err := time.ParseDuration(raw); err != nil || d <= 0 {
				badDuration = raw
			}
		}
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)

	mode := "lazy (start on first use)"
	if !cfg.Lazy {
		mode = "eager (lsp.lazy: false)"
	}
	fmt.Fprintf(stdout, "lsp: %s, idle timeout %s\n", mode, cfg.IdleTimeout)
	if badDuration != "" {
		fmt.Fprintf(stderr, "xdev lsp-config: lsp.idleTimeout %q is not a positive duration (using %s)\n", badDuration, cfg.IdleTimeout)
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "language\tcommand\tresolved\tfile types\troot markers")
	unusable := 0
	for _, n := range names {
		sp := cfg.Servers[n]
		cmdline := strings.TrimSpace(sp.Command + " " + strings.Join(sp.Args, " "))
		resolved := ""
		switch {
		case sp.Disabled:
			resolved = "disabled"
		case sp.Command == "":
			resolved = "no command configured"
			unusable++
		default:
			p, err := exec.LookPath(sp.Command)
			if err != nil {
				resolved = "not found on PATH"
				unusable++
			} else {
				resolved = p
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n, cmdline, resolved,
			"."+strings.Join(sp.FileTypes, " ."), strings.Join(sp.RootMarkers, ", "))
	}
	_ = w.Flush()

	if sub == "validate" {
		if unusable == 0 && badDuration == "" {
			fmt.Fprintf(stdout, "ok: %d server(s) configured, all binaries resolve\n", len(names))
			return 0
		}
		if unusable > 0 {
			fmt.Fprintf(stderr, "xdev lsp-config: %d server(s) unusable — install the binary or set lsp.servers.<language>.command in %s\n",
				unusable, config.GlobalSettingsPath())
		}
		return 1
	}
	return 0
}
