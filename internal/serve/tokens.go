package serve

import (
	"os"
	"path/filepath"
)

// CLI-facing view of the per-install service tokens (xdev token). The token
// file, its 0600 creation and the mint/race rules stay in serve.go — this file
// only exposes the same files the daemons read, so `xdev token rotate` can
// never disagree with what a running service loads.

// Services returns the service names serve dispatches, in the order the token
// table should list them.
func Services() []string {
	return []string{serviceBroker, serviceGateway, serviceRelay}
}

// KnownService reports whether name is one of Services().
func KnownService(name string) bool {
	for _, s := range Services() {
		if s == name {
			return true
		}
	}
	return false
}

// DefaultListen is the loopback address the service binds without --listen.
func DefaultListen(service string) string { return defaultListen(service) }

// TokenPath is <dataDir>/serve/<service>.token, the file a daemon reads.
func TokenPath(dataDir, service string) string {
	return tokenPath(filepath.Join(dataDir, "serve"), service)
}

// ReadToken returns the stored token without minting one ("" = never minted).
func ReadToken(dataDir, service string) string {
	return readToken(TokenPath(dataDir, service))
}

// EnsureToken returns the service's token, minting one on first use exactly
// like the daemons do (exclusive create, so two racers agree).
func EnsureToken(dataDir, service string) (string, error) {
	return ensureToken(service, Options{DataDir: dataDir})
}

// RotateToken mints a fresh token, replacing any existing one. A running
// daemon keeps the token it loaded at startup until it restarts, so callers
// must tell the operator that a restart is what makes a rotation effective.
func RotateToken(dataDir, service string) (string, error) {
	dir, err := serveDir(Options{DataDir: dataDir})
	if err != nil {
		return "", err
	}
	tok, err := randHex(32)
	if err != nil {
		return "", err
	}
	// Write to a temp file and rename: a rotation must never leave a
	// half-written token file that a daemon reads as a short secret.
	path := tokenPath(dir, service)
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return "", err
	}
	return tok, nil
}
