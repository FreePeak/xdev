package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/stats"
)

// usageTestSecret is the value ONEGW_KEY carries in the fixture: long enough
// that config.Redact keeps a recognisable prefix/suffix, so a leaked value
// would show up as the full string in the output under test.
const usageTestSecret = "sk-usage-fixture-0123456789abcdef"

// usageTestConfig is the fixture every assertion runs against: one provider
// whose credential is an unexpanded ${VAR} reference — the shape LoadModels
// expands while parsing models.yml and an extension-registered provider can
// still carry — and one keyless local server.
func usageTestConfig() *config.Config {
	return &config.Config{Providers: map[string]*config.ProviderConfig{
		"onegw": {
			BaseURL: "https://gw.example/v1",
			API:     "openai-completions",
			APIKey:  "${ONEGW_KEY}",
		},
		"local": {
			BaseURL: "http://127.0.0.1:11434/v1",
			API:     "openai-completions",
			Auth:    "none",
		},
	}}
}

// usageTestSettings binds one role to each provider, so the role column proves
// settings is actually consulted rather than merely accepted.
func usageTestSettings() *config.Settings {
	return &config.Settings{ModelRoles: map[string]string{"main": "onegw/free", "task": "local/qwen3"}}
}

// usageTestReport is the fixed local telemetry the stub scan returns.
func usageTestReport() *stats.Report {
	return &stats.Report{
		DataDir: "/tmp/xdev-usage-fixture",
		Totals: stats.Totals{
			Turns: 12, PricedTurns: 10, Input: 1_200_000, Output: 30_000,
			TotalTokens: 1_230_000, CostUSD: 3.42,
		},
		Models: []stats.ModelStat{
			{Model: "onegw/free", Turns: 11, TotalTokens: 1_200_000, CostUSD: 3.42},
			{Model: "local/qwen3", Turns: 1, TotalTokens: 30_000},
		},
	}
}

// TestUsageCmdReportsAccountsLimitsAndObservedUsage covers the command's core
// contract in one pass: every configured provider gets an ACCOUNT that names
// its credential source and an honest limits line, the credential value itself
// never appears, and the observed half renders the stub's totals and per-model
// rows.
func TestUsageCmdReportsAccountsLimitsAndObservedUsage(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("ONEGW_KEY", usageTestSecret)

	var out, errOut bytes.Buffer
	code := usageCmd(nil, usageTestConfig(), usageTestSettings(),
		func() (*stats.Report, error) { return usageTestReport(), nil }, &out, &errOut)
	if code != 0 {
		t.Fatalf("usage = %d, stderr %q", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"limits: not exposed by onegw (api openai-completions)",
		"limits: not exposed by local (api openai-completions)",
		"env (ONEGW_KEY)",
		"none (provider needs no credential)",
		"https://gw.example/v1",
		"local/qwen3",
		"@main",
		"@task",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}

	// The credential value is masked, never printed.
	if strings.Contains(got, usageTestSecret) {
		t.Errorf("output leaked the credential value:\n%s", got)
	}
	if masked := config.Redact(usageTestSecret); !strings.Contains(got, masked) {
		t.Errorf("output missing the masked credential %q:\n%s", masked, got)
	}

	// Observed totals.
	for _, want := range []string{
		"12 (10 priced)",
		"total " + stats.HumanTokens(1_230_000),
		stats.HumanMoney(3.42),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("observed totals missing %q:\n%s", want, got)
		}
	}

	// One per-model row, rendered as a single line with its own numbers.
	row := ""
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "onegw/free") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("no per-model row for onegw/free:\n%s", got)
	}
	for _, want := range []string{"11", stats.HumanTokens(1_200_000), stats.HumanMoney(3.42)} {
		if !strings.Contains(row, want) {
			t.Errorf("per-model row %q missing %q", row, want)
		}
	}
}

// TestUsageCmdUnknownProvider: naming a provider that models.yml does not have
// is a usage error, not an empty report.
func TestUsageCmdUnknownProvider(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())

	var out, errOut bytes.Buffer
	code := usageCmd([]string{"--provider", "missing"}, usageTestConfig(), usageTestSettings(),
		func() (*stats.Report, error) { return usageTestReport(), nil }, &out, &errOut)
	if code != 2 {
		t.Fatalf("usage = %d (want 2), stderr %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("a rejected provider still printed a report:\n%s", out.String())
	}
	msg := errOut.String()
	for _, want := range []string{"unknown provider", `"missing"`, "onegw", "local"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// TestUsageCmdProviderFilter: --provider narrows the table to that account
// while the observed section stays whole (local telemetry is not per account).
func TestUsageCmdProviderFilter(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("ONEGW_KEY", usageTestSecret)

	var out, errOut bytes.Buffer
	code := usageCmd([]string{"--provider", "local"}, usageTestConfig(), usageTestSettings(),
		func() (*stats.Report, error) { return usageTestReport(), nil }, &out, &errOut)
	if code != 0 {
		t.Fatalf("usage = %d, stderr %q", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "limits: not exposed by local (api openai-completions)") {
		t.Errorf("selected provider missing:\n%s", got)
	}
	if strings.Contains(got, "https://gw.example/v1") || strings.Contains(got, "limits: not exposed by onegw") {
		t.Errorf("--provider local still rendered onegw:\n%s", got)
	}
	if !strings.Contains(got, "onegw/free") {
		t.Errorf("observed section should stay whole:\n%s", got)
	}
}

// TestUsageCmdJSON: --json is one parseable object holding both halves, with
// the credential masked inside it as well.
func TestUsageCmdJSON(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Setenv("ONEGW_KEY", usageTestSecret)

	var out, errOut bytes.Buffer
	code := usageCmd([]string{"--json"}, usageTestConfig(), usageTestSettings(),
		func() (*stats.Report, error) { return usageTestReport(), nil }, &out, &errOut)
	if code != 0 {
		t.Fatalf("usage --json = %d, stderr %q", code, errOut.String())
	}
	var report usageReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("--json output is not parseable: %v\n%s", err, out.String())
	}
	if len(report.Providers) != 2 {
		t.Fatalf("providers = %d (want 2): %+v", len(report.Providers), report.Providers)
	}
	byName := map[string]usageProvider{}
	for _, p := range report.Providers {
		byName[p.Name] = p
	}
	if got := byName["onegw"]; got.Account != "env (ONEGW_KEY)" || got.Masked != config.Redact(usageTestSecret) || got.API != "openai-completions" {
		t.Errorf("onegw row = %+v", got)
	}
	if got := byName["local"]; got.Auth != "none" || got.Masked != "" {
		t.Errorf("local row = %+v", got)
	}
	if report.Observed.Totals.Turns != 12 || len(report.Observed.Models) != 2 || report.Observed.Models[0].Model != "onegw/free" {
		t.Errorf("observed = %+v", report.Observed)
	}
	if strings.Contains(out.String(), usageTestSecret) {
		t.Errorf("--json leaked the credential value:\n%s", out.String())
	}
}

// TestUsageCmdRejectsBadSince: an unparseable window fails before the store is
// read at all, so a typo cannot silently scan everything.
func TestUsageCmdRejectsBadSince(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())

	scanned := false
	var out, errOut bytes.Buffer
	code := usageCmd([]string{"--since", "yesterday"}, usageTestConfig(), usageTestSettings(),
		func() (*stats.Report, error) { scanned = true; return usageTestReport(), nil }, &out, &errOut)
	if code != 2 {
		t.Fatalf("usage --since yesterday = %d (want 2), stderr %q", code, errOut.String())
	}
	if scanned {
		t.Error("the scan ran despite a rejected --since")
	}
	if msg := errOut.String(); !strings.Contains(msg, "cannot parse --since") {
		t.Errorf("error %q does not name the bad flag value", msg)
	}
}

// TestUsageSinceArgAndParse pins the window parsing shared with runUsage:
// durations and dates are accepted, anything else is not, and the last flag
// occurrence wins exactly as flag.Parse would have it.
func TestUsageSinceArgAndParse(t *testing.T) {
	if _, err := usageParseSince("nonsense"); err == nil {
		t.Error("usageParseSince accepted a non-duration, non-date value")
	}
	date, err := usageParseSince("2026-09-01")
	if err != nil || !date.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date = %v, %v", date, err)
	}
	dur, err := usageParseSince("24h")
	if err != nil || time.Since(dur) < 23*time.Hour || time.Since(dur) > 25*time.Hour {
		t.Errorf("duration = %v, %v", dur, err)
	}

	if got, ok := usageSinceArg([]string{"--since", "nonsense"}); ok {
		t.Errorf("usageSinceArg accepted a bad value: %v", got)
	}
	if _, ok := usageSinceArg([]string{"--since"}); ok {
		t.Error("usageSinceArg accepted a dangling --since")
	}
	if _, ok := usageSinceArg([]string{"--provider", "onegw"}); ok {
		t.Error("usageSinceArg invented a window when no --since was passed")
	}
	last, ok := usageSinceArg([]string{"--since=2026-09-01", "--since", "1h"})
	if !ok {
		t.Fatal("usageSinceArg rejected a valid window")
	}
	if d := time.Since(last); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("usageSinceArg used %v (want the last --since, 1h ago)", d)
	}
}

// TestUsageCmdAccountSources: the ACCOUNT column names the source the chain
// would really use — a stored login only when models.yml declares nothing,
// and a rotation pool beats the store (pool[0] resolves before a login).
func TestUsageCmdAccountSources(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	t.Setenv("ONEGW_KEY", usageTestSecret)
	if err := config.SaveCredential("logged-in", config.StoredCredential{APIKey: "sk-stored-login-secret"}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	cfg := &config.Config{Providers: map[string]*config.ProviderConfig{
		"logged-in": {BaseURL: "https://a.example/v1", API: "openai-completions"},
		"pooled":    {BaseURL: "https://b.example/v1", API: "openai-completions", APIKeys: []string{"sk-pool-one-secret"}},
	}}

	var out, errOut bytes.Buffer
	code := usageCmd([]string{"--provider", "logged-in"}, cfg, usageTestSettings(),
		func() (*stats.Report, error) { return usageTestReport(), nil }, &out, &errOut)
	if code != 0 {
		t.Fatalf("usage = %d, stderr %q", code, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "credentials.json") || !strings.Contains(got, config.Redact("sk-stored-login-secret")) {
		t.Errorf("stored login not reported:\n%s", got)
	}

	out.Reset()
	errOut.Reset()
	code = usageCmd([]string{"--provider", "pooled"}, cfg, usageTestSettings(),
		func() (*stats.Report, error) { return usageTestReport(), nil }, &out, &errOut)
	if code != 0 {
		t.Fatalf("usage = %d, stderr %q", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"models.yml apiKeys (1)", config.Redact("sk-pool-one-secret")} {
		if !strings.Contains(got, want) {
			t.Errorf("pooled row missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "credentials.json") {
		t.Errorf("a stored login outranked a declared rotation pool:\n%s", got)
	}
	if strings.Contains(got, "sk-pool-one-secret") || strings.Contains(got, "sk-stored-login-secret") {
		t.Errorf("output leaked a credential value:\n%s", got)
	}
}
