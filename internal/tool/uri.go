package tool

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// URI schemes let the read tool reach non-filesystem resources: skills
// (skill://), long-term memory (memory://) and raw session history
// (history://current, history://full — M12 #45). omp exposes them through
// read URLs, and the model discovers them from the system prompt.
//
// Registration is process-global (once, at cmd startup) because the
// tools are constructed in several places (main registry, subagent
// children, the advisor); a registry keeps each construction simple.

// URIResolver returns the text content for a URI.
type URIResolver func(uri string) (string, error)

var (
	uriMu        sync.RWMutex
	uriResolvers = map[string]URIResolver{}
)

// RegisterURIScheme installs a resolver for a scheme (no "://" suffix).
func RegisterURIScheme(scheme string, fn URIResolver) {
	uriMu.Lock()
	defer uriMu.Unlock()
	uriResolvers[strings.ToLower(scheme)] = fn
}

// resolveURI reports whether uri uses a registered scheme and, if so,
// returns its content.
func resolveURI(uri string) (string, bool, error) {
	i := strings.Index(uri, "://")
	if i <= 0 {
		return "", false, nil
	}
	scheme := strings.ToLower(uri[:i])
	uriMu.RLock()
	fn, ok := uriResolvers[scheme]
	uriMu.RUnlock()
	if !ok {
		return "", false, nil
	}
	text, err := fn(uri)
	if err != nil {
		return "", true, fmt.Errorf("%s", err.Error())
	}
	return text, true, nil
}

// unknownURIScheme reports whether uri carries a scheme:// prefix that no
// resolver claims. That case used to be indistinguishable from a plain
// path, so read stat'ed it and answered "file not found: memory://..." for
// a URI that was never a file — the write side already refuses to guess
// (see WriteURI), and a read should not silently reinterpret it either.
func unknownURIScheme(uri string) (scheme string, unknown bool) {
	i := strings.Index(uri, "://")
	if i <= 0 {
		return "", false
	}
	scheme = strings.ToLower(uri[:i])
	uriMu.RLock()
	_, ok := uriResolvers[scheme]
	uriMu.RUnlock()
	return scheme, !ok
}

// supportedURISchemes renders the schemes this process resolves, for the
// error an unknown scheme earns (writeDeviceNames does the same job for
// write-devices).
func supportedURISchemes() string {
	uriMu.RLock()
	names := make([]string, 0, len(uriResolvers))
	for s := range uriResolvers {
		names = append(names, s+"://")
	}
	uriMu.RUnlock()
	if len(names) == 0 {
		return "filesystem paths"
	}
	sort.Strings(names)
	return strings.Join(names, ", ") + " and filesystem paths"
}

// URIWriter finalizes a write-device: content is the write payload, the
// returned text is what the write tool reports back. Write-devices are
// the omp xd:// transport — e.g. write xd://resolve "<reason>" finalizes
// a pending plan proposal (#36) — and ride the same process-global
// registry as the read schemes.
type URIWriter func(uri, content string) (string, error)

var uriWriters = map[string]map[string]URIWriter{} // scheme -> device -> writer

// RegisterWriteDevice installs a writer under scheme://device
// (e.g. "xd", "resolve"). Re-registration replaces (tests, repeated
// startup).
func RegisterWriteDevice(scheme, device string, fn URIWriter) {
	uriMu.Lock()
	defer uriMu.Unlock()
	scheme, device = strings.ToLower(scheme), strings.ToLower(device)
	if uriWriters[scheme] == nil {
		uriWriters[scheme] = map[string]URIWriter{}
	}
	uriWriters[scheme][device] = fn
}

// WriteURI dispatches a write to a registered device. handled=false when
// the scheme has no write-device, so callers fall through to the
// filesystem; a registered scheme with an unknown device is an error —
// a typo'd device must not silently create a file.
func WriteURI(uri, content string) (text string, handled bool, err error) {
	i := strings.Index(uri, "://")
	if i <= 0 {
		return "", false, nil
	}
	uriMu.RLock()
	scheme := strings.ToLower(uri[:i])
	devs, ok := uriWriters[scheme]
	uriMu.RUnlock()
	if !ok {
		return "", false, nil
	}
	uriMu.RLock()
	fn, ok := devs[strings.ToLower(uri[i+3:])]
	uriMu.RUnlock()
	if !ok {
		return "", true, fmt.Errorf("unknown %s:// write-device (have: %s)", scheme, strings.Join(writeDeviceNames(devs), ", "))
	}
	text, err = fn(uri, content)
	return text, true, err
}

func writeDeviceNames(devs map[string]URIWriter) []string {
	names := make([]string, 0, len(devs))
	for d := range devs {
		names = append(names, d)
	}
	sort.Strings(names)
	return names
}
