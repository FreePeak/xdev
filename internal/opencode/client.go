// Package opencode drives the opencode server the user already runs.
//
// Attach-only (R2). This package spawns nothing, signals nothing, and reads
// nothing out of opencode's config directory: every fact it needs comes from
// opencode's own commands (`service status`, `service get password`, `api`) or
// from the loopback URL those commands report. Adding a spawn here would make
// xdev the owner of a lifecycle that is not xdev's.
//
// The control plane goes through `opencode api` so the CLI owns the OpenAPI
// contract and the credentials. The event stream does not: `opencode api GET
// /api/event` yields zero bytes, so streaming needs direct HTTP (Slice 3).
//
// See docs/research/opencode-v2-spec.md for the surface this was written
// against (opencode v2.0.32).
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
)

// Sentinel errors, so callers can branch without string matching.
var (
	// ErrNoServer means opencode is not reachable or not usable. Discovery
	// degrades to this rather than inventing a URL or an empty password (F8).
	ErrNoServer = errors.New("opencode: no usable server")
	// ErrNotJSON means the response was not a JSON object — most often the
	// web-UI shell, which opencode serves with 200 for every path outside
	// /api, /auth/ and /openapi.json. Reading that as "empty" is the silent
	// bug F1 exists to prevent.
	ErrNotJSON = errors.New("opencode: response is not a JSON object")
	// ErrAPIError means opencode returned its own error envelope (a `_tag`
	// such as UnauthorizedError or RouteNotFound).
	ErrAPIError = errors.New("opencode: api returned an error")
	// ErrInvalidRequest means the caller passed something opencode would
	// reject anyway. Caught here so no subprocess is spawned for it.
	ErrInvalidRequest = errors.New("opencode: invalid request")
)

// Exec runs one opencode invocation and returns its stdout. Tests substitute
// it; nil means exec the real binary. It is a function field rather than an
// interface because there is exactly one implementation and the seam exists
// only for tests — the same shape as internal/tts.Speaker.Lookup.
type Exec func(ctx context.Context, args ...string) ([]byte, error)

// Client is a handle on the user's running opencode.
type Client struct {
	// Exec runs the opencode CLI. nil means the real binary.
	Exec Exec
	// Bin is the binary to run. Empty means "opencode" on PATH.
	Bin string
}

// New returns a client that drives the real opencode binary.
func New() *Client { return &Client{} }

func (c *Client) exec(ctx context.Context, args ...string) ([]byte, error) {
	run := c.Exec
	if run == nil {
		bin := c.Bin
		if bin == "" {
			bin = "opencode"
		}
		run = func(ctx context.Context, args ...string) ([]byte, error) {
			var stdout, stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, bin, args...)
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				msg := strings.TrimSpace(stderr.String())
				if msg == "" {
					msg = err.Error()
				}
				return nil, fmt.Errorf("opencode %s: %w: %s",
					strings.Join(args, " "), err, msg)
			}
			return stdout.Bytes(), nil
		}
	}
	return run(ctx, args...)
}

// Endpoint is where the user's opencode is listening, and how to authenticate
// to it. Both fields are filled together or not at all: a half-populated
// Endpoint is worse than none, because a caller would retry with an empty
// password and learn nothing.
type Endpoint struct {
	URL      string
	Password string
}

// Discover asks opencode where it is running. It uses two commands because
// opencode has no single call that returns both halves, and it never reads
// ~/.config/opencode/service.json itself — going through the CLI is what keeps
// this attach-only rather than rummaging (R2).
//
// Any failure yields ErrNoServer and a zero Endpoint, never a guess (F8).
func (c *Client) Discover(ctx context.Context) (Endpoint, error) {
	out, err := c.exec(ctx, "service", "status")
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w: service status: %v", ErrNoServer, err)
	}
	raw := strings.TrimSpace(string(out))
	if !isLoopbackHTTP(raw) {
		return Endpoint{}, fmt.Errorf("%w: service status reported %q, which is not a loopback http url", ErrNoServer, raw)
	}

	out, err = c.exec(ctx, "service", "get", "password")
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w: service get password: %v", ErrNoServer, err)
	}
	pw := strings.TrimSpace(string(out))
	if pw == "" {
		return Endpoint{}, fmt.Errorf("%w: service get password returned nothing", ErrNoServer)
	}
	return Endpoint{URL: raw, Password: pw}, nil
}

// isLoopbackHTTP keeps xdev pointed at a local server. 0.0.0.0 and any routable
// host are refused even though opencode would happily print them.
func isLoopbackHTTP(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// --- the F1 gate -----------------------------------------------------------

// decodeObject is the single place a response becomes a value. It rejects
// anything that is not a JSON object, and rejects opencode's error envelopes,
// so no caller can mistake a web-UI shell for an empty result.
func decodeObject(stdout []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(stdout, &obj); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotJSON, err)
	}
	if obj == nil {
		return nil, fmt.Errorf("%w: null", ErrNotJSON)
	}
	if tag, ok := obj["_tag"]; ok {
		var s string
		_ = json.Unmarshal(tag, &s)
		if s == "" {
			s = "(untagged)"
		}
		return nil, fmt.Errorf("%w: %s", ErrAPIError, s)
	}
	return obj, nil
}

// unwrap pulls the `data` field every opencode response wraps its payload in.
func unwrap(obj map[string]json.RawMessage, v any) error {
	raw, ok := obj["data"]
	if !ok {
		return fmt.Errorf("%w: response has no data field", ErrNotJSON)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: data field: %v", ErrNotJSON, err)
	}
	return nil
}

// --- session types ---------------------------------------------------------

// ModelRef is the model a session or assistant turn ran on.
type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant"`
}

// CacheUsage is opencode's prompt-cache accounting.
type CacheUsage struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

// TokenUsage is the per-session token roll-up.
type TokenUsage struct {
	Input     int64      `json:"input"`
	Output    int64      `json:"output"`
	Reasoning int64      `json:"reasoning"`
	Cache     CacheUsage `json:"cache"`
}

// TimeInfo is a session's timestamps, in ms epoch.
type TimeInfo struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
	Idle    int64 `json:"idle"`
	Viewed  int64 `json:"viewed"`
}

// Location is the working directory a session is scoped to.
type Location struct {
	Directory string `json:"directory"`
}

// PermissionRule is opencode's Permission.Rule. All three fields are required
// there, and Effect is one of allow / deny / ask — opencode rejects anything
// else, so a bad value surfaces as a server error rather than a silent no-op.
type PermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// SessionInfo is one row of opencode's session list. It carries everything a
// roster row needs — title, agent, model, cost, tokens, outcome, idle — so a
// session list never has to read messages.
type SessionInfo struct {
	ID        string     `json:"id"`
	ParentID  string     `json:"parentID"`
	ProjectID string     `json:"projectID"`
	Agent     string     `json:"agent"`
	Model     ModelRef   `json:"model"`
	Cost      float64    `json:"cost"`
	Tokens    TokenUsage `json:"tokens"`
	Outcome   string     `json:"outcome"`
	Time      TimeInfo   `json:"time"`
	Title     string     `json:"title"`
	Location  Location   `json:"location"`
}

// CreateRequest is session.create's body. Directory travels in `location`,
// not as a CLI parameter: the operation declares no query parameters, so a
// `--param directory=…` is silently ignored and opencode falls back to the
// server's own cwd.
//
// Directory is the ergonomic field; Location is what actually goes on the
// wire. Setting Directory fills Location when Location is not given.
type CreateRequest struct {
	ID       string         `json:"id,omitempty"`
	ParentID string         `json:"parentID,omitempty"`
	Title    string         `json:"title,omitempty"`
	Agent    string         `json:"agent,omitempty"`
	Model    *ModelRef      `json:"model,omitempty"`
	Location *Location      `json:"location,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Permissions overrides opencode's ruleset for this session only. Nil
	// leaves opencode's own rules in place, which is the common case.
	Permissions []PermissionRule `json:"permissions,omitempty"`

	// Directory is sugar for Location.Directory. It never appears on the
	// wire itself, so a caller cannot set both and silently disagree.
	Directory string `json:"-"`
}

// MarshalJSON folds Directory into Location when only Directory was set.
func (r CreateRequest) MarshalJSON() ([]byte, error) {
	type alias CreateRequest // no MarshalJSON here, so no recursion
	a := alias(r)
	if a.Location == nil && r.Directory != "" {
		a.Location = &Location{Directory: r.Directory}
	}
	return json.Marshal(a)
}

// ListSessions returns the sessions in a directory, newest first. An empty
// directory means "whatever opencode considers current".
func (c *Client) ListSessions(ctx context.Context, directory string) ([]SessionInfo, error) {
	args := []string{"api", "session.list"}
	if directory != "" {
		args = append(args, "--param", "directory="+directory)
	}
	out, err := c.exec(ctx, args...)
	if err != nil {
		return nil, err
	}
	obj, err := decodeObject(out)
	if err != nil {
		return nil, err
	}
	var rows []SessionInfo
	if err := unwrap(obj, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// CreateSession opens a session on opencode's side. Agent is the context lever
// (the plan's D2): the agent's prompt, model and permissions are opencode's to
// define, xdev only names one.
func (c *Client) CreateSession(ctx context.Context, req CreateRequest) (SessionInfo, error) {
	if strings.TrimSpace(req.Agent) == "" {
		return SessionInfo{}, fmt.Errorf("%w: create needs an agent", ErrInvalidRequest)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	out, err := c.exec(ctx, "api", "session.create", "-d", string(body))
	if err != nil {
		return SessionInfo{}, err
	}
	obj, err := decodeObject(out)
	if err != nil {
		return SessionInfo{}, err
	}
	var row SessionInfo
	if err := unwrap(obj, &row); err != nil {
		return SessionInfo{}, err
	}
	return row, nil
}

// GetSession reads one session's current state.
func (c *Client) GetSession(ctx context.Context, id string) (SessionInfo, error) {
	if strings.TrimSpace(id) == "" {
		return SessionInfo{}, fmt.Errorf("%w: session id is required", ErrInvalidRequest)
	}
	out, err := c.exec(ctx, "api", "session.get", "--param", "sessionID="+id)
	if err != nil {
		return SessionInfo{}, err
	}
	obj, err := decodeObject(out)
	if err != nil {
		return SessionInfo{}, err
	}
	var row SessionInfo
	if err := unwrap(obj, &row); err != nil {
		return SessionInfo{}, err
	}
	return row, nil
}

// promptRequest is session.prompt's body.
type promptRequest struct {
	Text string `json:"text"`
}

// Prompt sends a user turn. opencode runs the turn asynchronously, so this
// returns once the request is accepted, not once the turn finishes.
func (c *Client) Prompt(ctx context.Context, id, text string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: prompt text is empty", ErrInvalidRequest)
	}
	body, err := json.Marshal(promptRequest{Text: text})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	out, err := c.exec(ctx, "api", "session.prompt", "--param", "sessionID="+id, "-d", string(body))
	if err != nil {
		return err
	}
	// The gate still applies: a mistyped operation returns the web-UI shell.
	if _, err := decodeObject(out); err != nil {
		return err
	}
	return nil
}
