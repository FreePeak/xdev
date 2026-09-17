package tool

// GitHub tool (M13 #49, research parity-tools-providers §D): an
// op-dispatch wrapper over the `gh` CLI.
//
// xdev never embeds a GitHub client and never builds a shell command line:
// every argument travels as a discrete argv entry, so a repository name, a
// PR title, or a search query can never become shell syntax. Authentication
// is the CLI's business (`gh auth login`); the environment is passed through
// unchanged so the user's GH_TOKEN still works, and the classic stale-token
// 401 footgun is named in the error text when gh reports it.
//
// pr_checkout materializes a PR as a git worktree under the session temp
// dir — it never switches the working tree — and pr_push only pushes from
// a worktree this tool created in the same session.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// ghCommandTimeout bounds a single gh/git invocation.
	ghCommandTimeout = 5 * time.Minute
	// ghOutputCap bounds captured stdout+stderr (the bash tool's 8 MiB rule).
	ghOutputCap = 8 << 20
	// ghSearchLimitDefault / ghSearchLimitMax mirror omp's search clamping.
	ghSearchLimitDefault = 10
	ghSearchLimitMax     = 50
	// ghRunTailDefault / ghRunTailMax bound the failed-log tail of run_watch.
	ghRunTailDefault = 15
	ghRunTailMax     = 200
	// ghWatchTimeout bounds the whole run_watch poll loop.
	ghWatchTimeout = 10 * time.Minute
)

// githubOps is the dispatch surface; keep in sync with Execute's switch.
var githubOps = []string{
	"repo_view", "file_read", "pr_create", "pr_checkout", "pr_push",
	"search_issues", "search_prs", "search_code", "search_commits",
	"search_repos", "run_watch",
}

// GithubTool dispatches GitHub operations over the `gh` CLI. One instance
// serves a session: the checkout registry and the pr:///issue:// cache are
// per-instance, so build it once (newToolRegistry) and share it.
type GithubTool struct {
	// CWD is the session working directory: repository resolution and all
	// gh/git invocations run from here (git worktree operations especially).
	CWD string
	// ScratchDir is the parent directory for PR worktrees. Empty means
	// <TMPDIR>/xdev-pr-worktrees (the session temp dir).
	ScratchDir string

	mu        sync.Mutex
	checkouts map[string]githubCheckout // branch, PR number, and PR URL → record
	order     []string                  // branch names in checkout order
	cache     *githubCache
}

// NewGithubTool returns a GithubTool rooted at cwd.
func NewGithubTool(cwd string) *GithubTool {
	return &GithubTool{
		CWD:       cwd,
		checkouts: map[string]githubCheckout{},
		cache:     newGithubCache(githubCacheTTL, githubCacheCap),
	}
}

func (t *GithubTool) Name() string { return "github" }

func (t *GithubTool) Description() string {
	return "GitHub via gh: repo_view, file_read, pr_create, pr_checkout (worktree), pr_push, search_issues/prs/code/commits/repos, run_watch; pr://<n> and issue://<n> read URLs"
}

func (t *GithubTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {
      "type": "string",
      "enum": ["repo_view", "file_read", "pr_create", "pr_checkout", "pr_push", "search_issues", "search_prs", "search_code", "search_commits", "search_repos", "run_watch"],
      "description": "operation to run"
    },
    "repo": {"type": "string", "description": "[host/]owner/repo override (default: the current checkout)"},
    "branch": {"type": "string", "description": "branch (repo_view, file_read, pr_push, run_watch default)"},
    "path": {"type": "string", "description": "repository-relative file path (file_read)"},
    "pr": {"type": "string", "description": "PR number, branch, or GitHub URL (pr_checkout; array also accepted); also selects the checkout for pr_push"},
    "force": {"type": "boolean", "description": "pr_checkout: reset an existing worktree/branch"},
    "forceWithLease": {"type": "boolean", "description": "pr_push: push with --force-with-lease"},
    "title": {"type": "string", "description": "pr_create title (required unless fill)"},
    "body": {"type": "string", "description": "pr_create body; passed via --body-file"},
    "base": {"type": "string", "description": "pr_create --base"},
    "head": {"type": "string", "description": "pr_create --head"},
    "draft": {"type": "boolean", "description": "pr_create --draft"},
    "fill": {"type": "boolean", "description": "pr_create: derive title/body from commits (exclusive with title/body)"},
    "reviewer": {"type": "string", "description": "pr_create reviewer login (repeatable via array)"},
    "assignee": {"type": "string", "description": "pr_create assignee login (repeatable via array)"},
    "label": {"type": "string", "description": "pr_create label (repeatable via array)"},
    "query": {"type": "string", "description": "search query (required for search_code)"},
    "since": {"type": "string", "description": "lower date bound (3d, 2w, 2026-01-01, ISO) for search_* except search_code"},
    "until": {"type": "string", "description": "upper date bound (same formats)"},
    "dateField": {"type": "string", "enum": ["created", "updated"], "description": "date qualifier field (default created)"},
    "limit": {"type": "integer", "description": "search result cap, 1..50 (default 10)"},
    "run": {"type": "string", "description": "Actions run id or run URL (run_watch; default: latest run on branch)"},
    "tail": {"type": "integer", "description": "run_watch failed-log tail lines, 1..200 (default 15)"}
  },
  "required": ["op"]
}`)
}

// githubArgs is the input shape. pr/reviewer/assignee/label accept a JSON
// string or an array of strings (ghStrings unmarshals both).
type githubArgs struct {
	Op             string    `json:"op"`
	Repo           string    `json:"repo,omitempty"`
	Branch         string    `json:"branch,omitempty"`
	Path           string    `json:"path,omitempty"`
	PR             ghStrings `json:"pr,omitempty"`
	Force          bool      `json:"force,omitempty"`
	ForceWithLease bool      `json:"forceWithLease,omitempty"`
	Title          string    `json:"title,omitempty"`
	Body           string    `json:"body,omitempty"`
	Base           string    `json:"base,omitempty"`
	Head           string    `json:"head,omitempty"`
	Draft          bool      `json:"draft,omitempty"`
	Fill           bool      `json:"fill,omitempty"`
	Reviewer       ghStrings `json:"reviewer,omitempty"`
	Assignee       ghStrings `json:"assignee,omitempty"`
	Label          ghStrings `json:"label,omitempty"`
	Query          string    `json:"query,omitempty"`
	Since          string    `json:"since,omitempty"`
	Until          string    `json:"until,omitempty"`
	DateField      string    `json:"dateField,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	Run            string    `json:"run,omitempty"`
	Tail           int       `json:"tail,omitempty"`
}

// ghStrings accepts a JSON string, number, or array of those (the omp
// tool's `pr: string | number | string[]` convenience — models emit 7 as
// readily as "7").
type ghStrings []string

func (s *ghStrings) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(b, &items); err != nil {
			return err
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			v, err := ghScalarString(item)
			if err != nil {
				return err
			}
			if v != "" {
				out = append(out, v)
			}
		}
		*s = out
		return nil
	}
	v, err := ghScalarString(b)
	if err != nil {
		return err
	}
	if v != "" {
		*s = []string{v}
	}
	return nil
}

// ghScalarString renders one JSON scalar (string or number) as a string;
// any other shape is an error so a malformed list fails loudly.
func ghScalarString(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("github: expected a string or number, got %s", capText(string(raw), 64))
	}
}

func (t *GithubTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a githubArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("github: bad arguments: %w", err)
	}
	op := strings.TrimSpace(a.Op)
	if op == "" {
		return Result{Text: "github: op is required (one of " + strings.Join(githubOps, ", ") + ")", IsError: true}, nil
	}
	switch op {
	case "repo_view":
		return t.repoView(ctx, a)
	case "file_read":
		return t.fileRead(ctx, a)
	case "pr_create":
		return t.prCreate(ctx, a)
	case "pr_checkout":
		return t.prCheckout(ctx, a)
	case "pr_push":
		return t.prPush(ctx, a)
	case "search_issues", "search_prs", "search_code", "search_commits", "search_repos":
		return t.search(ctx, op, a)
	case "run_watch":
		return t.runWatch(ctx, a)
	default:
		return Result{Text: "github: unknown op " + quote(op) + "; expected one of " + strings.Join(githubOps, ", "), IsError: true}, nil
	}
}

// errResult wraps err as a failed tool result.
func errResult(err error) Result { return Result{Text: err.Error(), IsError: true} }

// ---------------------------------------------------------------------------
// gh / git execution

// gh runs the gh CLI from the session cwd.
func (t *GithubTool) gh(ctx context.Context, args ...string) (string, error) {
	return t.run(ctx, "gh", t.CWD, args)
}

// git runs git from dir ("" = session cwd).
func (t *GithubTool) git(ctx context.Context, dir string, args ...string) (string, error) {
	return t.run(ctx, "git", dir, args)
}

// run resolves the binary and executes it, mapping failures to actionable
// errors. Output is returned raw (callers trim; file_read must not).
func (t *GithubTool) run(ctx context.Context, bin, dir string, args []string) (string, error) {
	path, err := lookPath(bin)
	if err != nil {
		if bin == "gh" {
			return "", errors.New("github: the `gh` CLI is required but was not found on PATH; install it from https://cli.github.com and run `gh auth login`")
		}
		return "", fmt.Errorf("github: %s not found on PATH: %w", bin, err)
	}
	cctx, cancel := context.WithTimeout(ctx, ghCommandTimeout)
	defer cancel()
	stdout, stderr, err := execCapped(cctx, path, args, dir)
	if err != nil {
		switch {
		case cctx.Err() == context.DeadlineExceeded:
			return "", fmt.Errorf("github: %s timed out after %s", bin, ghCommandTimeout)
		case ctx.Err() != nil:
			return "", fmt.Errorf("github: %s cancelled", bin)
		default:
			return "", fmt.Errorf("github: %s: %s", bin, ghFailureText(stderr, err))
		}
	}
	return stdout, nil
}

// execCapped runs a command with discrete argv (never a shell) and bounded
// output capture.
func execCapped(ctx context.Context, bin string, args []string, dir string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// The environment passes through untouched — gh needs HOME and honors
	// the user's GH_TOKEN — except that interactive prompts are disabled so
	// an unattended call can never hang on a TTY question.
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	out, errBuf := &cappedBuf{limit: ghOutputCap}, &cappedBuf{limit: ghOutputCap}
	cmd.Stdout, cmd.Stderr = out, errBuf
	err := cmd.Run()
	if err == nil && (out.truncated || errBuf.truncated) {
		return out.buf.String(), errBuf.buf.String(), fmt.Errorf("output exceeded %d bytes", ghOutputCap)
	}
	return out.buf.String(), errBuf.buf.String(), err
}

// cappedBuf keeps at most limit bytes and records overflow so a runaway
// command cannot balloon memory (the bash tool's bounded-everything rule).
type cappedBuf struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuf) Write(p []byte) (int, error) {
	n := len(p)
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return n, nil
	}
	if n > room {
		c.truncated = true
		p = p[:room]
	}
	c.buf.Write(p)
	return n, nil
}

var _ io.Writer = (*cappedBuf)(nil)

// ghHeadFlagHint is gh's abort for a branch it cannot push for us: with
// GH_PROMPT_DISABLED=1 its "push the branch?" prompt is impossible, so a
// pr_create whose checked-out branch has no remote ref at HEAD dies here.
// prCreate replaces this generic wording with the branch and directory it
// really ran in (prCreateError), which is the part that makes it fixable.
const ghHeadFlagHint = "hint: gh will not push the checked-out branch for you (prompts are disabled), so a branch with no remote ref at HEAD aborts here: push it, or pass head with the branch holding the commits (xdev never pushes your working tree)."

// ghFailureText explains a gh/git failure, naming the footguns worth calling
// out: a stale exported token shadowing `gh auth` (401), a missing login, and
// the --head abort a branch with no remote ref hits. The raw stderr is
// preserved (capped) so nothing is hidden.
func ghFailureText(stderr string, err error) string {
	msg := capText(strings.TrimSpace(stderr), 4096)
	if msg == "" {
		msg = err.Error()
	}
	low := strings.ToLower(msg)
	var hint string
	switch {
	case strings.Contains(msg, "401") || strings.Contains(low, "bad credentials"):
		hint = "hint: gh rejected the credentials (401). An exported GITHUB_TOKEN/GH_TOKEN overrides `gh auth`; retry with `env -u GITHUB_TOKEN gh auth status` or refresh the token."
	case strings.Contains(low, "gh auth login") || strings.Contains(low, "authentication required"):
		hint = "hint: run `gh auth login` (or set GH_TOKEN) first."
	case strings.Contains(low, "not a git repository"):
		hint = "hint: run from a GitHub checkout, or pass repo (owner/repo) explicitly."
	case strings.Contains(low, "--head flag"):
		hint = ghHeadFlagHint
	}
	if hint == "" {
		return msg
	}
	return msg + "\n" + hint
}

// ---------------------------------------------------------------------------
// repo_view / file_read

const ghRepoFields = "nameWithOwner,description,url,defaultBranchRef,isPrivate,isArchived,isFork,stargazerCount,forkCount,primaryLanguage,pushedAt,homepageUrl,repositoryTopics,viewerPermission"

type ghRepoView struct {
	NameWithOwner    string `json:"nameWithOwner"`
	Description      string `json:"description"`
	URL              string `json:"url"`
	DefaultBranchRef *struct {
		Name string `json:"name"`
	} `json:"defaultBranchRef"`
	IsPrivate       bool `json:"isPrivate"`
	IsArchived      bool `json:"isArchived"`
	IsFork          bool `json:"isFork"`
	StargazerCount  int  `json:"stargazerCount"`
	ForkCount       int  `json:"forkCount"`
	PrimaryLanguage *struct {
		Name string `json:"name"`
	} `json:"primaryLanguage"`
	PushedAt         string `json:"pushedAt"`
	HomepageURL      string `json:"homepageUrl"`
	RepositoryTopics []struct {
		Name string `json:"name"`
	} `json:"repositoryTopics"`
	ViewerPermission string `json:"viewerPermission"`
}

func (t *GithubTool) repoView(ctx context.Context, a githubArgs) (Result, error) {
	argv := []string{"repo", "view"}
	if r := normalizeRepo(a.Repo); r != "" {
		argv = append(argv, r)
	}
	if a.Branch != "" {
		argv = append(argv, "--branch", a.Branch)
	}
	argv = append(argv, "--json", ghRepoFields)
	out, err := t.gh(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	var v ghRepoView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return Result{Text: "github: repo_view: unexpected gh output: " + capText(strings.TrimSpace(out), 512), IsError: true}, nil
	}
	return Result{
		Text:    renderRepoView(v),
		Details: map[string]any{"repo": v.NameWithOwner, "url": v.URL},
	}, nil
}

func renderRepoView(v ghRepoView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", v.NameWithOwner)
	if v.Description != "" {
		b.WriteString(v.Description + "\n")
	}
	b.WriteString("\n")
	row := func(k, val string) {
		if val != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, val)
		}
	}
	row("url", v.URL)
	if v.DefaultBranchRef != nil {
		row("default branch", v.DefaultBranchRef.Name)
	}
	if v.IsPrivate {
		row("visibility", "private")
	} else {
		row("visibility", "public")
	}
	row("permission", v.ViewerPermission)
	if v.PrimaryLanguage != nil {
		row("language", v.PrimaryLanguage.Name)
	}
	row("stars", strconv.Itoa(v.StargazerCount))
	row("forks", strconv.Itoa(v.ForkCount))
	var flags []string
	if v.IsArchived {
		flags = append(flags, "archived")
	}
	if v.IsFork {
		flags = append(flags, "fork")
	}
	if len(flags) > 0 {
		row("flags", strings.Join(flags, ", "))
	}
	row("pushed", v.PushedAt)
	row("homepage", v.HomepageURL)
	if len(v.RepositoryTopics) > 0 {
		names := make([]string, 0, len(v.RepositoryTopics))
		for _, tp := range v.RepositoryTopics {
			names = append(names, tp.Name)
		}
		row("topics", strings.Join(names, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// resolveRepo returns the repo to operate on, falling back to the current
// checkout's GitHub repository when the caller omitted one.
func (t *GithubTool) resolveRepo(ctx context.Context, repo string) (string, error) {
	if r := normalizeRepo(repo); r != "" {
		return r, nil
	}
	out, err := t.gh(ctx, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return "", err
	}
	r := strings.TrimSpace(out)
	if r == "" {
		return "", errors.New("github: cannot resolve the repository from the current checkout; pass repo (owner/repo) explicitly")
	}
	return r, nil
}

func (t *GithubTool) fileRead(ctx context.Context, a githubArgs) (Result, error) {
	p := strings.TrimSpace(a.Path)
	if p == "" {
		return Result{Text: "github: file_read requires path (repository-relative)", IsError: true}, nil
	}
	if strings.HasPrefix(p, "/") {
		return Result{Text: "github: file_read path must be repository-relative (no leading /)", IsError: true}, nil
	}
	repo, err := t.resolveRepo(ctx, a.Repo)
	if err != nil {
		return errResult(err), nil
	}
	argv := []string{
		"api", "/repos/" + splitRepoRef(repo) + "/contents/" + encodeGithubPath(p),
		"--method", "GET",
		"-H", "Accept: application/vnd.github.raw+json",
	}
	if a.Branch != "" {
		argv = append(argv, "-f", "ref="+a.Branch)
	}
	// Raw bytes come back verbatim (no trimming): file content is content.
	out, err := t.gh(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	details := map[string]any{"repo": repo, "path": p}
	if a.Branch != "" {
		details["branch"] = a.Branch
	}
	return Result{Text: out, Details: details}, nil
}

// encodeGithubPath percent-encodes each path segment independently so a
// file named "a b.md" or "c#d.md" survives the API URL.
func encodeGithubPath(p string) string {
	segs := strings.Split(filepath.ToSlash(p), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// normalizeRepo cleans a "[host/]owner/repo" (or https URL) reference:
// trims, strips a URL prefix and a trailing .git. Empty stays empty.
func normalizeRepo(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if i := strings.Index(ref, "://"); i >= 0 {
		u, err := url.Parse(ref)
		if err != nil || u.Path == "" {
			return ""
		}
		host := u.Hostname()
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(segs) < 2 {
			return ""
		}
		ref = host + "/" + segs[0] + "/" + segs[1]
	}
	ref = strings.TrimSuffix(ref, ".git")
	return strings.Trim(ref, "/")
}

// splitRepoRef drops a host prefix naming the default host: `gh api`
// endpoints never carry a host (enterprise repos keep theirs).
func splitRepoRef(ref string) string {
	parts := strings.Split(ref, "/")
	if len(parts) == 3 && strings.EqualFold(parts[0], "github.com") {
		return parts[1] + "/" + parts[2]
	}
	return ref
}

// ---------------------------------------------------------------------------
// pr_create

func (t *GithubTool) prCreate(ctx context.Context, a githubArgs) (Result, error) {
	if a.Fill && (strings.TrimSpace(a.Title) != "" || a.Body != "") {
		return Result{Text: "github: pr_create accepts either fill or title/body, not both", IsError: true}, nil
	}
	if !a.Fill && strings.TrimSpace(a.Title) == "" {
		return Result{Text: "github: pr_create requires title (or fill:true)", IsError: true}, nil
	}
	bodyFile := ""
	if a.Body != "" {
		f, err := os.CreateTemp("", "xdev-pr-body-*.md")
		if err != nil {
			return errResult(fmt.Errorf("github: cannot stage PR body: %w", err)), nil
		}
		if _, err := f.WriteString(a.Body); err != nil {
			f.Close()
			os.Remove(f.Name())
			return errResult(fmt.Errorf("github: cannot stage PR body: %w", err)), nil
		}
		if err := f.Close(); err != nil {
			os.Remove(f.Name())
			return errResult(fmt.Errorf("github: cannot stage PR body: %w", err)), nil
		}
		bodyFile = f.Name()
		defer os.Remove(bodyFile)
	}
	out, err := t.gh(ctx, prCreateArgs(a, bodyFile)...)
	if err != nil {
		return errResult(t.prCreateError(ctx, out, err)), nil
	}
	prURL := firstGithubURL(out)
	text := "created pull request"
	if prURL != "" {
		text = "created pull request: " + prURL
		// Best-effort refresh for a richer summary; a failure here must not
		// fail the create.
		if j, err := t.gh(ctx, "pr", "view", prURL, "--json", "number,title,state,url"); err == nil {
			var v ghPRView
			if json.Unmarshal([]byte(j), &v) == nil && v.Number != 0 {
				text = fmt.Sprintf("created PR #%d %q: %s", v.Number, v.Title, v.URL)
			}
		}
	} else if s := strings.TrimSpace(out); s != "" {
		text = s
	}
	return Result{Text: text, Details: map[string]any{"url": prURL}}, nil
}

// prCreateArgs builds the gh argv for pr_create (bodyFile stages a
// non-empty body; "" means an empty --body, which suppresses the
// interactive editor).
func prCreateArgs(a githubArgs, bodyFile string) []string {
	argv := []string{"pr", "create"}
	if a.Fill {
		argv = append(argv, "--fill")
	}
	if a.Title != "" {
		argv = append(argv, "--title", a.Title)
	}
	switch {
	case bodyFile != "":
		argv = append(argv, "--body-file", bodyFile)
	case !a.Fill:
		argv = append(argv, "--body", "")
	}
	if a.Base != "" {
		argv = append(argv, "--base", a.Base)
	}
	if a.Head != "" {
		argv = append(argv, "--head", a.Head)
	}
	if a.Draft {
		argv = append(argv, "--draft")
	}
	for _, r := range a.Reviewer {
		argv = append(argv, "--reviewer", r)
	}
	for _, r := range a.Assignee {
		argv = append(argv, "--assignee", r)
	}
	for _, l := range a.Label {
		argv = append(argv, "--label", l)
	}
	if r := normalizeRepo(a.Repo); r != "" {
		argv = append(argv, "--repo", r)
	}
	return argv
}

// prCreateError explains a failed pr_create: when gh aborted on the
// unpushed-branch footgun (ghHeadFlagHint) it names the branch and directory
// gh really ran in — the session cwd is not necessarily the checkout holding
// the commits, and without both facts the hint cannot be acted on. The argv is
// untouched (xdev never pushes a working tree); only the diagnosis improves.
func (t *GithubTool) prCreateError(ctx context.Context, stderr string, err error) error {
	msg := ghFailureText(stderr, err)
	if !strings.Contains(msg, ghHeadFlagHint) {
		return errors.New(msg)
	}
	ran := "pr_create ran in " + orDash(t.CWD)
	if out, gerr := t.git(ctx, t.CWD, "rev-parse", "--abbrev-ref", "HEAD"); gerr == nil {
		if branch := strings.TrimSpace(out); branch != "" && branch != "HEAD" {
			ran += " on branch " + strconv.Quote(branch)
		}
	}
	return errors.New(msg + "\n" + ran + ".")
}

// firstGithubURL extracts the first https://... URL gh printed (pr create
// ends with the new PR's URL).
func firstGithubURL(s string) string {
	for _, line := range strings.Split(s, "\n") {
		for _, tok := range strings.Fields(line) {
			if strings.HasPrefix(tok, "https://") {
				return strings.TrimRight(tok, ".,)")
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// pr_checkout / pr_push

// githubCheckout records one PR worktree this tool created. pr_push is
// only ever answered from these records: xdev never pushes the model's
// working tree.
type githubCheckout struct {
	PR       int    `json:"pr"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`  // local branch (pr-<n>)
	HeadRef  string `json:"headRef"` // PR head branch (remote name)
	Remote   string `json:"remote"`  // push target (remote name or fork URL)
	Worktree string `json:"worktree"`
	HeadSHA  string `json:"headSha"`
	URL      string `json:"url"`
	Title    string `json:"title"`
}

func (t *GithubTool) scratch() string {
	if t.ScratchDir != "" {
		return t.ScratchDir
	}
	return filepath.Join(os.TempDir(), "xdev-pr-worktrees")
}

func (t *GithubTool) recordCheckout(rec githubCheckout) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, dup := t.checkouts[rec.Branch]; !dup {
		t.order = append(t.order, rec.Branch)
	}
	t.checkouts[rec.Branch] = rec
	t.checkouts[strconv.Itoa(rec.PR)] = rec
	if rec.URL != "" {
		t.checkouts[rec.URL] = rec
	}
}

// lookupCheckout finds the checkout a pr_push refers to: an explicit pr
// identifier first, then a branch, then the only checkout if exactly one
// exists. No record means rejection — pr_push never invents a refspec.
func (t *GithubTool) lookupCheckout(ids []string, branch string) (githubCheckout, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		if v := strings.TrimSpace(id); v != "" {
			keys = append(keys, v)
		}
	}
	if b := strings.TrimSpace(branch); b != "" {
		keys = append(keys, b)
	}
	for _, k := range keys {
		if rec, ok := t.checkouts[k]; ok {
			return rec, true
		}
	}
	if len(keys) == 0 && len(t.order) == 1 {
		return t.checkouts[t.order[0]], true
	}
	return githubCheckout{}, false
}

func (t *GithubTool) prCheckout(ctx context.Context, a githubArgs) (Result, error) {
	ids := []string(a.PR)
	if len(ids) == 0 {
		ids = []string{""} // empty id = the current branch's PR
	}
	var lines []string
	var recs []githubCheckout
	for _, id := range ids {
		rec, err := t.checkoutOne(ctx, a, strings.TrimSpace(id))
		if err != nil {
			return errResult(err), nil
		}
		recs = append(recs, rec)
		lines = append(lines, fmt.Sprintf("PR #%d -> %s (branch %s, head %s)", rec.PR, rec.Worktree, rec.Branch, shortSHA(rec.HeadSHA)))
	}
	details := map[string]any{"checkouts": recs}
	if len(recs) == 1 {
		details["worktreePath"] = recs[0].Worktree
		details["branch"] = recs[0].Branch
		details["headSha"] = recs[0].HeadSHA
	}
	return Result{Text: strings.Join(lines, "\n"), Details: details}, nil
}

func (t *GithubTool) checkoutOne(ctx context.Context, a githubArgs, id string) (githubCheckout, error) {
	var meta ghPRView
	argv := []string{"pr", "view"}
	if id != "" {
		argv = append(argv, id)
	}
	argv = append(argv, "--json", ghPRMetaFields)
	out, err := t.gh(ctx, argv...)
	if err != nil {
		return githubCheckout{}, err
	}
	if err := json.Unmarshal([]byte(out), &meta); err != nil {
		return githubCheckout{}, fmt.Errorf("github: pr_checkout: unexpected gh output: %s", capText(strings.TrimSpace(out), 512))
	}
	if meta.Number == 0 {
		return githubCheckout{}, errors.New("github: pr_checkout: could not resolve the PR number")
	}
	dir := filepath.Join(t.scratch(), fmt.Sprintf("pr-%d", meta.Number))
	if _, err := os.Stat(dir); err == nil {
		if !a.Force {
			return githubCheckout{}, fmt.Errorf("github: worktree %s already exists; pass force:true to reset it", dir)
		}
		// Clearing the old worktree is what makes a re-checkout possible at
		// all: git refuses to update a branch that is checked out in a
		// worktree, and `worktree add` refuses a path that still exists
		// (verified against git 2.5x — --force alone is not enough).
		if _, err := t.git(ctx, t.CWD, "worktree", "remove", "--force", dir); err != nil {
			// Not a registered worktree (a stale directory): drop it and
			// prune any registration left behind.
			if rerr := os.RemoveAll(dir); rerr != nil {
				return githubCheckout{}, fmt.Errorf("github: clearing %s: %w", dir, rerr)
			}
			_, _ = t.git(ctx, t.CWD, "worktree", "prune")
		}
	}
	remote, err := t.checkoutPushTarget(ctx, meta)
	if err != nil {
		return githubCheckout{}, err
	}
	branch := fmt.Sprintf("pr-%d", meta.Number)
	refspec := fmt.Sprintf("refs/pull/%d/head:refs/heads/%s", meta.Number, branch)
	if a.Force {
		refspec = "+" + refspec
	}
	if out, err := t.git(ctx, t.CWD, "fetch", remote, refspec); err != nil {
		return githubCheckout{}, fmt.Errorf("github: fetching PR #%d: %s", meta.Number, ghFailureText(out, err))
	}
	wtArgs := []string{"worktree", "add"}
	if a.Force {
		wtArgs = append(wtArgs, "--force")
	}
	wtArgs = append(wtArgs, dir, branch)
	if out, err := t.git(ctx, t.CWD, wtArgs...); err != nil {
		return githubCheckout{}, fmt.Errorf("github: creating worktree for PR #%d: %s", meta.Number, ghFailureText(out, err))
	}
	rec := githubCheckout{
		PR:       meta.Number,
		Repo:     normalizeRepo(a.Repo),
		Branch:   branch,
		HeadRef:  meta.HeadRefName,
		Remote:   remote,
		Worktree: dir,
		HeadSHA:  meta.HeadRefOid,
		URL:      meta.URL,
		Title:    meta.Title,
	}
	t.recordCheckout(rec)
	return rec, nil
}

// checkoutPushTarget decides where to fetch the PR head from and where a
// later pr_push goes: a cross-repository (fork) PR uses the fork's clone
// URL; a same-repo PR uses the checkout's first git remote.
func (t *GithubTool) checkoutPushTarget(ctx context.Context, meta ghPRView) (string, error) {
	if meta.IsCrossRepository && meta.HeadRepositoryOwner.Login != "" && meta.HeadRepository.Name != "" {
		return fmt.Sprintf("https://github.com/%s/%s.git", meta.HeadRepositoryOwner.Login, meta.HeadRepository.Name), nil
	}
	out, err := t.git(ctx, t.CWD, "remote")
	if err != nil {
		return "", fmt.Errorf("github: resolving git remote: %s", ghFailureText(out, err))
	}
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			return name, nil
		}
	}
	return "origin", nil // conventional default for a remote-less clone
}

func (t *GithubTool) prPush(ctx context.Context, a githubArgs) (Result, error) {
	rec, ok := t.lookupCheckout(a.PR, a.Branch)
	if !ok {
		return Result{Text: "github: pr_push requires a prior pr_checkout in this session; xdev only pushes from the PR worktree it created (never the model's working tree)", IsError: true}, nil
	}
	refspec := "HEAD:" + rec.HeadRef
	if rec.HeadRef == "" {
		refspec = "HEAD"
	}
	argv := []string{"push"}
	if a.ForceWithLease {
		argv = append(argv, "--force-with-lease")
	}
	argv = append(argv, rec.Remote, refspec)
	out, err := t.git(ctx, rec.Worktree, argv...)
	if err != nil {
		return errResult(fmt.Errorf("github: pushing PR #%d: %s", rec.PR, ghFailureText(out, err))), nil
	}
	// The push may have changed the PR; drop every cached read of it.
	t.cache.invalidateNumber(rec.PR)
	text := strings.TrimSpace(out)
	if text == "" {
		text = fmt.Sprintf("pushed %s to %s (PR #%d)", refspec, rec.Remote, rec.PR)
	}
	return Result{Text: text, Details: map[string]any{"pr": rec.PR, "remote": rec.Remote, "refspec": refspec, "worktreePath": rec.Worktree}}, nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ---------------------------------------------------------------------------
// search_*

const (
	ghSearchIssueFields  = "number,title,state,url,repository,updatedAt,author"
	ghSearchPRFields     = "number,title,state,isDraft,url,repository,updatedAt,author"
	ghSearchCodeFields   = "repository,path,sha,url,textMatches"
	ghSearchCommitFields = "sha,commit,repository,url"
	ghSearchRepoFields   = "fullName,description,language,isPrivate,stargazersCount,updatedAt,url"
	ghRunViewFields      = "databaseId,status,conclusion,url,headSha,headBranch,displayTitle,workflowName,jobs"
)

func (t *GithubTool) search(ctx context.Context, op string, a githubArgs) (Result, error) {
	argv, err := searchArgs(op, a)
	if err != nil {
		return errResult(err), nil
	}
	out, err := t.gh(ctx, argv...)
	if err != nil {
		return errResult(err), nil
	}
	text, err := renderSearch(op, out)
	if err != nil {
		return Result{Text: "github: " + op + ": unexpected gh output: " + capText(strings.TrimSpace(out), 512), IsError: true}, nil
	}
	return Result{Text: text}, nil
}

// searchArgs builds the gh search argv. The query travels as ONE argv entry
// (qualifiers appended) — no shell ever sees it, and gh joins the terms.
func searchArgs(op string, a githubArgs) ([]string, error) {
	limit := a.Limit
	if limit <= 0 {
		limit = ghSearchLimitDefault
	}
	if limit > ghSearchLimitMax {
		limit = ghSearchLimitMax
	}
	sub, ok := map[string]string{
		"search_issues":  "issues",
		"search_prs":     "prs",
		"search_code":    "code",
		"search_commits": "commits",
		"search_repos":   "repos",
	}[op]
	if !ok {
		return nil, fmt.Errorf("github: unknown search op %q", op)
	}
	q := strings.TrimSpace(a.Query)
	if op == "search_code" && q == "" {
		return nil, errors.New("github: search_code requires query")
	}
	if a.Since != "" || a.Until != "" {
		if op == "search_code" {
			return nil, errors.New("github: search_code does not accept since/until")
		}
		field := "created"
		switch op {
		case "search_commits":
			field = "committer-date"
		case "search_repos":
			if a.DateField == "updated" {
				field = "pushed"
			}
		default:
			if a.DateField == "updated" {
				field = "updated"
			}
		}
		q = joinQuery(q, dateQualifier(field, a.Since, a.Until))
	}
	switch op {
	case "search_issues":
		q = joinQuery(q, "is:issue")
	case "search_prs":
		q = joinQuery(q, "is:pr")
	}
	var fields string
	switch op {
	case "search_issues":
		fields = ghSearchIssueFields
	case "search_prs":
		fields = ghSearchPRFields
	case "search_code":
		fields = ghSearchCodeFields
	case "search_commits":
		fields = ghSearchCommitFields
	default:
		fields = ghSearchRepoFields
	}
	argv := []string{"search", sub}
	if q != "" {
		argv = append(argv, q)
	}
	argv = append(argv, "--limit", strconv.Itoa(limit), "--json", fields)
	// search_repos is the only search op that never forwards repo: scoping
	// belongs in the query itself.
	if op != "search_repos" {
		if r := normalizeRepo(a.Repo); r != "" {
			argv = append(argv, "--repo", r)
		}
	}
	return argv, nil
}

func joinQuery(q, qualifier string) string {
	if q == "" {
		return qualifier
	}
	return q + " " + qualifier
}

func dateQualifier(field, since, until string) string {
	var parts []string
	if since != "" {
		parts = append(parts, field+":>="+since)
	}
	if until != "" {
		parts = append(parts, field+":<="+until)
	}
	return strings.Join(parts, " ")
}

// renderSearch formats one gh search JSON response as reader-mode lines.
func renderSearch(op, out string) (string, error) {
	switch op {
	case "search_issues", "search_prs":
		var rows []ghSearchIssue
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "no results", nil
		}
		lines := make([]string, 0, len(rows))
		for _, r := range rows {
			state := strings.ToLower(r.State)
			if op == "search_prs" && r.IsDraft {
				state = "draft"
			}
			lines = append(lines, fmt.Sprintf("#%d [%s] %s — %s (%s)", r.Number, state, r.Title, r.URL, r.Repository.NameWithOwner))
		}
		return strings.Join(lines, "\n"), nil
	case "search_code":
		var rows []ghSearchCode
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "no results", nil
		}
		lines := make([]string, 0, len(rows))
		for _, r := range rows {
			line := fmt.Sprintf("%s/%s (%s)", r.Repository.NameWithOwner, r.Path, r.URL)
			if frag := firstTextMatch(r.TextMatches); frag != "" {
				line += "\n  " + truncateLine(frag, 200)
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n"), nil
	case "search_commits":
		var rows []ghSearchCommit
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "no results", nil
		}
		lines := make([]string, 0, len(rows))
		for _, r := range rows {
			lines = append(lines, fmt.Sprintf("%s %s (%s) %s", shortSHA(r.SHA), firstLine(r.Commit.Message), r.Repository.NameWithOwner, r.URL))
		}
		return strings.Join(lines, "\n"), nil
	default: // search_repos
		var rows []ghSearchRepo
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "no results", nil
		}
		lines := make([]string, 0, len(rows))
		for _, r := range rows {
			vis := "public"
			if r.IsPrivate {
				vis = "private"
			}
			line := fmt.Sprintf("%s [%s, %s] ★%d %s", r.FullName, orDash(r.Language), vis, r.StargazersCount, r.URL)
			if r.Description != "" {
				line += "\n  " + truncateLine(r.Description, 200)
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n"), nil
	}
}

type ghSearchIssue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	IsDraft   bool   `json:"isDraft"`
	URL       string `json:"url"`
	UpdatedAt string `json:"updatedAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

type ghSearchCode struct {
	Path       string `json:"path"`
	SHA        string `json:"sha"`
	URL        string `json:"url"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	TextMatches []struct {
		Fragment string `json:"fragment"`
	} `json:"textMatches"`
}

type ghSearchCommit struct {
	SHA    string `json:"sha"`
	URL    string `json:"url"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

type ghSearchRepo struct {
	FullName        string `json:"fullName"`
	Description     string `json:"description"`
	Language        string `json:"language"`
	IsPrivate       bool   `json:"isPrivate"`
	StargazersCount int    `json:"stargazersCount"`
	UpdatedAt       string `json:"updatedAt"`
	URL             string `json:"url"`
}

func firstTextMatch(ms []struct {
	Fragment string `json:"fragment"`
}) string {
	for _, m := range ms {
		if f := strings.TrimSpace(m.Fragment); f != "" {
			return f
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// run_watch

// pollInterval matches gh's own watch pacing: fast for the first minute,
// relaxed after (3s then 15s, as omp).
func pollInterval(elapsed time.Duration) time.Duration {
	if elapsed < time.Minute {
		return 3 * time.Second
	}
	return 15 * time.Second
}

type ghRunView struct {
	DatabaseID   int64  `json:"databaseId"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	URL          string `json:"url"`
	HeadSHA      string `json:"headSha"`
	HeadBranch   string `json:"headBranch"`
	DisplayTitle string `json:"displayTitle"`
	WorkflowName string `json:"workflowName"`
	Jobs         []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"jobs"`
}

func (t *GithubTool) runWatch(ctx context.Context, a githubArgs) (Result, error) {
	runID, err := t.resolveRun(ctx, a)
	if err != nil {
		return errResult(err), nil
	}
	tail := a.Tail
	if tail <= 0 {
		tail = ghRunTailDefault
	}
	if tail > ghRunTailMax {
		tail = ghRunTailMax
	}
	wctx, cancel := context.WithTimeout(ctx, ghWatchTimeout)
	defer cancel()
	timedOut := false
	var run ghRunView
	start := time.Now()
watch:
	for {
		out, err := t.gh(wctx, "run", "view", runID, "--json", ghRunViewFields)
		if err != nil {
			// The watch budget can expire mid-command: still report the last
			// state observed rather than a bare "cancelled".
			if wctx.Err() != nil && ctx.Err() == nil {
				timedOut = true
				break
			}
			return errResult(err), nil
		}
		if err := json.Unmarshal([]byte(out), &run); err != nil {
			return Result{Text: "github: run_watch: unexpected gh output: " + capText(strings.TrimSpace(out), 512), IsError: true}, nil
		}
		if strings.EqualFold(run.Status, "completed") {
			break
		}
		select {
		case <-wctx.Done():
			timedOut = ctx.Err() == nil
			break watch
		case <-time.After(pollInterval(time.Since(start))):
		}
	}
	if timedOut && run.DatabaseID == 0 {
		return errResult(fmt.Errorf("github: run_watch timed out after %s before the run could be read", ghWatchTimeout)), nil
	}
	text, details := t.renderRunWatch(ctx, run, timedOut, tail)
	return Result{Text: text, Details: details}, nil
}

// resolveRun maps the run argument (numeric id or Actions URL) to a run id,
// falling back to the latest run on the branch.
func (t *GithubTool) resolveRun(ctx context.Context, a githubArgs) (string, error) {
	if r := strings.TrimSpace(a.Run); r != "" {
		if n := trailingNumber(r); n != "" {
			return n, nil
		}
		return "", fmt.Errorf("github: run must be a numeric run id or a run URL, got %q", r)
	}
	branch := strings.TrimSpace(a.Branch)
	if branch == "" {
		out, err := t.git(ctx, t.CWD, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", fmt.Errorf("github: resolving the current branch: %s", ghFailureText(out, err))
		}
		branch = strings.TrimSpace(out)
	}
	out, err := t.gh(ctx, "run", "list", "--branch", branch, "--limit", "1", "--json", "databaseId,status,conclusion,url,headSha,workflowName")
	if err != nil {
		return "", err
	}
	var rows []ghSearchRun
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) == 0 {
		if err == nil {
			return "", fmt.Errorf("github: no runs found on branch %q", branch)
		}
		return "", fmt.Errorf("github: run_watch: unexpected gh output: %s", capText(strings.TrimSpace(out), 512))
	}
	return strconv.FormatInt(rows[0].DatabaseID, 10), nil
}

type ghSearchRun struct {
	DatabaseID int64  `json:"databaseId"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
	HeadSHA    string `json:"headSha"`
	Workflow   string `json:"workflowName"`
}

// trailingNumber extracts the last numeric path segment ("123" or
// ".../runs/123" → "123"), "" otherwise.
func trailingNumber(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return s
}

// renderRunWatch formats the final run state; failures get the failed-job
// list and a tailed `--log-failed` capture.
func (t *GithubTool) renderRunWatch(ctx context.Context, run ghRunView, timedOut bool, tail int) (string, map[string]any) {
	var b strings.Builder
	fmt.Fprintf(&b, "run %d %s [%s/%s]\n", run.DatabaseID, orDash(run.WorkflowName), run.Status, orDash(run.Conclusion))
	if run.DisplayTitle != "" {
		fmt.Fprintf(&b, "%s\n", run.DisplayTitle)
	}
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	row("head", shortSHA(run.HeadSHA)+" ("+run.HeadBranch+")")
	row("url", run.URL)
	if timedOut {
		fmt.Fprintf(&b, "watch timed out after %s; last observed status shown above\n", ghWatchTimeout)
	}
	var failedJobs []string
	for _, j := range run.Jobs {
		c := strings.ToLower(j.Conclusion)
		if c != "" && c != "success" && c != "skipped" {
			failedJobs = append(failedJobs, j.Name)
		}
	}
	details := map[string]any{
		"runId":      run.DatabaseID,
		"status":     run.Status,
		"conclusion": run.Conclusion,
	}
	if len(failedJobs) > 0 {
		details["failedJobs"] = failedJobs
		fmt.Fprintf(&b, "failed jobs:\n")
		for _, name := range failedJobs {
			fmt.Fprintf(&b, "  - %s\n", name)
		}
		if logs, err := t.gh(ctx, "run", "view", strconv.FormatInt(run.DatabaseID, 10), "--log-failed"); err == nil {
			fmt.Fprintf(&b, "\nfailed log (last %d lines):\n%s\n", tail, tailLines(logs, tail))
		} else {
			b.WriteString("failed log unavailable: " + err.Error() + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n"), details
}

// tailLines keeps the last n non-empty lines of a log.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// capText bounds one text block, cutting on a rune boundary so a truncated
// body never ends in a broken UTF-8 sequence.
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… [truncated]"
}
