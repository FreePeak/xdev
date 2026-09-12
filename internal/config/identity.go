package config

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Install identity (M14 #63, omp's install-id): one random lowercase UUID
// per installation, shared by every profile and session, for provider
// metadata that needs a stable installation id (Codex `installationId`,
// Claude `device_id`). It is never derived from hostname, username, or
// hardware data, and a valid stored value is never rewritten.

// installIDPattern is the acceptance rule: case-insensitive on read (the
// stored spelling is returned as-is), lowercase on generation.
var installIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// installIDWarn receives the one-line lifecycle warnings (stderr; tests
// point it at a buffer).
var installIDWarn io.Writer = os.Stderr

var (
	installIDMu    sync.Mutex
	installIDValue string
)

// InstallID returns this installation's id, minting and persisting one on
// first call. The value is cached for the life of the process.
//
// Lifecycle: a stored UUID is returned as written. Anything else — absent,
// empty, or garbage — is regenerated once, with a warning on stderr. A
// concurrent first run is settled by the exclusive create: the loser of
// the race re-reads the winner's file and adopts that id, so two
// simultaneous runs converge on one installation id.
func InstallID() string {
	installIDMu.Lock()
	defer installIDMu.Unlock()
	if installIDValue != "" {
		return installIDValue
	}
	path := InstallIDPath()
	id := readInstallID(path)
	if id == "" {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(installIDWarn, "xdev: warning: %s does not hold a UUID; regenerating\n", path)
		}
		id = mintInstallID(path)
	}
	installIDValue = id
	return id
}

// InstallIDPath is <install dir>/install-id: ~/.xdev/install-id by default
// — outside the profile nesting, so every profile on the host reports the
// same installation id — or $XDEV_AGENT_DIR/install-id in a sandbox.
func InstallIDPath() string { return filepath.Join(installDir(), "install-id") }

// ResetInstallIDCacheForTests clears the in-process id cache. Test-only;
// production code never calls it (the cache mirrors the persisted value).
func ResetInstallIDCacheForTests() {
	installIDMu.Lock()
	defer installIDMu.Unlock()
	installIDValue = ""
}

// readInstallID returns the stored id exactly as written, or "" when the
// file is missing or does not hold a UUID.
func readInstallID(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(raw))
	if !installIDPattern.MatchString(v) {
		return ""
	}
	return v
}

// mintInstallID persists a fresh id and returns the one that won.
//
// Any file that is not a UUID is unlinked first so the exclusive create
// cannot trip on stale data; a valid file appearing in that window is left
// alone, and its value wins. A persistence failure is not fatal — the
// freshly minted id still holds for this process, and the next launch
// retries.
func mintInstallID(path string) string {
	if !installIDPattern.MatchString(readInstallID(path)) {
		if _, err := os.Stat(path); err == nil {
			// ponytail: unlink-then-create keeps O_EXCL meaningful, at the
			// cost of a theoretical double-mint if a second process reads
			// the file inside this window; both processes still converge
			// on whichever id lands first.
			if err := os.Remove(path); err != nil {
				warnInstallID("cannot remove %s: %v", path, err)
			}
		}
	}
	id := newUUID()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		warnInstallID("cannot create %s: %v; using an in-process id", filepath.Dir(path), err)
		return id
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if other := readInstallID(path); other != "" {
			return other // lost the exclusive-create race: adopt the winner
		}
		warnInstallID("cannot write %s: %v; using an in-process id", path, err)
		return id
	}
	defer f.Close()
	if _, err := f.WriteString(id + "\n"); err != nil {
		warnInstallID("cannot write %s: %v; using an in-process id", path, err)
	}
	return id
}

func warnInstallID(format string, args ...any) {
	fmt.Fprintf(installIDWarn, "xdev: warning: "+format+"\n", args...)
}

// newUUID mints a lowercase RFC 4122 version 4 UUID. crypto/rand does not
// fail (it panics when the OS source is unusable, which is unrecoverable
// either way), so no error path is needed.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sessionAlias is the agent identity --alias gives this session.
var sessionAlias string

// SetAlias records the --alias value: the name other sessions address this
// run by (mailbox RegisterIdentity). Empty clears it; the name must be
// path-safe because it becomes the identity file's name.
func SetAlias(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		sessionAlias = ""
		return nil
	}
	if !validName(name) {
		return fmt.Errorf("invalid alias %q: use letters, digits, '.', '_' or '-' (max 64)", name)
	}
	sessionAlias = name
	return nil
}

// Alias returns the session's agent identity name ("" = none: other
// sessions can still address it by session id).
func Alias() string { return sessionAlias }
