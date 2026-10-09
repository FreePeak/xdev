package tool

// Gmail tool: an op-dispatch wrapper over the `gog` CLI
// (https://github.com/openclaw/gogcli).
//
// xdev never embeds a Gmail client and never builds a shell command line:
// every argument travels as a discrete argv entry, so a query, subject, or
// body can never become shell syntax. Authentication is the CLI's business
// (`gog auth add` / `gog auth credentials set`); the environment is passed
// through unchanged so GOG_ACCOUNT and the keyring still work.
//
// Read ops always request JSON + agent-safe content wrapping. Send/reply
// write the body through a temp file (`--body-file`) so multi-line text
// never has to survive argv quoting.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// gogCommandTimeout bounds a single gog invocation.
	gogCommandTimeout = 2 * time.Minute
	// gogOutputCap bounds captured stdout+stderr (the bash tool's 8 MiB rule).
	gogOutputCap = 8 << 20
	// gmailSearchLimitDefault / gmailSearchLimitMax clamp search results.
	gmailSearchLimitDefault = 10
	gmailSearchLimitMax     = 50
)

// gmailOps is the dispatch surface; keep in sync with Execute's switch.
var gmailOps = []string{
	"search", "get", "thread", "send", "reply", "mark_read", "labels",
}

// GmailTool dispatches Gmail operations over the `gog` CLI.
type GmailTool struct {
	// CWD is the session working directory (attachment paths resolve here).
	CWD string
}

// NewGmailTool returns a GmailTool rooted at cwd.
func NewGmailTool(cwd string) *GmailTool {
	return &GmailTool{CWD: cwd}
}

func (t *GmailTool) Name() string { return "gmail" }

func (t *GmailTool) Description() string {
	return "Gmail via gog: search, get, thread, send, reply, mark_read, labels; auth is `gog auth add`"
}

func (t *GmailTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {
      "type": "string",
      "enum": ["search", "get", "thread", "send", "reply", "mark_read", "labels"],
      "description": "operation to run"
    },
    "query": {"type": "string", "description": "search: Gmail query (is:unread newer_than:7d, from:…, …)"},
    "id": {"type": "string", "description": "message id (get, reply, mark_read) or thread id (thread)"},
    "ids": {"type": "array", "items": {"type": "string"}, "description": "mark_read: message ids (alternative to id)"},
    "to": {"type": "string", "description": "send: recipients (comma-separated)"},
    "cc": {"type": "string", "description": "send/reply: Cc recipients (comma-separated)"},
    "bcc": {"type": "string", "description": "send/reply: Bcc recipients (comma-separated)"},
    "subject": {"type": "string", "description": "send: subject (required unless replying via reply)"},
    "body": {"type": "string", "description": "send/reply: plain-text body"},
    "account": {"type": "string", "description": "account email or gog alias (default: GOG_ACCOUNT / gog default)"},
    "limit": {"type": "integer", "description": "search result cap, 1..50 (default 10)"},
    "format": {"type": "string", "enum": ["full", "metadata", "raw"], "description": "get: message format (default full)"},
    "sanitize": {"type": "boolean", "description": "get/thread: strip HTML/URLs for agent use (default true)"}
  },
  "required": ["op"],
  "additionalProperties": false
}`)
}

// Caps: network I/O; not ConcurrentSafe (shared mailbox mutations). Read ops
// are not declared ReadOnly because the tool also sends mail — plan mode
// must fail closed on the whole surface.
func (t *GmailTool) Caps() Caps {
	return Caps{SideEffect: ScopeNetwork, Tier: TierWrite}
}

type gmailArgs struct {
	Op       string   `json:"op"`
	Query    string   `json:"query"`
	ID       string   `json:"id"`
	IDs      []string `json:"ids"`
	To       string   `json:"to"`
	CC       string   `json:"cc"`
	BCC      string   `json:"bcc"`
	Subject  string   `json:"subject"`
	Body     string   `json:"body"`
	Account  string   `json:"account"`
	Limit    int      `json:"limit"`
	Format   string   `json:"format"`
	Sanitize *bool    `json:"sanitize"`
}

func (t *GmailTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a gmailArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("gmail: bad arguments: %w", err)
	}
	op := strings.TrimSpace(a.Op)
	if op == "" {
		return Result{Text: "gmail: op is required (one of " + strings.Join(gmailOps, ", ") + ")", IsError: true}, nil
	}
	switch op {
	case "search":
		return t.search(ctx, a)
	case "get":
		return t.get(ctx, a)
	case "thread":
		return t.thread(ctx, a)
	case "send":
		return t.send(ctx, a)
	case "reply":
		return t.reply(ctx, a)
	case "mark_read":
		return t.markRead(ctx, a)
	case "labels":
		return t.labels(ctx, a)
	default:
		return Result{Text: "gmail: unknown op " + quote(op) + "; expected one of " + strings.Join(gmailOps, ", "), IsError: true}, nil
	}
}

func (t *GmailTool) search(ctx context.Context, a gmailArgs) (Result, error) {
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return Result{Text: "gmail: search requires query", IsError: true}, nil
	}
	limit := a.Limit
	if limit <= 0 {
		limit = gmailSearchLimitDefault
	}
	if limit > gmailSearchLimitMax {
		limit = gmailSearchLimitMax
	}
	// Flags before `--` so a query that starts with `-` (e.g. -in:inbox) is
	// never parsed as a flag — gog's documented agent-safe shape.
	argv := t.baseArgs(a, "gmail", "search", "--max", strconv.Itoa(limit), "--wrap-untrusted", "--", q)
	out, err := t.gog(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	return Result{Text: strings.TrimSpace(out)}, nil
}

func (t *GmailTool) get(ctx context.Context, a gmailArgs) (Result, error) {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return Result{Text: "gmail: get requires id (message id)", IsError: true}, nil
	}
	format := strings.TrimSpace(a.Format)
	if format == "" {
		format = "full"
	}
	switch format {
	case "full", "metadata", "raw":
	default:
		return Result{Text: "gmail: format must be full, metadata, or raw", IsError: true}, nil
	}
	argv := t.baseArgs(a, "gmail", "get", id, "--format", format)
	if sanitizeOn(a.Sanitize) {
		argv = append(argv, "--sanitize-content", "--wrap-untrusted")
	}
	out, err := t.gog(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	return Result{Text: strings.TrimSpace(out)}, nil
}

func (t *GmailTool) thread(ctx context.Context, a gmailArgs) (Result, error) {
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return Result{Text: "gmail: thread requires id (thread id)", IsError: true}, nil
	}
	argv := t.baseArgs(a, "gmail", "thread", "get", id)
	if sanitizeOn(a.Sanitize) {
		argv = append(argv, "--sanitize-content", "--wrap-untrusted")
	}
	out, err := t.gog(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	return Result{Text: strings.TrimSpace(out)}, nil
}

func (t *GmailTool) send(ctx context.Context, a gmailArgs) (Result, error) {
	to := strings.TrimSpace(a.To)
	subject := strings.TrimSpace(a.Subject)
	body := a.Body
	if to == "" {
		return Result{Text: "gmail: send requires to", IsError: true}, nil
	}
	if subject == "" {
		return Result{Text: "gmail: send requires subject", IsError: true}, nil
	}
	if strings.TrimSpace(body) == "" {
		return Result{Text: "gmail: send requires body", IsError: true}, nil
	}
	bodyFile, cleanup, err := writeBodyFile(body)
	if err != nil {
		return errResult(err), nil
	}
	defer cleanup()
	argv := t.baseArgs(a, "gmail", "send",
		"--to", to,
		"--subject", subject,
		"--body-file", bodyFile,
	)
	if cc := strings.TrimSpace(a.CC); cc != "" {
		argv = append(argv, "--cc", cc)
	}
	if bcc := strings.TrimSpace(a.BCC); bcc != "" {
		argv = append(argv, "--bcc", bcc)
	}
	out, err := t.gog(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	return Result{Text: strings.TrimSpace(out)}, nil
}

func (t *GmailTool) reply(ctx context.Context, a gmailArgs) (Result, error) {
	id := strings.TrimSpace(a.ID)
	body := a.Body
	if id == "" {
		return Result{Text: "gmail: reply requires id (message id)", IsError: true}, nil
	}
	if strings.TrimSpace(body) == "" {
		return Result{Text: "gmail: reply requires body", IsError: true}, nil
	}
	bodyFile, cleanup, err := writeBodyFile(body)
	if err != nil {
		return errResult(err), nil
	}
	defer cleanup()
	argv := t.baseArgs(a, "gmail", "reply", id, "--body-file", bodyFile)
	if cc := strings.TrimSpace(a.CC); cc != "" {
		// gog reply takes repeatable --cc; pass once with comma-joined list.
		argv = append(argv, "--cc", cc)
	}
	if bcc := strings.TrimSpace(a.BCC); bcc != "" {
		argv = append(argv, "--bcc", bcc)
	}
	out, err := t.gog(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	return Result{Text: strings.TrimSpace(out)}, nil
}

func (t *GmailTool) markRead(ctx context.Context, a gmailArgs) (Result, error) {
	ids := append([]string{}, a.IDs...)
	if id := strings.TrimSpace(a.ID); id != "" {
		ids = append(ids, id)
	}
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return Result{Text: "gmail: mark_read requires id or ids", IsError: true}, nil
	}
	argv := t.baseArgs(a, append([]string{"gmail", "mark-read"}, clean...)...)
	out, err := t.gog(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	return Result{Text: strings.TrimSpace(out)}, nil
}

func (t *GmailTool) labels(ctx context.Context, a gmailArgs) (Result, error) {
	argv := t.baseArgs(a, "gmail", "labels", "list")
	out, err := t.gog(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	return Result{Text: strings.TrimSpace(out)}, nil
}

// baseArgs prepends common agent flags and optional --account. cmd is the
// gog subcommand path after the binary (e.g. "gmail", "search", …).
func (t *GmailTool) baseArgs(a gmailArgs, cmd ...string) []string {
	argv := []string{"--json", "--no-input"}
	if acct := strings.TrimSpace(a.Account); acct != "" {
		argv = append(argv, "--account", acct)
	}
	return append(argv, cmd...)
}

// gog runs the gog CLI from the session cwd.
func (t *GmailTool) gog(ctx context.Context, args ...string) (string, error) {
	path, err := lookPath("gog")
	if err != nil {
		return "", errors.New("gmail: the `gog` CLI is required but was not found on PATH; install it with `brew install openclaw/tap/gogcli` (or `go install github.com/openclaw/gogcli/cmd/gog@latest`) and run `gog auth add you@gmail.com --services gmail`")
	}
	cctx, cancel := context.WithTimeout(ctx, gogCommandTimeout)
	defer cancel()
	stdout, stderr, err := execGog(cctx, path, args, t.CWD)
	if err != nil {
		switch {
		case cctx.Err() == context.DeadlineExceeded:
			return "", fmt.Errorf("gmail: gog timed out after %s", gogCommandTimeout)
		case ctx.Err() != nil:
			return "", fmt.Errorf("gmail: gog cancelled")
		default:
			return "", fmt.Errorf("gmail: gog: %s", gogFailureText(stderr, err))
		}
	}
	return stdout, nil
}

// execGog runs gog with discrete argv and bounded capture. Environment
// passes through so GOG_ACCOUNT / keyring work; interactive prompts are
// disabled by the --no-input flag already in argv.
func execGog(ctx context.Context, bin string, args []string, dir string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	out, errBuf := &cappedBuf{limit: gogOutputCap}, &cappedBuf{limit: gogOutputCap}
	cmd.Stdout, cmd.Stderr = out, errBuf
	err := cmd.Run()
	if err == nil && (out.truncated || errBuf.truncated) {
		return out.buf.String(), errBuf.buf.String(), fmt.Errorf("output exceeded %d bytes", gogOutputCap)
	}
	return out.buf.String(), errBuf.buf.String(), err
}

// gogFailureText explains a gog failure, naming the missing-auth footgun.
func gogFailureText(stderr string, err error) string {
	msg := capText(strings.TrimSpace(stderr), 4096)
	if msg == "" {
		msg = err.Error()
	}
	low := strings.ToLower(msg)
	var hint string
	switch {
	case strings.Contains(low, "no account") || strings.Contains(low, "not authenticated") ||
		strings.Contains(low, "auth") && (strings.Contains(low, "required") || strings.Contains(low, "login") || strings.Contains(low, "credentials")):
		hint = "hint: run `gog auth add you@gmail.com --services gmail` (and `gog auth credentials set <client_secret.json>` once) first; or set GOG_ACCOUNT."
	case strings.Contains(low, "token") && (strings.Contains(low, "expired") || strings.Contains(low, "invalid") || strings.Contains(low, "revoked")):
		hint = "hint: re-authorize with `gog auth add you@gmail.com --services gmail`."
	}
	if hint == "" {
		return msg
	}
	return msg + "\n" + hint
}

// sanitizeOn defaults true when the model omits the field — agent reads
// should not pull raw HTML/URLs into context unless asked.
func sanitizeOn(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

// writeBodyFile drops body into a temp file for --body-file. cleanup removes it.
func writeBodyFile(body string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "xdev-gmail-body-*.txt")
	if err != nil {
		return "", nil, fmt.Errorf("gmail: temp body file: %w", err)
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("gmail: write body file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("gmail: close body file: %w", err)
	}
	// Ensure the path is absolute so gog resolves it from any cwd.
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	return path, cleanup, nil
}
