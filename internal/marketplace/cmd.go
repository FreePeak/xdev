package marketplace

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Run implements `xdev plugin <sub>`: the Claude-compatible marketplace and
// plugin-manager CLI. It returns a process exit code (2 for usage errors, 1
// for failures). marketplaces is the settings layer's plugins.marketplaces;
// -marketplace adds locations for one run.
func Run(args []string, stdout, stderr io.Writer, marketplaces []string) int {
	sub := "list"
	subArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, subArgs = args[0], args[1:]
	}
	fs := flag.NewFlagSet("plugin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var extra repeatable
	fs.Var(&extra, "marketplace", "extra catalog location for this run (repeatable)")
	force := fs.Bool("force", false, "install: replace an existing plugin copy")
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	// flag.Parse stops at the first positional, so parse in rounds and keep
	// the positionals: `plugin search foo -marketplace X` must honor the
	// flag rather than silently see zero catalogs.
	var rest []string
	for remaining := subArgs; ; {
		if err := fs.Parse(remaining); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		rest = append(rest, fs.Arg(0))
		remaining = fs.Args()[1:]
	}

	opts := Options{Marketplaces: append(append([]string{}, marketplaces...), extra...), Force: *force}
	ctx := context.Background()
	warn := func(warns []string) {
		for _, w := range warns {
			fmt.Fprintf(stderr, "xdev plugin: %s\n", w)
		}
	}
	need := func(what string) (string, bool) {
		if len(rest) == 0 {
			fmt.Fprintf(stderr, "xdev plugin %s: %s required\n", sub, what)
			return "", false
		}
		return rest[0], true
	}

	switch sub {
	case "list":
		return cmdList(ctx, stdout, stderr, opts)

	case "search":
		q, ok := need("<query>")
		if !ok {
			return 2
		}
		return cmdSearch(ctx, stdout, stderr, opts, q)

	case "install":
		ref, ok := need("<name>[@version]")
		if !ok {
			return 2
		}
		inst, warns, err := Install(ctx, ref, opts)
		warn(warns)
		if err != nil {
			fmt.Fprintln(stderr, "xdev plugin install:", err)
			return 1
		}
		fmt.Fprintf(stdout, "installed %s %s → %s\n", inst.Name, orDash(inst.Version), inst.Path)
		fmt.Fprintf(stdout, "  revision  %s (pinned)\n", orDash(inst.Revision))
		printDirs(stdout, "  ", inst.Dirs)
		return 0

	case "remove":
		name, ok := need("<name>")
		if !ok {
			return 2
		}
		name, _ = SplitRef(name)
		inst, err := Remove(name)
		if err != nil {
			fmt.Fprintln(stderr, "xdev plugin remove:", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed %s (%s) — its discovery roots are gone\n", inst.Name, inst.Path)
		return 0

	case "info":
		name, ok := need("<name>")
		if !ok {
			return 2
		}
		return cmdInfo(ctx, stdout, stderr, opts, name)

	default:
		fmt.Fprintf(stderr, "xdev plugin: unknown subcommand %q (want list|search|install|remove|info)\n", sub)
		fmt.Fprint(stderr, usage)
		return 2
	}
}

const usage = `usage: xdev plugin [list|search|install|remove|info] [args]

  list                              installed plugins, then the catalogs' available entries
  search <query>                    catalog entries matching name/description/marketplace
  install [-force] <name>[@version] clone a plugin, verify its manifest and pin the revision
  remove <name>                     uninstall a plugin and drop its discovery roots
  info <name>                       details for an installed plugin (or a catalog entry)

Flags (anywhere in the argument list):
  -marketplace <loc>   extra catalog location for this run (repeatable; path or git URL)
  -force               install: replace an existing plugin copy

Catalogs come from settings plugins.marketplaces. Installed plugins live in
<dataDir>/plugins/<name> and contribute commands/skills/agents/hooks at the
LOWEST discovery priority, so they never shadow your own content.
`

// repeatable implements flag.Value for -marketplace.
type repeatable []string

func (r *repeatable) String() string     { return strings.Join(*r, ",") }
func (r *repeatable) Set(v string) error { *r = append(*r, v); return nil }

func cmdList(ctx context.Context, stdout, stderr io.Writer, opts Options) int {
	insts := InstalledPlugins()
	fmt.Fprintf(stdout, "installed (%d)\n", len(insts))
	if len(insts) == 0 {
		fmt.Fprintln(stdout, "  none — install one with: xdev plugin install <name>")
	} else {
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tVERSION\tREVISION\tMARKETPLACE\tPATH")
		for _, p := range insts {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, orDash(p.Version), shortRev(p.Revision), orDash(p.Marketplace), p.Path)
		}
		_ = w.Flush()
	}

	catalogs, warns := Catalogs(ctx, opts.Marketplaces)
	avail := Available(catalogs)
	fmt.Fprintf(stdout, "\navailable (%d) from %d marketplace(s)\n", len(avail), len(catalogs))
	for _, c := range catalogs {
		fmt.Fprintf(stdout, "  %s @ %s\n", c.Name, c.Location)
	}
	if len(catalogs) == 0 {
		fmt.Fprintln(stdout, "  no catalogs — set plugins.marketplaces or pass -marketplace <path|git-url>")
	} else {
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tVERSION\tMARKETPLACE\tDESCRIPTION")
		for _, e := range avail {
			desc := e.Description
			if desc == "" {
				desc = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Name, orDash(e.Version), e.Marketplace, desc)
		}
		_ = w.Flush()
	}
	for _, wr := range warns {
		fmt.Fprintf(stderr, "xdev plugin: %s\n", wr)
	}
	return 0
}

func cmdSearch(ctx context.Context, stdout, stderr io.Writer, opts Options, q string) int {
	catalogs, warns := Catalogs(ctx, opts.Marketplaces)
	hits := Search(catalogs, q)
	if len(hits) == 0 {
		fmt.Fprintf(stdout, "no plugin matches %q in %d marketplace(s)\n", q, len(catalogs))
	} else {
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tVERSION\tMARKETPLACE\tREVISION\tDESCRIPTION")
		for _, e := range hits {
			desc := e.Description
			if desc == "" {
				desc = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Name, orDash(e.Version), e.Marketplace, orDash(e.Revision), desc)
		}
		_ = w.Flush()
	}
	for _, wr := range warns {
		fmt.Fprintf(stderr, "xdev plugin: %s\n", wr)
	}
	return 0
}

func cmdInfo(ctx context.Context, stdout, stderr io.Writer, opts Options, ref string) int {
	name, _ := SplitRef(ref)
	if err := validateName(name); err != nil {
		fmt.Fprintln(stderr, "xdev plugin info:", err)
		return 2
	}
	if inst, ok := lookupInstalled(name); ok {
		fmt.Fprintf(stdout, "name:        %s\n", inst.Name)
		fmt.Fprintf(stdout, "version:     %s\n", orDash(inst.Version))
		fmt.Fprintf(stdout, "description: %s\n", orDash(inst.Description))
		fmt.Fprintf(stdout, "marketplace: %s\n", orDash(inst.Marketplace))
		fmt.Fprintf(stdout, "source:      %s\n", inst.Source)
		fmt.Fprintf(stdout, "revision:    %s (pinned at install)\n", orDash(inst.Revision))
		fmt.Fprintf(stdout, "path:        %s\n", inst.Path)
		fmt.Fprintf(stdout, "installed:   %s\n", inst.InstalledAt.Format(time.RFC3339))
		printDirs(stdout, "", inst.Dirs)
		return 0
	}
	catalogs, warns := Catalogs(ctx, opts.Marketplaces)
	if e, ok := findEntry(catalogs, name, ""); ok {
		fmt.Fprintf(stdout, "name:        %s\n", e.Name)
		fmt.Fprintf(stdout, "version:     %s\n", orDash(e.Version))
		fmt.Fprintf(stdout, "description: %s\n", orDash(e.Description))
		fmt.Fprintf(stdout, "marketplace: %s (%s)\n", e.Marketplace, e.Location)
		fmt.Fprintf(stdout, "source:      %s\n", e.Source)
		fmt.Fprintf(stdout, "revision:    %s\n", orDash(e.Revision))
		fmt.Fprintln(stdout, "status:      not installed")
		return 0
	}
	for _, wr := range warns {
		fmt.Fprintf(stderr, "xdev plugin: %s\n", wr)
	}
	fmt.Fprintf(stderr, "xdev plugin info: plugin %q is neither installed nor in %d marketplace(s)\n", name, len(catalogs))
	return 1
}

func lookupInstalled(name string) (Installed, bool) {
	reg, err := Load()
	if err != nil {
		return Installed{}, false
	}
	return reg.Find(name)
}

func printDirs(w io.Writer, indent string, d Dirs) {
	for _, row := range []struct {
		kind string
		dirs []string
	}{
		{"commands", d.Commands},
		{"skills", d.Skills},
		{"agents", d.Agents},
		{"hooks", d.Hooks},
	} {
		if len(row.dirs) == 0 {
			fmt.Fprintf(w, "%s%-10s (none)\n", indent, row.kind)
			continue
		}
		fmt.Fprintf(w, "%s%-10s %s\n", indent, row.kind, strings.Join(row.dirs, ", "))
	}
}

// shortRev keeps the table readable; the full value is in `plugin info`.
func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return orDash(rev)
}
