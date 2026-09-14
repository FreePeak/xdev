// The macOS Keychain as a credential SOURCE (#123, part 2), measured rather
// than assumed.
//
// What the Keychain is good for here: keeping a long-lived token out of any
// file at all. It is the only place on a Mac where a secret is encrypted at
// rest by default and survives a copied home directory, a Time Machine
// restore, or a dotfiles sync that grabbed ~/.xdev by accident.
//
// What it is NOT good for, and why xdev never writes to it: /usr/bin/security
// takes the secret one of two ways, and both are unusable from a program.
//
//   - As an argument (`-w <secret>`): it lands in the process table, where any
//     other user on the machine can read it with ps. That is strictly worse
//     than the 0600 file it would replace — a leak introduced by the fix.
//   - Through the interactive prompt (`-w` with no argument, fed on stdin):
//     measured on macOS 26, the prompt read is line-buffered and silently
//     truncates at 128 bytes — 129 written stores 128, with exit status 0. An
//     OpenAI or Anthropic API key fits; an OAuth access or refresh token (a
//     JWT, typically 1–2 KB) does not, and the failure mode is a corrupt
//     credential that no longer authenticates, reported as a successful login.
//     The same shape through a real PTY (expect, which is what fx drives)
//     stored nothing at all in the same measurement.
//
// Reading has neither problem: the value comes back on stdout through no line
// discipline, measured exact at 8 KiB. So xdev reads, never writes, and the
// item is created by a tool built for it — a human at the prompt with `xdev
// credential keychain-add`, 1Password's CLI, or a secrets agent.
//
// A named item that is absent is an error, never a fall-through: silently
// resolving a different credential is how a work token ends up on a personal
// account (#114 is the same failure seen from the config side).
package config

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const (
	// keychainBinary is an absolute path on purpose: a credential lookup must
	// not end up running whatever a PATH entry happens to shadow that name.
	keychainBinary = "/usr/bin/security"

	// keychainPrefix is the spelling in a models.yml apiKey that says "this
	// provider's credential lives in the Keychain, under this service". An
	// account part is optional because most people keep one item per provider.
	//
	//	keychain:dev.xdev.credential.openai
	//	keychain:dev.xdev.credential.openai/linh
	keychainPrefix = "keychain:"

	// DisableKeychainEnv skips the lookup entirely, so a machine whose login
	// keychain is locked or unreadable (headless CI, ssh without an unlocked
	// session) fails fast with the file-store message instead of a Keychain one.
	DisableKeychainEnv = "XDEV_DISABLE_KEYCHAIN"
)

// KeychainRef is a parsed `keychain:<service>[/<account>]` credential reference.
type KeychainRef struct {
	Service string
	Account string
}

// parseKeychainRef reports whether a configured credential value names a
// Keychain item rather than being the secret itself.
func parseKeychainRef(v string) (KeychainRef, bool) {
	raw, found := strings.CutPrefix(strings.TrimSpace(v), keychainPrefix)
	if !found {
		return KeychainRef{}, false
	}
	service, account, _ := strings.Cut(raw, "/")
	if strings.TrimSpace(service) == "" {
		return KeychainRef{}, false
	}
	if account == "" {
		account = service
	}
	return KeychainRef{Service: strings.TrimSpace(service), Account: strings.TrimSpace(account)}, true
}

// KeychainAvailable reports whether a Keychain lookup can be attempted: darwin,
// the system tool present, and not disabled by the environment.
func KeychainAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	if v := strings.TrimSpace(os.Getenv(DisableKeychainEnv)); v != "" && v != "0" && v != "false" {
		return false
	}
	_, err := os.Stat(keychainBinary)
	return err == nil
}

// keychainRead fetches one item's secret through /usr/bin/security.
//
// The value is read on stdout, never from argv and never through a tty, so no
// secret appears in the process table and no length limit applies. Diagnostics
// come back separately: `security` prints its own errors on stderr, and an
// empty stdout with exit 44 means the item does not exist.
func keychainRead(ref KeychainRef) (string, error) {
	return runKeychainRead(keychainBinary, ref)
}

// runKeychainRead is keychainRead with the tool injected, so the tests exercise
// the parsing and the missing-item contract without a Keychain (CI is Linux).
func runKeychainRead(binary string, ref KeychainRef) (string, error) {
	cmd := exec.Command(binary, "find-generic-password", "-s", ref.Service, "-a", ref.Account, "-w")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if isMissingItem(msg) {
			return "", fmt.Errorf("keychain item %s/%s not found", ref.Service, ref.Account)
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s: %s", binary, msg)
	}
	// The tool terminates the value with a newline; a secret's own trailing
	// whitespace is preserved because exactly one terminator is removed.
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

// keychainLookup is the fetch the resolution chain uses, as one seam so tests
// can drive both outcomes on a host without a Keychain (CI is Linux).
var keychainLookup = func(ref KeychainRef) (string, error) {
	if !KeychainAvailable() {
		return "", fmt.Errorf("this host cannot query the Keychain (it is macOS-only, and %s must not be set to disable it)", DisableKeychainEnv)
	}
	return keychainRead(ref)
}

// isMissingItem recognises the wording /usr/bin/security prints for an item
// that does not exist — observed verbatim as
// `security: SecKeychainSearchCopyNext: The specified item could not be found
// in the keychain.` — so an absent credential is reported as missing rather
// than as a mystery failure.
func isMissingItem(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "could not be found")
}
