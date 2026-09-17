package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
)

// DefaultIdleTimeout shuts down a server idle for this long.
const DefaultIdleTimeout = 5 * time.Minute

// Config is the resolved lsp configuration: settings layered over the
// built-in servers.
type Config struct {
	Lazy        bool
	IdleTimeout time.Duration
	Servers     map[string]ServerSpec
}

// ConfigFromSettings layers user settings under DefaultServers: a user entry
// for a known language merges onto the default (override the command, keep
// the file types); an unknown key adds a server. Malformed durations keep
// the default rather than failing the session — the lsp-config command
// reports them.
func ConfigFromSettings(s *config.Settings) Config {
	cfg := Config{Lazy: true, IdleTimeout: DefaultIdleTimeout, Servers: DefaultServers()}
	if s == nil || s.LSP == nil {
		return cfg
	}
	l := s.LSP
	if l.Lazy != nil {
		cfg.Lazy = *l.Lazy
	}
	if d, err := time.ParseDuration(strings.TrimSpace(l.IdleTimeout)); err == nil && d > 0 {
		cfg.IdleTimeout = d
	}
	for name, over := range l.Servers {
		cfg.Servers[name] = mergeSpec(cfg.Servers[name], specFromConfig(over))
	}
	return cfg
}

// specFromConfig maps the config type onto the lsp one. RootPatterns is the
// documented alias for rootMarkers.
func specFromConfig(c config.LSPServer) ServerSpec {
	markers := c.RootMarkers
	if len(markers) == 0 {
		markers = c.RootPatterns
	}
	sp := ServerSpec{
		Command:     c.Command,
		Args:        c.Args,
		FileTypes:   c.FileTypes,
		RootMarkers: markers,
		Disabled:    c.Disabled,
	}
	if len(c.InitOptions) > 0 {
		if raw, err := json.Marshal(c.InitOptions); err == nil {
			sp.InitOptions = raw
		}
	}
	return sp
}

func mergeSpec(base, over ServerSpec) ServerSpec {
	out := base
	if over.Command != "" {
		out.Command = over.Command
	}
	if over.Args != nil {
		out.Args = over.Args
	}
	if over.FileTypes != nil {
		out.FileTypes = over.FileTypes
	}
	if over.RootMarkers != nil {
		out.RootMarkers = over.RootMarkers
	}
	if len(over.InitOptions) > 0 {
		out.InitOptions = over.InitOptions
	}
	if over.Disabled {
		out.Disabled = true
	}
	return out
}

type startResult struct {
	c   *Client
	err error
}

// startCall is one in-flight launch, shared by every caller that arrived
// while it was running. A single result channel cannot be shared: the first
// waiter takes the buffered value and every later waiter sees the zero value
// from the closed channel, i.e. a nil *startResult — which the caller then
// dereferenced (a real panic when two tool calls raced the first gopls
// launch). Each waiter gets its own one-shot delivery instead.
type startCall struct {
	mu      sync.Mutex
	done    bool
	res     *startResult
	waiters []chan *startResult
}

// add returns the channel one waiter should read; it is answered
// immediately when the launch already finished.
func (sc *startCall) add() chan *startResult {
	ch := make(chan *startResult, 1)
	sc.mu.Lock()
	if sc.done {
		ch <- sc.res
	} else {
		sc.waiters = append(sc.waiters, ch)
	}
	sc.mu.Unlock()
	return ch
}

// finish publishes the result to every waiter exactly once.
func (sc *startCall) finish(res *startResult) {
	sc.mu.Lock()
	if sc.done {
		sc.mu.Unlock()
		return
	}
	sc.done, sc.res = true, res
	waiters := sc.waiters
	sc.waiters = nil
	sc.mu.Unlock()
	for _, ch := range waiters {
		ch <- res
	}
}

type entry struct {
	c     *Client
	timer *time.Timer // fires IdleTimeout after the last use; nil = never
	last  time.Time   // last use, so a timer that fires mid-call can re-arm
}

// Manager owns the lazily launched servers of one session. Servers are keyed
// by (name, root): the same language in two projects gets two servers.
type Manager struct {
	cfg Config
	cwd string

	mu       sync.Mutex
	servers  map[string]*entry
	starting map[string]*startCall
	closed   bool

	// start is the launch seam, swappable for tests.
	start func(ctx context.Context, name string, spec ServerSpec, root string) (*Client, error)
}

// NewManager returns a manager for one session.
func NewManager(cfg Config, cwd string) *Manager {
	return &Manager{
		cfg:      cfg,
		cwd:      cwd,
		servers:  map[string]*entry{},
		starting: map[string]*startCall{},
		start:    startServer,
	}
}

// ClientFor returns the server handling file, launching it lazily on first
// use. Concurrent first calls share one launch.
func (m *Manager) ClientFor(ctx context.Context, file string) (*Client, string, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, "", fmt.Errorf("lsp: %w", err)
	}
	name, spec, langID, err := m.resolve(abs)
	if err != nil {
		return nil, "", err
	}
	root := detectRoot(filepath.Dir(abs), m.cwd, spec.RootMarkers)
	c, err := m.clientAt(ctx, name, spec, root)
	return c, langID, err
}

// clientAt returns (or launches) the server for (name, root) and touches its
// idle timer.
func (m *Manager) clientAt(ctx context.Context, name string, spec ServerSpec, root string) (*Client, error) {
	key := name + "\x00" + root
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("lsp/%s: manager closed", name)
	}
	if e, ok := m.servers[key]; ok {
		select {
		case <-e.c.Done():
			// The server died: drop it and fall through to a fresh launch,
			// so a crashed gopls does not poison the rest of the session.
			delete(m.servers, key)
			if e.timer != nil {
				e.timer.Stop()
			}
		default:
			m.touch(e)
			m.mu.Unlock()
			return e.c, nil
		}
	}
	if sc, ok := m.starting[key]; ok {
		m.mu.Unlock()
		res := <-sc.add()
		// res is non-nil by construction now (every waiter is answered),
		// but a nil result must never become a nil-dereference.
		if res == nil {
			return nil, fmt.Errorf("lsp/%s: launch produced no result", name)
		}
		return res.c, res.err
	}
	sc := &startCall{}
	m.starting[key] = sc
	m.mu.Unlock()

	c, err := m.start(ctx, name, spec, root)

	m.mu.Lock()
	delete(m.starting, key)
	if err == nil {
		e := &entry{c: c, last: time.Now()}
		if m.cfg.IdleTimeout > 0 {
			e.timer = time.AfterFunc(m.cfg.IdleTimeout, func() { m.idleDrop(key) })
		}
		m.servers[key] = e
	}
	m.mu.Unlock()

	sc.finish(&startResult{c: c, err: err})
	return c, err
}

// touch resets the idle timer. Callers hold m.mu.
func (m *Manager) touch(e *entry) {
	e.last = time.Now()
	if e.timer != nil {
		e.timer.Reset(m.cfg.IdleTimeout)
	}
}

// idleDrop shuts down one idle server. Runs off the timer goroutine so a
// slow handshake never blocks a tool call.
func (m *Manager) idleDrop(key string) {
	m.mu.Lock()
	e, ok := m.servers[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	// A call that arrived while this timer was firing already reset it; the
	// entry is not idle, so re-arm instead of closing a client in use.
	if left := m.cfg.IdleTimeout - time.Since(e.last); left > 0 {
		if e.timer != nil {
			e.timer.Reset(left)
		}
		m.mu.Unlock()
		return
	}
	delete(m.servers, key)
	m.mu.Unlock()
	logx.Debugf("lsp: idle timeout, stopping %s", key)
	_ = e.c.Close()
}

// resolve maps a file to its server: first a spec whose fileTypes claim the
// extension (sorted for determinism), else the built-in language table.
func (m *Manager) resolve(path string) (string, ServerSpec, string, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		return "", ServerSpec{}, "", fmt.Errorf("lsp: %s: no file extension, cannot pick a server", filepath.Base(path))
	}
	names := make([]string, 0, len(m.cfg.Servers))
	for n := range m.cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sp := m.cfg.Servers[n]
		if sp.Disabled || sp.Command == "" {
			continue
		}
		for _, ft := range sp.FileTypes {
			if strings.EqualFold(strings.TrimPrefix(ft, "."), ext) {
				return n, sp, langIDFor(ext), nil
			}
		}
	}
	lang, ok := builtinLanguage[ext]
	if !ok {
		return "", ServerSpec{}, "", fmt.Errorf(
			"lsp: no language server for .%s files — add lsp.servers.<name> to .xdev/config.yml", ext)
	}
	if sp, ok := m.cfg.Servers[lang]; ok && !sp.Disabled && sp.Command != "" {
		return lang, sp, langIDFor(ext), nil
	}
	return "", ServerSpec{}, "", fmt.Errorf(
		"lsp: the %s server for .%s files is disabled or has no command (lsp.servers.%s in .xdev/config.yml)", lang, ext, lang)
}

// Prewarm starts servers eagerly when lazy is off (lsp.lazy: false): only
// languages whose root marker is present at the session cwd, and never
// blocking startup. With the default lazy=true this is a no-op.
func (m *Manager) Prewarm() {
	if m.cfg.Lazy {
		return
	}
	go func() {
		names := make([]string, 0, len(m.cfg.Servers))
		for n := range m.cfg.Servers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			spec := m.cfg.Servers[name]
			if spec.Disabled || spec.Command == "" || !markerPresent(m.cwd, spec.RootMarkers) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if _, err := m.clientAt(ctx, name, spec, m.cwd); err != nil {
				logx.Debugf("lsp: prewarm %s skipped: %v", name, err)
			}
			cancel()
		}
	}()
}

func markerPresent(dir string, markers []string) bool {
	for _, mk := range markers {
		if _, err := os.Stat(filepath.Join(dir, mk)); err == nil {
			return true
		}
	}
	return false
}

// AllDiagnostics merges the cached publishDiagnostics of every running
// server, keyed by file path.
func (m *Manager) AllDiagnostics() map[string][]Diagnostic {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.servers))
	for _, e := range m.servers {
		clients = append(clients, e.c)
	}
	m.mu.Unlock()
	out := map[string][]Diagnostic{}
	for _, c := range clients {
		for uri, ds := range c.CachedDiagnostics() {
			out[uriToPath(uri)] = ds
		}
	}
	return out
}

// Close stops every server this manager launched.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	entries := make([]*entry, 0, len(m.servers))
	for k, e := range m.servers {
		if e.timer != nil {
			e.timer.Stop()
		}
		entries = append(entries, e)
		delete(m.servers, k)
	}
	m.mu.Unlock()
	for _, e := range entries {
		_ = e.c.Close()
	}
}
