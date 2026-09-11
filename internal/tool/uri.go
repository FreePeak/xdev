package tool

import (
	"fmt"
	"strings"
	"sync"
)

// URI schemes let the read tool reach non-filesystem resources: skills
// (skill://) and long-term memory (memory://). omp exposes both through
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
