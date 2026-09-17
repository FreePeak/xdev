package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/stats"
)

// usageLimitsNote is the honest answer to "how much quota is left": nothing
// in xdev knows. It is printed once, after the provider table, instead of a
// zero or a guess next to every account.
//
// ponytail: there is no usage/limits transport in internal/ai — Provider
// speaks chat/completions only, and no adapter records the rate-limit headers
// that a few vendors send. Ceiling: the account column can say WHO is
// configured and WHAT was spent locally, never what remains upstream. Upgrade
// path: an optional `usageUrl` per provider in models.yml plus a
// `Provider.Usage(ctx) (Usage, error)` method implemented by the adapters,
// with usageCmd rendering it when present and keeping this line otherwise.
const usageLimitsNote = `  no provider in this table exposes a usage or limits endpoint that xdev
  wires up, and xdev records no rate-limit response headers, so there is no
  remaining-quota number to print for any of them. A plan and its limits are
  visible only in that provider's own console or API dashboard.
`

// usageColumn widths fix the provider table; every cell is truncated, so a
// long base URL cannot wrap the row it belongs to.
const (
	usageColProvider = 18
	usageColAPI      = 22
	usageColBaseURL  = 32
	usageColAuth     = 9
)

// runUsage implements `xdev usage` (issue #34): the accounts xdev can spend
// on, the credential each one resolves to, and the usage the local session
// store observed.
func runUsage(args []string) int {
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		// A broken models.yml must not hide the local half of the report.
		// The error is named rather than swallowed so an unreadable file
		// stays distinguishable from "no providers configured".
		fmt.Fprintln(os.Stderr, "xdev usage: models.yml:", err)
		cfg = &config.Config{}
	}
	// The window is parsed here, not inside usageCmd, because it has to be
	// baked into the scan closure. usageCmd still owns the flag and the
	// message for a bad value, so an unparseable --since fails there before
	// this closure is ever called.
	since, _ := usageSinceArg(args)
	scan := func() (*stats.Report, error) {
		return stats.Scan(stats.Options{DataDir: config.DataDir(), Since: since, NoRollup: true})
	}
	return usageCmd(args, cfg, lastSettings(), scan, os.Stdout, os.Stderr)
}

// usageCmd renders the per-provider account table plus the locally observed
// usage. scan is injected so the caller — main in production, a table of
// fixed numbers in tests — owns how the session store is read.
func usageCmd(args []string, cfg *config.Config, settings *config.Settings, scan func() (*stats.Report, error), out, errOut io.Writer) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "print the whole report as one JSON object")
	only := fs.String("provider", "", "report one provider only")
	since := fs.String("since", "", "observed-usage window (a duration like 168h, or a date like 2026-09-01)")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev usage [flags]

  xdev usage                      accounts, credential sources, local usage
  xdev usage --provider onegw     one provider only
  xdev usage --json               the same report as one JSON object

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(errOut, `
The provider table describes what models.yml declares; the observed section
comes from the local session store. No provider exposes a usage or limits
endpoint here, and cost is what the providers reported — not a bill.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintln(errOut, "xdev usage: unexpected argument", rest[0])
		return 2
	}
	if v := strings.TrimSpace(*since); v != "" {
		if _, err := usageParseSince(v); err != nil {
			fmt.Fprintln(errOut, "xdev usage:", err)
			return 2
		}
	}

	if cfg == nil {
		cfg = &config.Config{}
	}
	names := providerKeys(cfg)
	if p := strings.TrimSpace(*only); p != "" {
		if _, ok := cfg.Providers[p]; !ok {
			fmt.Fprintf(errOut, "xdev usage: unknown provider %q (have: %v)\n", p, names)
			return 2
		}
		names = []string{p}
	}

	// One read of the credential store covers the whole table: the masked
	// value shown next to a source is the stored secret's shape, and
	// hasStoredCredential (print.go) stays the single definition of "a login
	// is stored for this provider".
	store, err := config.LoadCredentials()
	if err != nil {
		fmt.Fprintln(errOut, "xdev usage:", err)
		store = config.CredentialStore{}
	}
	report := usageReport{Providers: make([]usageProvider, 0, len(names))}
	for _, name := range names {
		report.Providers = append(report.Providers, usageProviderInfo(name, cfg.Providers[name], settings, store))
	}

	rep, err := scan()
	if err != nil {
		// The account half is cheap and already built, but a report that
		// silently dropped the usage half would read as "nothing was spent":
		// fail instead of printing a misleading table.
		fmt.Fprintln(errOut, "xdev usage:", err)
		return 1
	}
	report.Observed = usageObserved{
		DataDir: rep.DataDir,
		Since:   strings.TrimSpace(*since),
		Totals:  rep.Totals,
		Models:  rep.Models,
	}

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(errOut, "xdev usage:", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(out, usageText(report))
	return 0
}

// usageReport is the whole `xdev usage` answer. Both views render this value,
// so the text and JSON output cannot drift apart.
type usageReport struct {
	Providers []usageProvider `json:"providers"`
	Observed  usageObserved   `json:"observed"`
}

// usageProvider is one account: what models.yml declares, and the credential
// the chain would hand the wire.
type usageProvider struct {
	Name    string `json:"name"`
	API     string `json:"api"`
	BaseURL string `json:"baseUrl"`
	Auth    string `json:"auth"`
	// Account names where the credential comes from; Masked is
	// config.Redact of the credential itself, never the secret.
	Account string `json:"account"`
	Masked  string `json:"masked,omitempty"`
	// Models names the settings-level bindings this provider backs: the
	// run model (defaultModel) and, when set, the reviewer (advisorModel).
	Models   []string `json:"models,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
	OAuth    bool     `json:"oauth,omitempty"`
}

// usageObserved is the local telemetry half: the stats scan, unfiltered.
type usageObserved struct {
	DataDir string            `json:"dataDir,omitempty"`
	Since   string            `json:"since,omitempty"`
	Totals  stats.Totals      `json:"totals"`
	Models  []stats.ModelStat `json:"models"`
}

// usageProviderInfo resolves one provider row. The account source follows
// config.ResolveCredential's chain (models.yml → stored login → env) so the
// report names the same credential a run would use, with one addition:
// an unexpanded ${VAR}. LoadModels expands those while parsing models.yml,
// but a config built in process (extension register_provider, tests) can
// still carry the literal reference.
//
// The base URL is masked too: https://user:pass@host carries a credential
// that never passes through the credential chain.
func usageProviderInfo(name string, pc *config.ProviderConfig, settings *config.Settings, store config.CredentialStore) usageProvider {
	if pc == nil {
		pc = &config.ProviderConfig{}
	}
	p := usageProvider{
		Name:     name,
		API:      usageOrDefault(strings.TrimSpace(pc.API), "(api default)"),
		BaseURL:  usageRedactURL(pc.BaseURL),
		Auth:     usageOrDefault(strings.TrimSpace(pc.Auth), "api_key"),
		Models:   usageModelsFor(settings, name),
		Disabled: settings.ProviderDisabled(name),
		OAuth:    pc.OAuth != nil,
	}
	p.Account, p.Masked = usageAccount(name, pc, p.Auth, store)
	return p
}

// usageAccount is the ACCOUNT column: which source supplies this provider's
// credential, plus its masked shape when xdev can see the value itself.
func usageAccount(name string, pc *config.ProviderConfig, auth string, store config.CredentialStore) (label, masked string) {
	if auth == "none" {
		// A local server (ollama, lm-studio) is legitimately keyless; saying
		// "no credential" beats an empty cell that reads as a misconfiguration.
		return "none (provider needs no credential)", ""
	}
	if key := strings.TrimSpace(pc.APIKey); key != "" {
		if env, ok := usageEnvRef(key); ok {
			if os.Getenv(env) == "" {
				return "env (" + env + ") unset", ""
			}
			return "env (" + env + ")", usageMaskDeclared(key)
		}
		return "models.yml apiKey", usageMaskDeclared(key)
	}
	// apiKeys are sibling credentials for the same provider (M5 #25), and the
	// chain resolves pool[0] before it ever looks at a stored login, so the
	// report must too.
	if n := len(pc.APIKeys); n > 0 {
		return fmt.Sprintf("models.yml apiKeys (%d)", n), usageMaskDeclared(usageFirstKey(pc.APIKeys...))
	}
	if hasStoredCredential(name) {
		entry := store[name]
		return "credentials.json", config.Redact(usageFirstKey(entry.APIKey, entry.AccessToken))
	}
	// Nothing declared: the chain may still find an ambient
	// <PROVIDER>_API_KEY / <PROVIDER>_KEY. Ask the real resolver rather than
	// restating its naming rules (and its order) here.
	res, err := config.ResolveCredential(config.CredentialRequest{Provider: name, ProviderCfg: pc, Store: store})
	if err != nil {
		return "none declared", ""
	}
	if env, ok := strings.CutPrefix(res.Source, "env:"); ok {
		return "env (" + env + ")", config.Redact(res.Value)
	}
	if res.Source == "oauth" || res.Source == "login" {
		return "credentials.json", config.Redact(res.Value)
	}
	return usageOrDefault(res.Source, "none declared"), config.Redact(res.Value)
}

// usageModelsFor names the settings-level model bindings that belong to this
// provider, so "which account does the run model charge" is answerable from
// the same table. An unset or unparseable ref is skipped: it cannot name an
// account, and a report is no place to fail a run.
func usageModelsFor(settings *config.Settings, provider string) []string {
	if settings == nil {
		return nil
	}
	var out []string
	for _, b := range []struct{ label, ref string }{
		{"defaultModel", settings.DefaultModel},
		{"advisorModel", settings.AdvisorModel},
	} {
		ref := strings.TrimSpace(b.ref)
		if ref == "" {
			continue
		}
		if p, _, err := config.ParseModelRef(ref); err != nil || p != provider {
			continue
		}
		out = append(out, b.label)
	}
	return out
}

// usageEnvRef reports whether a declared credential is an unexpanded ${VAR}
// reference and returns the variable name.
func usageEnvRef(v string) (string, bool) {
	name, ok := strings.CutPrefix(v, "${")
	if !ok {
		return "", false
	}
	name, ok = strings.CutSuffix(name, "}")
	if !ok || name == "" || strings.ContainsAny(name, "${}") {
		return "", false
	}
	return name, true
}

// usageMaskDeclared masks one declared credential, resolving a ${VAR}
// reference first so the mask describes the value the wire will carry rather
// than the reference text.
func usageMaskDeclared(v string) string {
	if env, ok := usageEnvRef(strings.TrimSpace(v)); ok {
		return config.Redact(os.Getenv(env))
	}
	return config.Redact(v)
}

// usageFirstKey returns the first non-blank candidate (a stored entry keeps
// its key and its OAuth token in separate fields).
func usageFirstKey(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// usageRedactURL blanks a base URL's userinfo: https://user:pass@host carries
// a credential that never passes through the credential chain, and this is
// exactly the output that gets pasted into an issue.
func usageRedactURL(raw string) string {
	raw = strings.TrimSpace(raw)
	scheme := strings.Index(raw, "://")
	if scheme < 0 {
		return raw
	}
	authority := raw[scheme+3:]
	at := strings.Index(authority, "@")
	if at < 0 || strings.Contains(authority[:at], "/") {
		return raw
	}
	return raw[:scheme+3] + "…@" + authority[at+1:]
}

// usageOrDefault renders a possibly-empty cell or source label as something
// readable, so no column is ever blank.
func usageOrDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// usageSinceArg extracts the last --since value before usageCmd's flag set
// owns it: the window has to be baked into the scan closure runUsage builds.
// Last match wins, mirroring flag.Parse. A missing value or an unparseable one
// yields ok=false — usageCmd rejects both before it ever calls scan, so the
// fallback window is never rendered as if it were the user's.
func usageSinceArg(args []string) (time.Time, bool) {
	var (
		raw   string
		found bool
	)
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--since" || a == "-since":
			if i+1 >= len(args) {
				return time.Time{}, false
			}
			i++
			raw, found = args[i], true
		case strings.HasPrefix(a, "--since="):
			raw, found = strings.TrimPrefix(a, "--since="), true
		case strings.HasPrefix(a, "-since="):
			raw, found = strings.TrimPrefix(a, "-since="), true
		}
	}
	if !found || strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	t, err := usageParseSince(raw)
	return t, err == nil
}

// usageParseSince accepts the same two forms `xdev stats --since` does: a
// duration window relative to now, or a calendar date.
func usageParseSince(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse --since %q (want a duration like 168h or a date like 2026-09-01)", v)
}

// usageText renders the human view: one table row per account with its
// credential source, then the observed local telemetry.
func usageText(r usageReport) string {
	var b strings.Builder
	b.WriteString("xdev usage — accounts from models.yml, usage from the local session store\n")
	if len(r.Providers) == 0 {
		b.WriteString("\n  no providers configured: add one to ~/.xdev/agent/models.yml\n")
	} else {
		fmt.Fprintf(&b, "\n  %-*s %-*s %-*s %-*s %s\n", usageColProvider, "PROVIDER", usageColAPI, "API",
			usageColBaseURL, "BASE URL", usageColAuth, "AUTH", "ACCOUNT")
		for _, p := range r.Providers {
			account := p.Account
			if p.Masked != "" {
				account += " " + p.Masked
			}
			fmt.Fprintf(&b, "  %-*s %-*s %-*s %-*s %s\n",
				usageColProvider, truncate(p.Name, usageColProvider), usageColAPI, truncate(p.API, usageColAPI),
				usageColBaseURL, truncate(p.BaseURL, usageColBaseURL), usageColAuth, truncate(p.Auth, usageColAuth),
				account)
			if len(p.Models) > 0 {
				fmt.Fprintf(&b, "    models  %s\n", strings.Join(p.Models, ", "))
			}
			if p.Disabled {
				fmt.Fprintf(&b, "    status  disabled by settings (disabledProviders) — a run would refuse it\n")
			}
			// The one line every provider gets: there is no quota number to
			// print, and an invented one would be worse than none.
			fmt.Fprintf(&b, "    limits: not exposed by %s (api %s)\n", p.Name, p.API)
			if p.OAuth {
				fmt.Fprintf(&b, "    oauth account: the plan and its remaining limits are visible only in %s's own console\n", p.Name)
			}
		}
		b.WriteString("\n" + usageLimitsNote)
	}
	usageObservedText(&b, r.Observed)
	return b.String()
}

// usageObservedText renders the local telemetry half. Every number here is
// what the providers reported into the session store, so the section says so
// rather than presenting an invoice.
func usageObservedText(b *strings.Builder, o usageObserved) {
	t := o.Totals
	b.WriteString("\nOBSERVED (local session store — what the providers reported, not a bill)\n")
	if o.Since != "" {
		fmt.Fprintf(b, "  %-12s %s (as passed to --since)\n", "window", o.Since)
	}
	fmt.Fprintf(b, "  %-12s %d (%d priced)\n", "turns", t.Turns, t.PricedTurns)
	fmt.Fprintf(b, "  %-12s %d (harness-injected, not user input)\n", "injected", t.InjectedTurns)
	fmt.Fprintf(b, "  %-12s total %s (in %s · out %s · cache read %s · cache write %s)\n", "tokens",
		stats.HumanTokens(t.TotalTokens), stats.HumanTokens(t.Input), stats.HumanTokens(t.Output),
		stats.HumanTokens(t.CacheRead), stats.HumanTokens(t.CacheWrite))
	// The cache-hit ratio is the one number that says whether breakpoints are
	// working: input is what the provider charged full price for, so a run of
	// 0% means the prefix never got read back.
	if prompt := t.Input + t.CacheRead + t.CacheWrite; prompt > 0 {
		fmt.Fprintf(b, "  %-12s %.1f%% of %s prompt tokens\n", "cache hit",
			100*float64(t.CacheRead)/float64(prompt), stats.HumanTokens(prompt))
	}
	fmt.Fprintf(b, "  %-12s %s\n", "cost", stats.HumanMoney(t.CostUSD))
	if t.PricedTurns < t.Turns {
		fmt.Fprintf(b, "  %-12s %d of %d turns carried no provider price (counted as 0)\n", "",
			t.Turns-t.PricedTurns, t.Turns)
	}
	if len(o.Models) == 0 {
		fmt.Fprintf(b, "  %-12s none recorded under %s\n", "by model", usageOrDefault(o.DataDir, "(unknown data dir)"))
		return
	}
	fmt.Fprintf(b, "\n  by model\n  %-34s %8s %10s %10s\n", "MODEL", "TURNS", "TOKENS", "COST")
	for _, m := range o.Models {
		fmt.Fprintf(b, "  %-34s %8d %10s %10s\n", truncate(m.Model, 34), m.Turns,
			stats.HumanTokens(m.TotalTokens), stats.HumanMoney(m.CostUSD))
	}
}
