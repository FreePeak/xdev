// `xdev connect` (issue: subscription parity) turns an account the user already
// pays for into a configured provider: one row of the generated models.dev
// catalog written into models.yml, plus the key if they paste it.
//
// The list is the product here — the same ~200 hosts opencode offers — so the
// command reports state rather than just acting: what is connectable, what
// already has a credential in hand, and what a run would then use.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
)

// runConnect implements `xdev connect [--list] [--json] [<provider>] [--key K]
// [--set-default]`.
func runConnect(args []string) int {
	return connectCmd(args, os.Stdout, os.Stderr)
}

func connectCmd(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asList := fs.Bool("list", false, "print the whole catalog with its state")
	asJSON := fs.Bool("json", false, "print the listing as JSON")
	keyFlag := fs.String("key", "", "credential to store for this provider (0600 credentials.json); pass - to read it from stdin")
	setDefault := fs.Bool("set-default", false, "point models.yml's defaultModel at the connected provider")
	info := fs.Bool("info", false, "print a provider's pinned models instead of connecting")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev connect [flags] [provider]

  xdev connect                 the providers a credential is ready to use
  xdev connect --list          every provider in the catalog, with its state
  xdev connect <provider>      write that provider into models.yml
  xdev connect <provider> --key ...      ...and store the credential it needs
  xdev connect <provider> --key -        ...reading that credential from stdin
  xdev connect <provider> --info         show its pinned models
  /connect                     the same list as a picker, inside the TUI

A connected provider keeps its credential as a ${VAR} reference in models.yml
(the variable the host documents), so the config file stays safe to paste.
--key writes to credentials.json (0600) instead. Discovery is turned on for
every connected provider, so models released after this snapshot appear on
their own.

Passing a key as an argument exposes it to ps and to your shell history;
prefer the environment (xdev reads the variable the --list row names), or
--key - with a pipe:

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlagRounds(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprintln(errOut, "xdev connect: pass one provider, not several")
		return 2
	}
	if len(positional) == 0 && *setDefault {
		fmt.Fprintln(errOut, "xdev connect: --set-default needs a provider")
		return 2
	}
	cfg, cfgErr := config.LoadModelsLayered()
	if cfgErr != nil {
		fmt.Fprintln(errOut, "xdev connect: models.yml:", cfgErr)
	}
	store, storeErr := config.LoadCredentials()
	if storeErr != nil {
		fmt.Fprintln(errOut, "xdev connect: credentials.json:", storeErr)
	}
	opts := config.ConnectOptions(cfg, store)

	// --- one named provider
	if len(positional) == 1 {
		name := positional[0]
		if !config.HasConnect(name) {
			fmt.Fprintf(errOut, "xdev connect: unknown provider %q\n", name)
			if near := connectNear(name, opts); near != "" {
				fmt.Fprintf(errOut, "did you mean: %s\n", near)
			}
			fmt.Fprintf(errOut, "the full catalog: xdev connect --list\n")
			return 2
		}
		if *info {
			return connectInfo(name, out)
		}
		key := *keyFlag
		if key == "-" {
			// "-" is the private route: the credential arrives on a pipe and
			// never lands in the argument vector.
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintln(errOut, "xdev connect: reading the key from stdin:", err)
				return 1
			}
			key = strings.TrimSpace(string(b))
			if key == "" {
				fmt.Fprintln(errOut, "xdev connect: --key - read nothing from stdin")
				return 2
			}
		}
		return connectApply(name, key, *setDefault, opts, out, errOut)
	}

	// --- listings: the ready rows by default (the useful 20), everything on
	// --list; --json is for scripts and the docs build.
	if *asJSON {
		rows := opts
		if !*asList {
			rows = connectReady(opts)
		}
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	if !*asList {
		ready := connectReady(opts)
		if len(ready) == 0 {
			fmt.Fprintf(out, "no provider has a credential yet. Export the key one host reads and re-run:\n")
			for _, o := range connectFirst(opts, 8) {
				fmt.Fprintf(out, "  export %s=...   then xdev connect %s\n", o.Env, o.Name)
			}
			fmt.Fprintf(out, "\nor connect one by name: xdev connect <provider> --key ... (xdev connect --list shows all %d)\n", len(opts))
			return 0
		}
		fmt.Fprintf(out, "ready to connect (a credential is in hand):\n\n")
		connectTable(ready, out)
		fmt.Fprintf(out, "\nxdev connect <provider> writes one into models.yml. %d more in the catalog: xdev connect --list\n", len(opts)-len(ready))
		return 0
	}
	connectTable(opts, out)
	return 0
}

// connectApply writes one provider and reports what became usable.
func connectApply(name, key string, setDefault bool, opts []config.ConnectOption, out, errOut io.Writer) int {
	opt := connectFind(opts, name)
	refs, err := config.Connect(name, key)
	if err != nil {
		fmt.Fprintln(errOut, "xdev connect:", err)
		return 1
	}
	fmt.Fprintf(out, "connected %s (%s @ %s)\n", name, opt.API, opt.BaseURL)
	// Tell the user where the key came from, because that is the difference
	// between "it works" and "it works in this shell".
	if key != "" {
		fmt.Fprintf(out, "  key stored in %s\n", config.CredentialsPath())
	} else if opt.Env != "" {
		if opt.Ready && strings.HasPrefix(opt.ReadyFrom, "env ") {
			fmt.Fprintf(out, "  using %s from the environment\n", strings.TrimPrefix(opt.ReadyFrom, "env "))
		} else if opt.Ready {
			fmt.Fprintf(out, "  using the %s already configured\n", opt.ReadyFrom)
		} else {
			fmt.Fprintf(out, "  models.yml references ${%s} — export it (or put it in %s) before the first run\n", opt.Env, filepath.Join(config.DataDir(), ".env"))
		}
	}
	if len(refs) > 0 {
		fmt.Fprintf(out, "  %d models pinned; try: xdev print -model %s \"hello\"\n", len(refs), refs[0])
	}
	if setDefault {
		if err := connectSetDefault(name, out); err != nil {
			fmt.Fprintln(errOut, "xdev connect:", err)
			return 1
		}
	}
	return 0
}

// connectInfo lists one provider's pinned models: the ids are usually the thing
// a user is deciding between.
func connectInfo(name string, out io.Writer) int {
	refs := config.ConnectModelRefs(name)
	fmt.Fprintf(out, "%s: %d pinned models\n", name, len(refs))
	for _, r := range refs {
		fmt.Fprintf(out, "  %s\n", r)
	}
	fmt.Fprintf(out, "\nmodels released after this snapshot appear through discovery; xdev models shows the merged catalog.\n")
	return 0
}

// connectSetDefault points models.yml's defaultModel at the connected
// provider. --set-default is the decision, so an existing default is replaced
// rather than silently kept — but named, because it is usually a model the user
// chose deliberately and would like to know they no longer have.
func connectSetDefault(name string, out io.Writer) error {
	ref := config.ConnectDefaultRef(name)
	if ref == "" {
		return fmt.Errorf("no models pinned for %s to default to", name)
	}
	path := filepath.Join(config.DataDir(), "models.yml")
	cfg, err := config.LoadModels(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if cfg != nil && strings.TrimSpace(cfg.DefaultModel) != "" && cfg.DefaultModel != ref {
		fmt.Fprintf(out, "  default model: %s (was %s)\n", ref, cfg.DefaultModel)
		return config.SetModelsDefault(path, ref)
	}
	fmt.Fprintf(out, "  default model: %s\n", ref)
	return config.SetModelsDefault(path, ref)
}

// connectTable renders the listing. Two columns of state, because the question
// a user scans the list for is "can I use this" and "is it already set up".
func connectTable(opts []config.ConnectOption, out io.Writer) {
	// The credential column is spelled out rather than truncated: it names the
	// variable to export, which is the one thing a user copies from this list.
	// The wire kind is left to --json and --info, since almost every row is
	// openai-completions and the column was mostly noise.
	fmt.Fprintf(out, "  %-24s %-6s %-26s %-20s %s\n", "PROVIDER", "MODELS", "CREDENTIAL", "STATE", "ENDPOINT")
	for _, o := range opts {
		cred := "none"
		if o.Env != "" {
			cred = "$" + o.Env
		}
		var state string
		switch {
		case o.Connected && o.Ready:
			state = "✓ ready"
		case o.Connected:
			state = "configured"
		case o.Ready:
			state = "ready"
		case o.Plan:
			state = "plan"
		}
		fmt.Fprintf(out, "  %-24s %-6d %-26s %-20s %s\n",
			truncate(o.Name, 24), o.Models, truncate(cred, 26), state, o.BaseURL)
	}
	fmt.Fprintf(out, "\nconfigured = models.yml already declares it; ready = a credential resolves for it now.\n")
	fmt.Fprintf(out, "xdev connect <provider> --info shows a row's models; --json carries the wire kind and credential source.\n")
}

// connectReady keeps the rows a user could connect without fetching a key.
func connectReady(opts []config.ConnectOption) []config.ConnectOption {
	out := make([]config.ConnectOption, 0, 8)
	for _, o := range opts {
		if o.Ready {
			out = append(out, o)
		}
	}
	return out
}

// connectFirst keeps the n best-known rows for the empty-state hint: the hosts
// a coding agent actually reaches for, not the alphabetically first. Rows whose
// host publishes no variable are skipped — the hint says "export", and there is
// nothing to export for those (connect them with --key instead).
func connectFirst(opts []config.ConnectOption, n int) []config.ConnectOption {
	rank := func(o config.ConnectOption) int {
		switch o.Name {
		case "anthropic", "openai", "google", "github-copilot", "opencode", "opencode-go":
			return 0
		default:
			return 1
		}
	}
	filtered := make([]config.ConnectOption, 0, len(opts))
	for _, o := range opts {
		if o.Env != "" {
			filtered = append(filtered, o)
		}
	}
	sorted := filtered
	sort.SliceStable(sorted, func(i, j int) bool {
		if rank(sorted[i]) != rank(sorted[j]) {
			return rank(sorted[i]) < rank(sorted[j])
		}
		return sorted[i].Name < sorted[j].Name
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

func connectFind(opts []config.ConnectOption, name string) config.ConnectOption {
	for _, o := range opts {
		if o.Name == name {
			return o
		}
	}
	return config.ConnectOption{Name: name}
}

// connectNear is the typo hint: one substring pass over the catalog names,
// because a wrong provider name is the likeliest error at this prompt.
func connectNear(query string, opts []config.ConnectOption) string {
	q := strings.ToLower(query)
	var hits []string
	for _, o := range opts {
		n := strings.ToLower(o.Name)
		if strings.Contains(n, q) || strings.Contains(q, n) {
			hits = append(hits, o.Name)
		}
	}
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 4 {
		hits = hits[:4]
	}
	return strings.Join(hits, ", ")
}
