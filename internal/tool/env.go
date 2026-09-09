package tool

import (
	"os"
	"regexp"
	"sort"
	"strings"
)

// envStripRe matches any env var whose key contains a sensitive marker
// (port of omp's env-hardening pattern list). Case-insensitive.
var envStripRe = regexp.MustCompile(`(?i)(API|KEY|TOKEN|SECRET|PASSWD|PASSWORD|CREDENTIAL|AWS_|AZURE_|GOOGLE_|OPENAI|ANTHROPIC|GEMINI|SSH_AUTH_SOCK|GPG|GNUPG|XDEV_|ONEGW_)`)

// envAlwaysStrip is removed from the child environment unconditionally.
var envAlwaysStrip = []string{
	"HISTFILE",
	"INPUTRC",
	"CDPATH",
	"GLOBIGNORE",
}

// envAlwaysSet is injected into the child environment unconditionally,
// making tool output deterministic and non-interactive. It overrides any
// inherited value of the same key.
var envAlwaysSet = []string{
	"TERM=dumb",
	"NO_COLOR=1",
	"GIT_PAGER=cat",
	"PAGER=cat",
	"DEBIAN_FRONTEND=noninteractive",
	"LC_ALL=en_US.UTF-8",
}

// EnvHardening returns the list of env var key names to strip from child
// bash processes: every key matching the sensitive pattern plus the
// always-strip set.
func EnvHardening() []string {
	return hardenEnvKeys(os.Environ())
}

// HardenedEnv returns the filtered child environment: os.Environ() minus
// stripped keys, plus the always-set non-interactive variables. Keys are
// sorted for determinism.
func HardenedEnv() []string { return HardenedEnvFrom(os.Environ()) }

// HardenedEnvFrom is HardenedEnv over an injected environment (KEY=VALUE
// entries, as returned by os.Environ). Always-set variables override any
// inherited value of the same key; keys are sorted for determinism. Pure
// function (injectable env) so it is directly testable.
func HardenedEnvFrom(environ []string) []string {
	alwaysKeys := make(map[string]bool, len(envAlwaysSet))
	for _, kv := range envAlwaysSet {
		key, _, _ := strings.Cut(kv, "=")
		alwaysKeys[key] = true
	}

	out := make([]string, 0, len(environ)+len(envAlwaysSet))
	seen := make(map[string]bool, len(environ)+len(envAlwaysSet))
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		if shouldStripKey(key) || alwaysKeys[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, kv)
	}
	out = append(out, envAlwaysSet...)
	sort.Strings(out)
	return out
}

// shouldStripKey reports whether an env key matches the hardening pattern
// or the always-strip set.
func shouldStripKey(key string) bool {
	for _, s := range envAlwaysStrip {
		if key == s {
			return true
		}
	}
	return envStripRe.MatchString(key)
}

// hardenEnvKeys is EnvHardening over an injected environment.
func hardenEnvKeys(environ []string) []string {
	keys := make([]string, 0, len(environ))
	seen := make(map[string]bool, len(environ))
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" || seen[key] || !shouldStripKey(key) {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
