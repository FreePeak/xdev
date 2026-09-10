package tool

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
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
func envAlwaysSet() []string {
	return []string{
		"TERM=dumb",
		"NO_COLOR=1",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"DEBIAN_FRONTEND=noninteractive",
		"LC_ALL=" + resolveLocale(),
	}
}

// preferredLocales are deterministic UTF-8 locales, best first. Naming one
// that the host has not generated makes bash print
// "warning: setlocale: LC_ALL: cannot change locale" onto the stderr of
// EVERY command (observed on a slim Linux container), which pollutes tool
// output for the model. So the value is chosen from what exists locally,
// with "C" — always reported by locale -a — as the floor. Nothing is
// listed after C: an unreachable preference is dead code.
var preferredLocales = []string{"C.UTF-8", "C"}

var (
	localeOnce  sync.Once
	localeValue string
)

// resolveLocale returns the LC_ALL value for child processes. Probed once
// per process; a failed probe falls back to the only guaranteed locale.
func resolveLocale() string {
	localeOnce.Do(func() {
		localeValue = "C"
		out, err := exec.Command("locale", "-a").Output()
		if err != nil {
			return
		}
		have := map[string]bool{}
		for _, line := range strings.Split(string(out), "\n") {
			n := strings.TrimSpace(line)
			have[n] = true
			// locale -a prints one spelling and often the bare name too.
			if i := strings.IndexByte(n, '.'); i > 0 {
				have[n[:i]] = true
			}
			lower := strings.ToLower(n)
			have[strings.ReplaceAll(lower, "-", "")] = true
		}
		normalized := func(x string) string {
			x = strings.ToLower(x)
			x = strings.ReplaceAll(x, "-", "")
			return strings.ReplaceAll(x, "_", "")
		}
		for _, want := range preferredLocales {
			if have[want] || have[normalized(want)] {
				localeValue = want
				return
			}
		}
	})
	return localeValue
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

// Locale is the LC_ALL value child processes receive (probed once).
func Locale() string { return resolveLocale() }

// HardenedEnvFrom is HardenedEnv over an injected environment (KEY=VALUE
// entries, as returned by os.Environ). Always-set variables override any
// inherited value of the same key; keys are sorted for determinism. Pure
// function (injectable env) so it is directly testable.
func HardenedEnvFrom(environ []string) []string {
	always := envAlwaysSet()
	alwaysKeys := make(map[string]bool, len(always))
	for _, kv := range always {
		key, _, _ := strings.Cut(kv, "=")
		alwaysKeys[key] = true
	}

	out := make([]string, 0, len(environ)+len(always))
	seen := make(map[string]bool, len(environ)+len(always))
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
	out = append(out, always...)
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
