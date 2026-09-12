package memory

// Hindsight is the remote memory backend (M12 #43, omp memory.md
// §Hindsight): it speaks the Hindsight HTTP API directly (stdlib net/http,
// no client dependency):
//
//	POST /v1/default/banks/{bank}/memories/recall   recall
//	POST /v1/default/banks/{bank}/memories          retain (items[])
//	POST /v1/default/banks/{bank}/reflect           reflect
//	GET  /v1/default/banks/{bank}/stats             /memory stats
//	GET  /health                                    /memory diagnose
//
// Scoping defaults to per-project-tagged: writes carry
// project:<repo-root basename, lowercased> and recall matches that tag
// literally (tags_match "any", so untagged global memories still surface).
// per-project puts each repository in its own bank (<base>-<path hash>);
// global shares one bank and sends no tags. Both project modes name the
// project from the repository's primary checkout root, so every linked
// worktree of one repository resolves to the same scope.
//
// A server that cannot be reached never breaks the session: recall returns
// nothing, retains queue locally up to a bound, and one warning names the
// URL (it re-arms after the next success).
//
// Environment overrides (settings hindsight.* are overridden by env, env by
// nothing; omp names win where both exist). The xdev aliases come first:
//
//	HINDSIGHT_URL              | HINDSIGHT_API_URL              base URL
//	HINDSIGHT_TOKEN            | HINDSIGHT_API_TOKEN            bearer token
//	HINDSIGHT_TIMEOUT          | HINDSIGHT_REQUEST_TIMEOUT_MS   default request timeout (Go duration or seconds)
//	HINDSIGHT_BANK_ID          bank base ("bank")
//	HINDSIGHT_BANK_MISSION     unused server-side hint, carried for parity
//	HINDSIGHT_SCOPING          global | per-project | per-project-tagged
//	HINDSIGHT_RETAIN_MODE      full-session | last-turn
//	HINDSIGHT_RECALL_BUDGET    low | mid | high
//	HINDSIGHT_AUTO_RECALL      bool, default true
//	HINDSIGHT_AUTO_RETAIN      bool, default true
//	HINDSIGHT_DEBUG            bool, default false (request logging)
//	HINDSIGHT_RECALL_MAX_TOKENS        default 1024
//	HINDSIGHT_RECALL_CONTEXT_TURNS     default 1
//	HINDSIGHT_RECALL_MAX_QUERY_CHARS   default 800
//	HINDSIGHT_RETAIN_EVERY_N_TURNS     default 3
//	HINDSIGHT_RECALL_TIMEOUT_MS        default 30000
//	HINDSIGHT_RETAIN_TIMEOUT_MS        default 60000
//	HINDSIGHT_REFLECT_TIMEOUT_MS       default 120000
//
// xdev-only settings keys (no env): hindsight.injectionTokenLimit bounds
// what reaches the prompt/compaction, hindsight.queueLimit bounds the
// offline retain queue, hindsight.recallTTL caches the injected block.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
)

// Backend name and scope defaults.
const (
	BackendHindsight = "hindsight"

	DefaultHindsightURL  = "http://localhost:8888"
	DefaultHindsightBank = "xdev"
	// HindsightTagPrefix names the per-project tag; tags are matched
	// literally, so the value is folded to lower case once, here.
	HindsightTagPrefix = "project:"

	// HindsightScopingTagged is the default scoping mode.
	HindsightScopingTagged = "per-project-tagged"
	HindsightRetainFull    = "full-session"
	HindsightRetainLast    = "last-turn"
)

// Defaults for the behaviour knobs (omp parity) and the xdev bounds.
const (
	DefaultHindsightRecallMaxTokens     = 1024
	DefaultHindsightRetainEveryNTurns   = 3
	DefaultHindsightRecallMaxQueryChars = 800
	DefaultHindsightRecallContextTurns  = 1
	// DefaultHindsightInjectionTokenLimit bounds the recalled block injected
	// into the prompt or appended to a compaction summary (~4 chars/token).
	DefaultHindsightInjectionTokenLimit = 1024
	// DefaultHindsightQueueLimit bounds retains kept while the server is
	// unreachable; the oldest queued retain is dropped past the bound.
	DefaultHindsightQueueLimit = 8
	// DefaultHindsightRecallTTL caches the injected block: the prompt is
	// rebuilt on every user turn, and a recalled block must stay stable
	// (prompt cache) and cheap.
	DefaultHindsightRecallTTL = 60 * time.Second
	// DefaultHindsightSummaryCapChars bounds /memory view and memory://root.
	DefaultHindsightSummaryCapChars = 6000
	// maxRecallTurns bounds the remembered turn window the recall query is
	// built from (recallContextTurns is clamped to it).
	maxRecallTurns = 8

	approxCharsPerToken = 4

	// hindsightHealthCap bounds the /health body echoed by /memory diagnose.
	hindsightHealthCap = 200
	// hindsightErrCap bounds a server error body echoed into a message.
	hindsightErrCap = 240
)

// HindsightConfig is the resolved backend configuration. Zero values take
// the defaults above; NewHindsight normalizes a copy.
type HindsightConfig struct {
	URL    string
	Token  string
	BankID string
	// BankMission is carried for parity with omp's hindsight.bankMission
	// (the server owns its use) and reported by /memory diagnose.
	BankMission string
	// Scoping is global | per-project | per-project-tagged.
	Scoping string
	// RetainMode is full-session | last-turn: what one cadence retain
	// carries (every user turn since the last retain, or the last one).
	RetainMode string
	// RecallBudget is low | mid | high (the server's search depth).
	RecallBudget string
	// AutoRecall recalls once at the first turn and AutoRetain retains on
	// the RetainEveryNTurns cadence. nil = the default (both on), so a zero
	// config behaves like omp's defaults and an explicit false is
	// expressible through settings and env.
	AutoRecall *bool
	AutoRetain *bool
	Debug      bool

	RecallMaxTokens     int
	RecallContextTurns  int
	RecallMaxQueryChars int
	RetainEveryNTurns   int

	InjectionTokenLimit int
	QueueLimit          int
	SummaryCapChars     int
	RecallTTL           time.Duration

	RequestTimeout time.Duration
	RecallTimeout  time.Duration
	RetainTimeout  time.Duration
	ReflectTimeout time.Duration

	// ProjectRoot anchors the project scope. Empty resolves the cwd's
	// repository root.
	ProjectRoot string
	// HTTPClient is the transport (tests inject the httptest client).
	HTTPClient *http.Client
	// Logf receives the one-time unreachable warning and debug lines.
	Logf func(format string, args ...any)
	// Now is the clock (tests).
	Now func() time.Time
}

// HindsightConfigFromSettings resolves the backend config: built-in
// defaults ← settings.hindsight.* ← HINDSIGHT_* environment variables (see
// the package comment for the full table; env wins). cwd anchors the
// project scope when it is non-empty.
func HindsightConfigFromSettings(settings *config.Settings, cwd string) HindsightConfig {
	var cfg HindsightConfig
	if settings != nil {
		h := settings.Hindsight
		cfg.URL = h.APIURL
		cfg.Token = h.APIToken
		cfg.BankID = h.BankID
		cfg.BankMission = h.BankMission
		cfg.Scoping = h.Scoping
		cfg.RetainMode = h.RetainMode
		cfg.RecallBudget = h.RecallBudget
		cfg.AutoRecall, cfg.AutoRetain = h.AutoRecall, h.AutoRetain
		cfg.Debug = h.Debug
		cfg.RecallMaxTokens = h.RecallMaxTokens
		cfg.RecallContextTurns = h.RecallContextTurns
		cfg.RecallMaxQueryChars = h.RecallMaxQueryChars
		cfg.RetainEveryNTurns = h.RetainEveryNTurns
		cfg.InjectionTokenLimit = h.InjectionTokenLimit
		cfg.QueueLimit = h.QueueLimit
		cfg.SummaryCapChars = h.SummaryCapChars
		cfg.RecallTTL = time.Duration(h.RecallTTLSeconds) * time.Second
		cfg.RequestTimeout = time.Duration(h.RequestTimeoutMS) * time.Millisecond
		cfg.RecallTimeout = time.Duration(h.RecallTimeoutMS) * time.Millisecond
		cfg.RetainTimeout = time.Duration(h.RetainTimeoutMS) * time.Millisecond
		cfg.ReflectTimeout = time.Duration(h.ReflectTimeoutMS) * time.Millisecond
	}
	cfg.applyEnv(os.Getenv)
	cfg.ProjectRoot = cwd
	return cfg
}

// applyEnv overlays the HINDSIGHT_* environment variables (get is os.Getenv;
// tests pass a map lookup). An unparsable value is ignored, like omp's
// loader: a typo in an env var must not take the backend down.
func (c *HindsightConfig) applyEnv(get func(string) string) {
	if v, ok := envString(get, "HINDSIGHT_URL", "HINDSIGHT_API_URL"); ok {
		c.URL = v
	}
	if v, ok := envString(get, "HINDSIGHT_TOKEN", "HINDSIGHT_API_TOKEN"); ok {
		c.Token = v
	}
	if v, ok := envString(get, "HINDSIGHT_BANK_ID"); ok {
		c.BankID = v
	}
	if v, ok := envString(get, "HINDSIGHT_BANK_MISSION"); ok {
		c.BankMission = v
	}
	if v, ok := envEnum(get, "HINDSIGHT_SCOPING", "global", "per-project", HindsightScopingTagged); ok {
		c.Scoping = v
	}
	if v, ok := envEnum(get, "HINDSIGHT_RETAIN_MODE", HindsightRetainFull, HindsightRetainLast); ok {
		c.RetainMode = v
	}
	if v, ok := envEnum(get, "HINDSIGHT_RECALL_BUDGET", "low", "mid", "high"); ok {
		c.RecallBudget = v
	}
	if v, ok := envBool(get, "HINDSIGHT_AUTO_RECALL"); ok {
		c.AutoRecall = &v
	}
	if v, ok := envBool(get, "HINDSIGHT_AUTO_RETAIN"); ok {
		c.AutoRetain = &v
	}
	if v, ok := envBool(get, "HINDSIGHT_DEBUG"); ok {
		c.Debug = v
	}
	if v, ok := envInt(get, "HINDSIGHT_RECALL_MAX_TOKENS"); ok && v > 0 {
		c.RecallMaxTokens = v
	}
	if v, ok := envInt(get, "HINDSIGHT_RECALL_CONTEXT_TURNS"); ok && v > 0 {
		c.RecallContextTurns = v
	}
	if v, ok := envInt(get, "HINDSIGHT_RECALL_MAX_QUERY_CHARS"); ok && v > 0 {
		c.RecallMaxQueryChars = v
	}
	if v, ok := envInt(get, "HINDSIGHT_RETAIN_EVERY_N_TURNS"); ok && v > 0 {
		c.RetainEveryNTurns = v
	}
	// HINDSIGHT_TIMEOUT is the xdev alias: a Go duration ("45s") or plain
	// seconds; the *_TIMEOUT_MS vars are milliseconds (omp names).
	if v, ok := envString(get, "HINDSIGHT_TIMEOUT"); ok {
		c.RequestTimeout = parseDurationOrSeconds(v, c.RequestTimeout)
	}
	if v, ok := envInt(get, "HINDSIGHT_REQUEST_TIMEOUT_MS"); ok && v > 0 {
		c.RequestTimeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := envInt(get, "HINDSIGHT_RECALL_TIMEOUT_MS"); ok && v > 0 {
		c.RecallTimeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := envInt(get, "HINDSIGHT_RETAIN_TIMEOUT_MS"); ok && v > 0 {
		c.RetainTimeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := envInt(get, "HINDSIGHT_REFLECT_TIMEOUT_MS"); ok && v > 0 {
		c.ReflectTimeout = time.Duration(v) * time.Millisecond
	}
}

func parseDurationOrSeconds(v string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}

// envString returns the first non-empty alias. The second result reports
// whether any alias was set.
func envString(get func(string) string, names ...string) (string, bool) {
	for _, n := range names {
		if v := strings.TrimSpace(get(n)); v != "" {
			return v, true
		}
	}
	return "", false
}

func envInt(get func(string) string, name string) (int, bool) {
	v, ok := envString(get, name)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// envBool follows omp's rule: true, 1 and yes (case-insensitive) are true;
// any other defined value is false.
func envBool(get func(string) string, name string) (bool, bool) {
	v, ok := envString(get, name)
	if !ok {
		return false, false
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true, true
	}
	return false, true
}

// envEnum accepts only an exactly listed (lower-case) value.
func envEnum(get func(string) string, name string, allowed ...string) (string, bool) {
	v, ok := envString(get, name)
	if !ok {
		return "", false
	}
	v = strings.ToLower(v)
	for _, a := range allowed {
		if v == a {
			return v, true
		}
	}
	return "", false
}

// withDefaults normalizes a config copy: every unset knob takes its default
// and the project scope is resolved.
func (c HindsightConfig) withDefaults() HindsightConfig {
	if strings.TrimSpace(c.URL) == "" {
		c.URL = DefaultHindsightURL
	}
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	if strings.TrimSpace(c.BankID) == "" {
		c.BankID = DefaultHindsightBank
	}
	switch c.Scoping {
	case "global", "per-project", HindsightScopingTagged:
	default:
		c.Scoping = HindsightScopingTagged
	}
	switch c.RetainMode {
	case HindsightRetainFull, HindsightRetainLast:
	default:
		c.RetainMode = HindsightRetainFull
	}
	switch c.RecallBudget {
	case "low", "mid", "high":
	default:
		c.RecallBudget = "mid"
	}
	if c.RecallMaxTokens <= 0 {
		c.RecallMaxTokens = DefaultHindsightRecallMaxTokens
	}
	if c.RecallContextTurns <= 0 {
		c.RecallContextTurns = DefaultHindsightRecallContextTurns
	}
	if c.RecallMaxQueryChars <= 0 {
		c.RecallMaxQueryChars = DefaultHindsightRecallMaxQueryChars
	}
	if c.RetainEveryNTurns <= 0 {
		c.RetainEveryNTurns = DefaultHindsightRetainEveryNTurns
	}
	if c.InjectionTokenLimit <= 0 {
		c.InjectionTokenLimit = DefaultHindsightInjectionTokenLimit
	}
	if c.QueueLimit <= 0 {
		c.QueueLimit = DefaultHindsightQueueLimit
	}
	if c.SummaryCapChars <= 0 {
		c.SummaryCapChars = DefaultHindsightSummaryCapChars
	}
	if c.RecallTTL <= 0 {
		c.RecallTTL = DefaultHindsightRecallTTL
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.RecallTimeout <= 0 {
		c.RecallTimeout = c.RequestTimeout
	}
	if c.RetainTimeout <= 0 {
		c.RetainTimeout = 60 * time.Second
	}
	if c.ReflectTimeout <= 0 {
		c.ReflectTimeout = 120 * time.Second
	}
	if c.Logf == nil {
		c.Logf = func(format string, args ...any) { logx.Errorf(format, args...) }
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.ProjectRoot == "" {
		wd, err := os.Getwd()
		if err == nil {
			c.ProjectRoot = wd
		}
	}
	return c
}

// Hindsight is one session's view of a Hindsight server: the scope (bank +
// project tag), the bounded offline retain queue, and the recall cache.
type Hindsight struct {
	cfg         HindsightConfig
	client      *http.Client
	bank        string
	tag         string // "" when scoping is global
	projectID   string
	projectRoot string

	mu       sync.Mutex
	queue    []retainItem
	cached   string
	cachedAt time.Time
	warned   bool
	warnSink func(msg string)
	flushing bool
	turns    int
	pending  []string // user turns since the last cadence retain
	recent   []string // newest user turns, for the recall query
}

// NewHindsight builds the backend. The returned value is ready to use; no
// request is issued until a recall/retain/diagnose call.
func NewHindsight(cfg HindsightConfig) *Hindsight {
	cfg = cfg.withDefaults()
	root, tag, id := ProjectScope(cfg.ProjectRoot)
	bank := cfg.BankID
	if cfg.Scoping == "per-project" {
		bank += "-" + id
	}
	if cfg.Scoping == "global" {
		tag = ""
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.RequestTimeout}
	}
	return &Hindsight{
		cfg:         cfg,
		client:      client,
		bank:        bank,
		tag:         tag,
		projectID:   id,
		projectRoot: root,
	}
}

// ProjectScope names a project the way omp's Hindsight backend does: the
// repository's primary checkout root (linked worktrees resolve to the same
// root), the tag "project:<lowercased basename>" matched literally, and a
// stable id derived from the root path hash.
func ProjectScope(cwd string) (root, tag, id string) {
	root = findRepoRoot(cwd)
	tag = HindsightTagPrefix + strings.ToLower(filepath.Base(root))
	sum := sha256.Sum256([]byte(root))
	return root, tag, hex.EncodeToString(sum[:])[:12]
}

// findRepoRoot walks up from cwd to the repository root. A .git file means
// a linked worktree: its "gitdir:" line points at <main>/.git/worktrees/
// <name>, so the main checkout's root is the shared scope.
func findRepoRoot(cwd string) string {
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	if cwd == "" {
		return ""
	}
	start := filepath.Clean(cwd)
	if abs, err := filepath.Abs(start); err == nil {
		start = abs
	}
	dir := start
	for {
		git := filepath.Join(dir, ".git")
		if fi, err := os.Stat(git); err == nil {
			if fi.IsDir() {
				return dir
			}
			if main := mainWorktreeRoot(git); main != "" {
				return main
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start // no repository above: cwd is the scope
		}
		dir = parent
	}
}

// mainWorktreeRoot parses a linked worktree's .git file. It returns "" when
// the pointer does not name a worktrees entry.
func mainWorktreeRoot(gitFile string) string {
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	if line == "" {
		return ""
	}
	slashed := strings.ReplaceAll(line, "\\", "/")
	marker := "/worktrees/"
	i := strings.LastIndex(slashed, marker)
	if i < 0 {
		return ""
	}
	gitDir := slashed[:i] // <main>/.git
	if filepath.Base(gitDir) != ".git" {
		return ""
	}
	return filepath.FromSlash(filepath.Dir(gitDir))
}

// --- backend surface (the Store shape the local backend shares) ---

// autoRecall / autoRetain resolve the nil-means-default flags.
func (h *Hindsight) autoRecall() bool {
	return h.cfg.AutoRecall == nil || *h.cfg.AutoRecall
}

func (h *Hindsight) autoRetain() bool {
	return h.cfg.AutoRetain == nil || *h.cfg.AutoRetain
}

// Off reports whether the backend is disabled.
func (h *Hindsight) Off() bool { return h == nil || h.cfg.URL == "" }

// Ensure is a no-op: the store lives on the server, not on disk. It exists
// so the backend satisfies the same surface as the local one.
func (h *Hindsight) Ensure() error { return nil }

// Scope reports the resolved bank and project tag (diagnostics, tests).
func (h *Hindsight) Scope() (bank, tag string) {
	if h == nil {
		return "", ""
	}
	return h.bank, h.tag
}

// Summary is what the prompt saw: the cached recall block, refreshed when
// the cache is cold.
func (h *Hindsight) Summary() string {
	if h.Off() {
		return ""
	}
	// /memory view and memory://root get the larger view cap; the prompt and
	// a compaction summary get the smaller injection cap.
	return capRecall(h.recallText(h.recallQuery()), h.cfg.SummaryCapChars)
}

// GuidanceBlock wraps the recall in the Memory Guidance shape the prompt
// injects, or "" when there is nothing to inject. It also flushes queued
// retains in the background: the prompt is rebuilt at every turn boundary,
// which is exactly when a bounded flush is safe.
func (h *Hindsight) GuidanceBlock() string {
	if h.Off() {
		return ""
	}
	h.RetainAsync()
	s := h.AutoRecall()
	if s == "" {
		return ""
	}
	scope := "bank " + h.bank
	if h.tag != "" {
		scope += ", tag " + h.tag
	}
	return "# Memory Guidance (Hindsight " + scope + ")\n\n" +
		"Heuristic context recalled from earlier sessions — not authoritative. " +
		"When it changes your plan, read the source (memory://root) and cite it.\n\n" + s
}

// SaveLesson retains a lesson. The server owns the store, so this is a
// retain with the lesson context (and it queues when the server is down).
func (h *Hindsight) SaveLesson(text, context string) error {
	if h.Off() {
		return fmt.Errorf("hindsight: backend off")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("hindsight: lesson text is required")
	}
	item := retainItem{Content: text, Tags: h.writeTags(), Timestamp: h.now().UTC().Format(time.RFC3339)}
	if c := collapseWS(context); c != "" {
		item.Context = c
	} else {
		item.Context = "lesson"
	}
	return h.retain(item)
}

// Read resolves a memory:// URL. The remote backend keeps everything behind
// the API, so only the injected view is addressable.
func (h *Hindsight) Read(uri string) (string, error) {
	if h.Off() {
		return "", fmt.Errorf("hindsight: backend off (set memory: hindsight in settings)")
	}
	rest := strings.TrimPrefix(uri, "memory://")
	if rest == uri {
		return "", fmt.Errorf("memory: not a memory URL: %q", uri)
	}
	switch strings.Trim(rest, "/") {
	case "", "root":
		if s := h.Summary(); s != "" {
			return s, nil
		}
		return "(no memories recalled yet)", nil
	default:
		return "", fmt.Errorf("memory: hindsight serves only memory://root (got %q)", rest)
	}
}

// WriteSummary retains a consolidated summary block.
func (h *Hindsight) WriteSummary(text string) error {
	if h.Off() {
		return fmt.Errorf("hindsight: backend off")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return h.retain(retainItem{Content: text, Context: "consolidated summary", Tags: h.writeTags(), Timestamp: h.now().UTC().Format(time.RFC3339)})
}

// Clear drains pending retains, then drops only the local state (queue and
// recall cache). The server-side bank is never deleted from here — omp keeps
// that to the Hindsight UI/API.
func (h *Hindsight) Clear() error {
	if h.Off() {
		return fmt.Errorf("hindsight: backend off")
	}
	_ = h.Flush() // best effort: a dead server must not block the clear
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queue = nil
	h.cached, h.cachedAt = "", time.Time{}
	h.turns, h.pending, h.recent = 0, nil, nil
	return nil
}

// Stats reports server-side bank statistics plus the local queue depth.
func (h *Hindsight) Stats() string {
	if h.Off() {
		return "memory: off"
	}
	out := fmt.Sprintf("memory: hindsight at %s\n  bank %s (%s)", h.cfg.URL, h.bank, h.scopeLabel())
	var raw map[string]any
	if err := h.do(context.Background(), h.cfg.RequestTimeout, http.MethodGet, h.bankPath("/stats"), nil, &raw); err != nil {
		out += "\n  server unreachable: " + oneLine(err.Error())
	} else if b, err := json.Marshal(raw); err == nil {
		out += "\n  server: " + capText(string(b), 400)
	}
	return out + fmt.Sprintf("\n  retain queue %d/%d, turns since retain %d", h.QueueLen(), h.cfg.QueueLimit, h.turnsSinceRetain())
}

// Paths labels the remote scope for /memory view (there are no files). The
// shape matches the local backend's two paths so the caller renders both
// the same way.
func (h *Hindsight) Paths() (string, string) {
	if h.Off() {
		return "", ""
	}
	tag := h.tag
	if tag == "" {
		tag = "(none — global scope)"
	}
	return h.cfg.URL + h.bankPath(""), "tag " + tag
}

// --- recall / retain / reflect ---

// RecallNote is one recalled memory.
type RecallNote struct {
	ID    string
	Text  string
	Type  string
	Tags  []string
	Score float64
}

// recallRequest is the POST .../memories/recall body.
type recallRequest struct {
	Query     string   `json:"query"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Budget    string   `json:"budget,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	TagsMatch string   `json:"tags_match,omitempty"`
}

type recallResult struct {
	ID     string             `json:"id"`
	Text   string             `json:"text"`
	Type   string             `json:"type"`
	Tags   []string           `json:"tags"`
	Scores map[string]float64 `json:"scores"`
}

type recallResponse struct {
	Results []recallResult `json:"results"`
}

// retainItem is one entry of POST .../memories items[].
type retainItem struct {
	Content   string   `json:"content"`
	Context   string   `json:"context,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
}

type retainRequest struct {
	Items []retainItem `json:"items"`
	Async bool         `json:"async"`
}

type retainResponse struct {
	Success      bool     `json:"success"`
	BankID       string   `json:"bank_id"`
	ItemsCount   int      `json:"items_count"`
	Async        bool     `json:"async"`
	OperationIDs []string `json:"operation_ids"`
}

// reflectRequest is the POST .../reflect body.
type reflectRequest struct {
	Query     string   `json:"query"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Budget    string   `json:"budget,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	TagsMatch string   `json:"tags_match,omitempty"`
}

type reflectResponse struct {
	Text string `json:"text"`
}

// Recall searches the bank. The query is capped so a long turn cannot blow
// the server's request budget.
func (h *Hindsight) Recall(query string) ([]RecallNote, error) {
	if h.Off() {
		return nil, fmt.Errorf("hindsight: backend off")
	}
	req := recallRequest{
		Query:     capText(collapseWS(query), h.cfg.RecallMaxQueryChars),
		MaxTokens: h.cfg.RecallMaxTokens,
		Budget:    h.cfg.RecallBudget,
		Tags:      h.readTags(),
		TagsMatch: "any",
	}
	var res recallResponse
	if err := h.do(context.Background(), h.cfg.RecallTimeout, http.MethodPost, h.bankPath("/memories/recall"), req, &res); err != nil {
		return nil, err
	}
	notes := make([]RecallNote, 0, len(res.Results))
	for _, r := range res.Results {
		n := RecallNote{ID: r.ID, Text: r.Text, Type: r.Type, Tags: r.Tags}
		for _, s := range r.Scores {
			if s > n.Score {
				n.Score = s
			}
		}
		notes = append(notes, n)
	}
	return notes, nil
}

// Retain stores text in the bank. When the server is unreachable the item is
// queued locally (bounded) and the error says so, so the session continues.
func (h *Hindsight) Retain(text string) error {
	if h.Off() {
		return fmt.Errorf("hindsight: backend off")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("hindsight: retain text is required")
	}
	return h.retain(retainItem{Content: text, Context: "session retain", Tags: h.writeTags(), Timestamp: h.now().UTC().Format(time.RFC3339)})
}

// Reflect asks the server to answer from memory.
func (h *Hindsight) Reflect(query string) (string, error) {
	if h.Off() {
		return "", fmt.Errorf("hindsight: backend off")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("hindsight: reflect query is required")
	}
	var res reflectResponse
	req := reflectRequest{
		Query:     capText(collapseWS(query), h.cfg.RecallMaxQueryChars),
		MaxTokens: h.cfg.RecallMaxTokens,
		Budget:    h.cfg.RecallBudget,
		Tags:      h.readTags(),
		TagsMatch: "any",
	}
	if err := h.do(context.Background(), h.cfg.ReflectTimeout, http.MethodPost, h.bankPath("/reflect"), req, &res); err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Text), nil
}

// retain sends one item and falls back to the offline queue.
func (h *Hindsight) retain(item retainItem) error {
	req := retainRequest{Items: []retainItem{item}, Async: true}
	var res retainResponse
	if err := h.do(context.Background(), h.cfg.RetainTimeout, http.MethodPost, h.bankPath("/memories"), req, &res); err != nil {
		n := h.enqueue(item)
		return fmt.Errorf("hindsight: %v; retain queued locally (%d/%d) and the session continues", err, n, h.cfg.QueueLimit)
	}
	h.retainSucceeded()
	return nil
}

// --- turn cadence ---

// NoteUserTurn records one user turn: the turn text joins the pending
// buffer and, every RetainEveryNTurns turns, the buffer is queued as one
// retain (full-session mode) or the turn itself is (last-turn mode). The
// queue flush is left to the caller's RetainAsync/GuidanceBlock so the turn
// path never waits on the network.
func (h *Hindsight) NoteUserTurn(text string) {
	if h.Off() || !h.autoRetain() {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turns++
	h.recent = append(h.recent, text)
	if over := len(h.recent) - maxRecallTurns; over > 0 {
		h.recent = append([]string(nil), h.recent[over:]...)
	}
	h.pending = append(h.pending, text)
	if h.turns%h.cfg.RetainEveryNTurns != 0 {
		return
	}
	batch := text
	if h.cfg.RetainMode == HindsightRetainFull {
		batch = strings.Join(h.pending, "\n\n")
	}
	h.pending = nil
	h.enqueueLocked(retainItem{Content: batch, Context: "session retain", Tags: h.writeTagsLocked(), Timestamp: h.now().UTC().Format(time.RFC3339)})
}

// RetainAsync flushes the queue in the background: it is a no-op when the
// queue is empty or a flush is already in flight, and the session never
// waits for it.
func (h *Hindsight) RetainAsync() {
	if h.Off() {
		return
	}
	h.mu.Lock()
	if len(h.queue) == 0 || h.flushing {
		h.mu.Unlock()
		return
	}
	h.flushing = true
	h.mu.Unlock()
	go func() {
		defer func() {
			h.mu.Lock()
			h.flushing = false
			h.mu.Unlock()
		}()
		if err := h.Flush(); err != nil {
			h.warn(err)
		}
	}()
}

// Flush sends every queued retain, oldest first, and stops at the first
// failure (the rest stay queued for the next boundary).
func (h *Hindsight) Flush() error {
	if h.Off() {
		return fmt.Errorf("hindsight: backend off")
	}
	for {
		h.mu.Lock()
		if len(h.queue) == 0 {
			h.mu.Unlock()
			return nil
		}
		item := h.queue[0]
		h.mu.Unlock()
		var res retainResponse
		if err := h.do(context.Background(), h.cfg.RetainTimeout, http.MethodPost, h.bankPath("/memories"),
			retainRequest{Items: []retainItem{item}, Async: true}, &res); err != nil {
			return err
		}
		h.retainSucceeded()
		h.mu.Lock()
		if len(h.queue) > 0 {
			h.queue = h.queue[1:]
		}
		h.mu.Unlock()
	}
}

// EndSession is the session boundary (omp: "at agent end, schedule
// cadence-based retention and flush the retain queue"): the user turns that
// have not tripped the cadence yet become one final retain — a one-shot run
// must not be lost — and the queue is then drained. The drain is bounded by
// the retain timeout: an unreachable server leaves the queue behind and
// never hangs the exit.
func (h *Hindsight) EndSession() error {
	if h.Off() {
		return nil
	}
	h.mu.Lock()
	if h.autoRetain() && len(h.pending) > 0 {
		batch := strings.Join(h.pending, "\n\n")
		h.pending = nil
		h.enqueueLocked(retainItem{Content: batch, Context: "session retain", Tags: h.writeTagsLocked(), Timestamp: h.now().UTC().Format(time.RFC3339)})
	}
	h.mu.Unlock()
	return h.Flush()
}

// CompactionContext is the extra context a compaction summary sees: the
// recalled memories, queried with the most recent user turn.
func (h *Hindsight) CompactionContext() string {
	if h.Off() {
		return ""
	}
	return capRecall(h.recallText(h.recallQuery()), h.injectionChars())
}

// Enqueue is the /memory enqueue action: it queues the pending turns as a
// retain for the next boundary and flushes what is already queued. Both
// halves are bounded and never fatal.
func (h *Hindsight) Enqueue() (string, error) {
	if h.Off() {
		return "", fmt.Errorf("hindsight: backend off")
	}
	h.mu.Lock()
	var batch string
	if len(h.pending) > 0 {
		batch = strings.Join(h.pending, "\n\n")
		h.pending = nil
		h.enqueueLocked(retainItem{Content: batch, Context: "manual retain", Tags: h.writeTagsLocked(), Timestamp: h.now().UTC().Format(time.RFC3339)})
	}
	queued := len(h.queue)
	h.mu.Unlock()
	out := fmt.Sprintf("hindsight: %d retain(s) queued", queued)
	if batch != "" {
		out += fmt.Sprintf(" (%d bytes of session turns)", len(batch))
	}
	if err := h.Flush(); err != nil {
		return out + fmt.Sprintf("; flush failed: %v (%d still queued)", err, h.QueueLen()), nil
	}
	return out + "; queue flushed", nil
}

// Diagnose is the /memory diagnose dump: resolved config, scope, health.
func (h *Hindsight) Diagnose() string {
	if h.Off() {
		return "memory: off"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "hindsight backend\n  url      %s\n  bank     %s\n  scope    %s\n  project  %s (%s)\n",
		h.cfg.URL, h.bank, h.scopeLabel(), h.projectRoot, h.projectID)
	if h.cfg.BankMission != "" {
		fmt.Fprintf(&b, "  mission  %s\n", collapseWS(h.cfg.BankMission))
	}
	fmt.Fprintf(&b, "  recall   auto=%t budget=%s maxTokens=%d maxQuery=%d ttl=%s\n",
		h.autoRecall(), h.cfg.RecallBudget, h.cfg.RecallMaxTokens, h.cfg.RecallMaxQueryChars, h.cfg.RecallTTL)
	fmt.Fprintf(&b, "  retain   auto=%t mode=%s every=%d turns queue=%d/%d\n",
		h.autoRetain(), h.cfg.RetainMode, h.cfg.RetainEveryNTurns, h.QueueLen(), h.cfg.QueueLimit)
	fmt.Fprintf(&b, "  timeouts request=%s recall=%s retain=%s reflect=%s\n",
		h.cfg.RequestTimeout, h.cfg.RecallTimeout, h.cfg.RetainTimeout, h.cfg.ReflectTimeout)
	if h.cfg.Token != "" {
		b.WriteString("  auth     bearer token set\n")
	} else {
		b.WriteString("  auth     none\n")
	}
	var health map[string]any
	if err := h.do(context.Background(), h.cfg.RequestTimeout, http.MethodGet, "/health", nil, &health); err != nil {
		fmt.Fprintf(&b, "  health   unreachable: %s\n", oneLine(err.Error()))
	} else if raw, err := json.Marshal(health); err == nil {
		fmt.Fprintf(&b, "  health   ok — %s\n", capText(string(raw), hindsightHealthCap))
	}
	return strings.TrimRight(b.String(), "\n")
}

// QueueLen reports the pending offline retains.
func (h *Hindsight) QueueLen() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queue)
}

// --- injection ---

// AutoRecall is the first-turn recall: the bounded block the prompt injects
// (and the compaction context reuses).
func (h *Hindsight) AutoRecall() string {
	if h.Off() || !h.autoRecall() {
		return ""
	}
	return capRecall(h.recallText(h.recallQuery()), h.injectionChars())
}

// recallText renders the recalled memories, served from the RecallTTL cache
// while it is fresh: the prompt is rebuilt at every turn boundary, so a
// cached block keeps the prompt prefix stable and the server quiet. A
// failure yields "" after one warning (the session continues without it).
func (h *Hindsight) recallText(query string) string {
	h.mu.Lock()
	if h.cachedAfter() {
		cached := h.cached
		h.mu.Unlock()
		return cached
	}
	h.mu.Unlock()
	notes, err := h.Recall(query)
	if err != nil {
		h.warn(err)
		return ""
	}
	block := h.renderNotes(notes)
	h.mu.Lock()
	h.cached, h.cachedAt = block, h.now()
	h.mu.Unlock()
	return block
}

// recallQuery joins the last RecallContextTurns user turns into the query.
func (h *Hindsight) recallQuery() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.cfg.RecallContextTurns
	if n < 1 {
		n = 1
	}
	if n > maxRecallTurns {
		n = maxRecallTurns
	}
	recent := h.recent
	if len(recent) > n {
		recent = recent[len(recent)-n:]
	}
	return strings.Join(recent, "\n")
}

// cachedAfter reports whether the cached block is still fresh (caller holds
// the lock).
func (h *Hindsight) cachedAfter() bool {
	return h.cachedAt.After(h.now().Add(-h.cfg.RecallTTL))
}

// capRecall truncates a recalled block with a marker that names the remote
// backend (capText's marker points at the local backend's MEMORY.md file).
func capRecall(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n… (recall truncated; read memory://root for the rest)"
}

func (h *Hindsight) renderNotes(notes []RecallNote) string {
	var b strings.Builder
	for _, n := range notes {
		t := collapseWS(n.Text)
		if t == "" {
			continue
		}
		b.WriteString("- " + t + "\n")
	}
	return strings.TrimSpace(b.String())
}

func (h *Hindsight) injectionChars() int { return h.cfg.InjectionTokenLimit * approxCharsPerToken }

// --- queue / warnings ---

// enqueueLocked adds an item to the bounded queue, dropping the oldest when
// the bound is reached (caller holds the lock).
func (h *Hindsight) enqueueLocked(item retainItem) {
	if h.cfg.QueueLimit <= 0 {
		return
	}
	h.queue = append(h.queue, item)
	if over := len(h.queue) - h.cfg.QueueLimit; over > 0 {
		h.queue = append([]retainItem(nil), h.queue[over:]...)
	}
}

func (h *Hindsight) enqueue(item retainItem) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enqueueLocked(item)
	return len(h.queue)
}

func (h *Hindsight) retainSucceeded() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.warned = false // a reachable server re-arms the warning
}

// SetWarnSink routes the one-time unreachable warning somewhere the user
// actually sees it. The default is the process logger, which print mode
// leaves off, so cmd points this at stderr there and the TUI can route it
// into a system block. The sink receives the whole message.
func (h *Hindsight) SetWarnSink(f func(msg string)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.warnSink = f
}

// warn reports the unreachable server once per outage, naming the URL: a
// degraded backend must be visible, and must not spam every turn.
func (h *Hindsight) warn(err error) {
	msg := fmt.Sprintf("hindsight: %s unreachable (%s) — using memory without it; retains queue locally", h.cfg.URL, oneLine(err.Error()))
	h.mu.Lock()
	first, sink := !h.warned, h.warnSink
	h.warned = true
	h.mu.Unlock()
	if !first {
		return
	}
	if sink != nil {
		sink(msg)
		return
	}
	h.cfg.Logf("%s", msg)
}

// --- http plumbing ---

// bankPath builds a path under the scoped bank.
func (h *Hindsight) bankPath(suffix string) string {
	return "/v1/default/banks/" + url.PathEscape(h.bank) + suffix
}

// readTags are the tags a recall matches; writeTags the tags a retain
// carries. Global scoping sends none.
func (h *Hindsight) readTags() []string {
	if h.tag == "" {
		return nil
	}
	return []string{h.tag}
}

func (h *Hindsight) writeTags() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writeTagsLocked()
}

func (h *Hindsight) writeTagsLocked() []string {
	if h.tag == "" {
		return nil
	}
	return []string{h.tag}
}

func (h *Hindsight) scopeLabel() string {
	if h.tag == "" {
		return h.cfg.Scoping + ", no tags"
	}
	return h.cfg.Scoping + ", tag " + h.tag
}

func (h *Hindsight) now() time.Time {
	if h.cfg.Now == nil {
		return time.Now()
	}
	return h.cfg.Now()
}

func (h *Hindsight) turnsSinceRetain() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pending)
}

// do performs one JSON request with the operation's timeout and decodes the
// response into out (which may be nil). A non-2xx status is an error
// carrying a bounded slice of the body: the server's own message is the
// useful part.
func (h *Hindsight) do(ctx context.Context, timeout time.Duration, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.cfg.URL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if h.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	}
	if h.cfg.Debug {
		h.cfg.Logf("hindsight: %s %s", method, path)
	}
	client := *h.client
	if timeout > 0 {
		client.Timeout = timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, capText(collapseWS(string(raw)), hindsightErrCap))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// oneLine collapses whitespace for a single-line message.
func oneLine(s string) string { return collapseWS(s) }
