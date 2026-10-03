// Package codegraph talks to a LeanKG code-graph server.
//
// It exists because a graph is worth having on the write path and is not
// worth owning in the agent: xdev stays one CGO-free binary with a bounded
// footprint, and the graph stays in a second process that can be down without
// breaking a session. See
// docs/decisions/code-graph-via-leankg-not-soulmap.md for why the engine is
// not being ported.
//
// Everything here is stdlib net/http. The client speaks LeanKG's REST query
// surface (POST /api/v1/query) and nothing else.
package codegraph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config points the client at one LeanKG server and one project inside it.
type Config struct {
	// BaseURL is the REST listener, e.g. "http://127.0.0.1:9700".
	BaseURL string
	// Project is the LeanKG project selector, sent as ?project=. Empty means
	// the server's default project, which is almost never what a coding
	// session wants — see Verify's contract below.
	Project string
	// ExpectDir is the repository directory this client believes Project
	// resolves to. When set, Verify refuses a server that answers for a
	// different store instead of returning another repository's graph.
	// This exists because a single-project server silently ignores the
	// selector: measured 2026-10-02, ?project=xdev against a server bound to
	// another repo still answered 200 with that repo's elements.
	ExpectDir string
	// Timeout bounds one request. Default 3s.
	Timeout time.Duration
	// MaxResults caps returned hits. Default 20.
	MaxResults int
	// MaxBytes caps one response body. Default 256 KiB — a graph answer that
	// needs more than that is not a tool result, it is context exhaustion.
	MaxBytes int64
}

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = 3 * time.Second
	}
	if c.MaxResults <= 0 {
		c.MaxResults = 20
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 256 << 10
	}
	return c
}

// Client is a LeanKG query client. It is safe for concurrent use.
type Client struct {
	cfg Config
	hc  *http.Client

	// verify caches the once-per-session project check. A coding session asks
	// the same server the same question about identity every turn; asking
	// again would be a request the model pays for in latency and nothing in
	// return.
	verifyMu   sync.Mutex
	verifyErr  error
	verifyDone bool
}

// New returns a client for cfg. A cfg without a BaseURL yields a client whose
// calls report ErrUnconfigured rather than dialling an empty host.
func New(cfg Config) *Client {
	cfg = cfg.withDefaults()
	return &Client{
		cfg: cfg,
		hc:  &http.Client{Timeout: cfg.Timeout},
	}
}

// ErrUnconfigured is returned when no server was configured.
var ErrUnconfigured = fmt.Errorf("codegraph: no server configured")

// UnavailableError reports that the graph cannot be trusted right now: it is
// down, it is slow, or it is answering for a different repository. It is
// deliberately distinct from an empty result — "no callers" and "I looked in
// the wrong repository" must never read the same to the model.
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string {
	return "code graph unavailable: " + e.Reason + " — continue with grep/ast_grep/lsp"
}

// Candidate is one element the ladder returned for a search.
type Candidate struct {
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Type          string `json:"element_type"`
	File          string `json:"file_path"`
	Line          int    `json:"line_start"`
	Language      string `json:"language"`
}

// Locate resolves a symbol name to the elements LeanKG holds for it, using the
// server's own ladder (exact -> fuzzy -> semantic) rather than a rung xdev
// picks. Preferring the server's ranking is the point: it knows which store
// it is bound to, and a rung chosen here would be a guess.
func (c *Client) Locate(ctx context.Context, symbol string, limit int) ([]Candidate, error) {
	if limit <= 0 || limit > c.cfg.MaxResults {
		limit = c.cfg.MaxResults
	}
	raw, err := c.Query(ctx, Request{Query: symbol, Limit: limit})
	if err != nil {
		return nil, err
	}
	var out struct {
		Hits []Candidate `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("codegraph: malformed ladder answer: %w", err)
	}
	return out.Hits, nil
}

// Request is one query-tool payload (LeanKG's QueryRequest).
type Request struct {
	// Action is the query tool action; empty routes down the ladder.
	Action string `json:"action,omitempty"`
	Query  string `json:"query"`
	Limit  int    `json:"limit,omitempty"`
	Args   map[string]any
}

// Query posts one query-tool request and returns the raw JSON body.
//
// The caller gets raw JSON rather than a parsed struct because LeanKG's
// answer is a map with per-action shapes, and pinning that shape here would
// make every LeanKG release an xdev release. The tools that use this are the
// only places that decode.
func (c *Client) Query(ctx context.Context, req Request) ([]byte, error) {
	if c.cfg.BaseURL == "" {
		return nil, ErrUnconfigured
	}
	if err := c.Verify(ctx); err != nil {
		return nil, err
	}
	if req.Limit <= 0 || req.Limit > c.cfg.MaxResults {
		req.Limit = c.cfg.MaxResults
	}
	body, err := json.Marshal(map[string]any{
		"action": req.Action,
		"query":  req.Query,
		"limit":  req.Limit,
		"args":   req.Args,
	})
	if err != nil {
		return nil, fmt.Errorf("codegraph: %w", err)
	}
	u := strings.TrimRight(c.cfg.BaseURL, "/") + "/api/v1/query"
	if c.cfg.Project != "" {
		u += "?project=" + url.QueryEscape(c.cfg.Project)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codegraph: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	res, err := c.hc.Do(httpReq)
	if err != nil {
		// ctx cancellation is the caller's abort, not a broken server, and
		// reporting it as "unavailable" would send the model looking for a
		// dead server that is perfectly fine.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &UnavailableError{Reason: "no answer from " + c.cfg.BaseURL}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, &UnavailableError{Reason: fmt.Sprintf("server said %s", res.Status)}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, c.cfg.MaxBytes+1))
	if err != nil {
		return nil, &UnavailableError{Reason: "response interrupted"}
	}
	if int64(len(raw)) > c.cfg.MaxBytes {
		return nil, &UnavailableError{Reason: "answer too large to return"}
	}
	return raw, nil
}

// Verify checks once per session that the server is up AND answering for the
// repository this client thinks it is talking about.
//
// The second half is not paranoia. A single-project LeanKG server ignores
// ?project= entirely: the request succeeds, the response is a well-formed
// graph, and every element in it belongs to some other repository. For a tool
// whose whole job is "tell me what depends on this file", a confident answer
// from the wrong graph is worse than no answer at all, so the client checks
// /api/v1/status and compares project_dir before it trusts a single result.
//
// ExpectDir empty disables the directory comparison but keeps the health
// probe.
func (c *Client) Verify(ctx context.Context) error {
	c.verifyMu.Lock()
	defer c.verifyMu.Unlock()
	if c.verifyDone {
		return c.verifyErr
	}
	c.verifyDone, c.verifyErr = true, c.verifyOnce(ctx)
	return c.verifyErr
}

func (c *Client) verifyOnce(ctx context.Context) error {
	u := strings.TrimRight(c.cfg.BaseURL, "/") + "/api/v1/status"
	if c.cfg.Project != "" {
		u += "?project=" + url.QueryEscape(c.cfg.Project)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return &UnavailableError{Reason: err.Error()}
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return &UnavailableError{Reason: "no answer from " + c.cfg.BaseURL}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return &UnavailableError{Reason: fmt.Sprintf("server said %s", res.Status)}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return &UnavailableError{Reason: "status response interrupted"}
	}
	if c.cfg.ExpectDir == "" {
		return nil
	}
	var st struct {
		ProjectDir string `json:"project_dir"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return &UnavailableError{Reason: "status response is not the shape this client expects"}
	}
	if st.ProjectDir == "" {
		return &UnavailableError{Reason: "server did not report which repository it is bound to"}
	}
	if !sameDir(st.ProjectDir, c.cfg.ExpectDir) {
		return &UnavailableError{Reason: fmt.Sprintf(
			"server is bound to %s, not %s — the project selector was ignored",
			st.ProjectDir, c.cfg.ExpectDir)}
	}
	return nil
}

// sameDir compares two paths, tolerating a trailing separator and symlinked
// temp dirs (macOS t.TempDir is a symlink into /var, which is why the
// caller-side path is resolved before the comparison).
func sameDir(a, b string) bool {
	return trimDir(a) == trimDir(b)
}

func trimDir(s string) string {
	for len(s) > 1 && strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}
	return s
}
