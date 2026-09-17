package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
)

// runModels implements `xdev models` (issue #34): list/search/refresh the
// resolved catalog — the same merge of models.yml pinning and live discovery a
// run resolves through (resolveModel → providerModels), so what the CLI prints
// is exactly what a run can use.
func runModels(args []string) int {
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		// A broken models.yml still lists whatever parsed: refusing to print
		// anything would hide the problem behind a second failure.
		fmt.Fprintf(os.Stderr, "xdev models: models.yml: %v\n", err)
		cfg = &config.Config{}
	}
	return modelsCmd(args, cfg, lastSettings(), os.Stdout, os.Stderr)
}

// modelRow is one line of the catalog (and one element of --json).
type modelRow struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Name          string `json:"name,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxTokens     int    `json:"maxTokens,omitempty"`
	Source        string `json:"source"` // pinned | discovered
	// Account is the masked credential source the model runs on.
	Account string `json:"account,omitempty"`
	// Default marks what a run uses without -model.
	Default bool `json:"default,omitempty"`
}

func modelsCmd(args []string, cfg *config.Config, settings *config.Settings, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "print the catalog as JSON")
	refresh := fs.Bool("refresh", false, "re-run live discovery before printing (ignore this process's cache)")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev models [flags] [query]

  xdev models                 print the resolved catalog
  xdev models free            case-insensitive match on provider/model/name
  xdev models --refresh       re-discover from the providers' list endpoints

The catalog is what a run resolves through: models.yml pinning merged with live
discovery (providers with a discovery: block).

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlagRounds(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprintln(errOut, "xdev models: pass one query, not several")
		return 2
	}
	query := ""
	if len(positional) == 1 {
		query = strings.ToLower(strings.TrimSpace(positional[0]))
	}

	if *refresh {
		// providerModelCache is package-level (print.go): clearing it here is
		// what makes the pass below re-ask the servers instead of replaying
		// whatever this process already resolved.
		clear(providerModelCache)
	}

	rows := catalogRows(cfg, settings)
	if query != "" {
		rows = modelsFilter(rows, query)
	}

	if *asJSON {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	fmt.Fprint(out, modelsRender(rows, cfg, settings, *refresh, query))
	return 0
}

// catalogRows resolves every provider's models through the shared helpers
// (providerModels merges pinned + discovered, cached per process) and
// attributes accounts and the default marker.
func catalogRows(cfg *config.Config, settings *config.Settings) []modelRow {
	if cfg == nil {
		return nil
	}
	defaultRef := cfg.DefaultModelRef()
	if settings != nil && strings.TrimSpace(settings.DefaultModel) != "" {
		defaultRef = strings.TrimSpace(settings.DefaultModel)
	}

	var rows []modelRow
	for _, name := range providerKeys(cfg) {
		pc := cfg.Providers[name]
		if pc == nil {
			continue
		}
		account := modelsAccount(pc, name)
		for _, m := range providerModels(name, pc) {
			ref := name + "/" + m.ID
			rows = append(rows, modelRow{
				Provider:      name,
				Model:         m.ID,
				Name:          m.Name,
				Reasoning:     m.Reasoning,
				ContextWindow: m.ContextWindow,
				MaxTokens:     m.MaxTokens,
				Source:        modelsSource(pc, m.ID),
				Account:       account,
				Default:       ref == defaultRef,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].Model < rows[j].Model
	})
	return rows
}

// modelsFilter keeps the rows a query names (provider, model, or display
// name — an empty result is information, not an error).
func modelsFilter(rows []modelRow, query string) []modelRow {
	out := make([]modelRow, 0, len(rows))
	for _, r := range rows {
		haystack := strings.ToLower(r.Provider + "/" + r.Model + " " + r.Name)
		if strings.Contains(haystack, query) {
			out = append(out, r)
		}
	}
	return out
}

// modelsSource splits pinned from discovered: an id models.yml declared is
// pinned, anything providerModels added on top came from the server.
func modelsSource(pc *config.ProviderConfig, id string) string {
	for _, m := range pc.Models {
		if m.ID == id {
			return "pinned"
		}
	}
	return "discovered"
}

// modelsAccount masks a provider's credential source for the account column.
func modelsAccount(pc *config.ProviderConfig, provider string) string {
	if strings.EqualFold(strings.TrimSpace(pc.Auth), "none") {
		return "none (local server)"
	}
	if key := strings.TrimSpace(pc.APIKey); key != "" {
		if strings.HasPrefix(key, "${") && strings.HasSuffix(key, "}") {
			// An env reference: name the variable, never a value.
			return "env " + strings.TrimSuffix(strings.TrimPrefix(key, "${"), "}")
		}
		return "models.yml key " + config.Redact(config.Resolve(key))
	}
	if n := len(pc.APIKeys); n > 0 {
		return "apiKeys x" + strconv.Itoa(n)
	}
	if hasStoredCredential(provider) {
		return "credentials.json"
	}
	return "unconfigured"
}

func modelsRender(rows []modelRow, cfg *config.Config, settings *config.Settings, refreshed bool, query string) string {
	var b strings.Builder
	switch {
	case len(rows) == 0 && query != "":
		fmt.Fprintf(&b, "no model matches %q\n", query)
	case len(rows) == 0:
		b.WriteString("no models configured: add ~/.xdev/agent/models.yml (or run `xdev setup`)\n")
	default:
		b.WriteString("MODELS (resolved catalog)\n\n")
		fmt.Fprintf(&b, "  %-12s %-22s %9s %8s %-11s %s\n", "PROVIDER", "MODEL", "WINDOW", "MAXOUT", "SOURCE", "ACCOUNT")
		for _, r := range rows {
			marker := ""
			if r.Default {
				marker = " *"
			}
			reason := ""
			if r.Reasoning {
				reason = "~"
			}
			fmt.Fprintf(&b, "  %-12s %-22s %9s %8s %-11s %s%s\n",
				truncate(r.Provider, 12), truncate(r.Model+reason, 22),
				modelsNum(r.ContextWindow), modelsNum(r.MaxTokens), r.Source,
				r.Account, marker)
		}
		pinned, discovered := 0, 0
		for _, r := range rows {
			if r.Source == "pinned" {
				pinned++
			} else {
				discovered++
			}
		}
		fmt.Fprintf(&b, "\n  %d %s across %d %s (%d pinned, %d discovered)\n",
			len(rows), modelsPlural(len(rows), "model"), modelsProviders(rows), modelsPlural(modelsProviders(rows), "provider"), pinned, discovered)
		b.WriteString("  * default (what a run uses without -model); ~ reasoning model\n")
	}
	if refreshed {
		n := modelsDiscoveryProviders(cfg)
		if n == 0 {
			b.WriteString("\nrefreshed: no provider declares a discovery: block (nothing to re-discover)\n")
		} else {
			fmt.Fprintf(&b, "\nrefreshed: re-discovered %d provider(s) with a discovery: block\n", n)
		}
	}
	if settings != nil && len(settings.DisabledProviders) > 0 {
		fmt.Fprintf(&b, "disabledProviders: %s\n", strings.Join(settings.DisabledProviders, ","))
	}
	return b.String()
}

func modelsProviders(rows []modelRow) int {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Provider] = true
	}
	return len(seen)
}

// modelsDiscoveryProviders counts the providers a --refresh actually re-asks.
func modelsDiscoveryProviders(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	n := 0
	for _, name := range providerKeys(cfg) {
		if pc := cfg.Providers[name]; pc != nil && pc.Discovery != nil {
			n++
		}
	}
	return n
}

// modelsNum renders a token count column ("-" when the provider never said).
func modelsNum(n int) string {
	if n <= 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// modelsPlural keeps the summary line readable for a single-row catalog.
func modelsPlural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
