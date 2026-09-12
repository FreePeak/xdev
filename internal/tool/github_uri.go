package tool

// pr:// and issue:// reader URLs (M13 #49). Both resolve through the same
// `gh` instance as the github tool, share its bounded cache, and return
// reader-mode text so the model can read an issue or PR the way it reads a
// file: heading, metadata, body, then a capped comment list.
//
// Accepted forms (the host prefix is optional, matching the tool's repo
// argument):
//
//	pr://123                       id in the current checkout's repository
//	issue://owner/repo/123         explicit repository
//	pr://github.com/owner/repo/123 enterprise-safe full form

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// githubCacheTTL is deliberately short: one agent turn is exactly the
	// window where the same PR gets read twice, and a repeat read is cheap
	// to redo after the TTL.
	githubCacheTTL = 30 * time.Second
	// githubCacheCap bounds the cache: 32 rows is a turn's worth of reads.
	githubCacheCap = 32
	// githubReaderBlockCap / githubReaderCommentCap bound the rendered text
	// so a 200-comment thread cannot flood the context window.
	githubReaderBlockCap   = 8 << 10
	githubReaderCommentCap = 20
)

const (
	ghPRMetaFields      = "number,title,url,state,isDraft,headRefName,headRefOid,baseRefName,isCrossRepository,headRepository,headRepositoryOwner"
	ghPRReaderFields    = "number,title,state,isDraft,author,url,createdAt,updatedAt,body,baseRefName,headRefName,labels,comments"
	ghIssueReaderFields = "number,title,state,author,url,createdAt,updatedAt,body,labels,comments"
)

// githubCache is a tiny bounded TTL cache keyed by URL. It exists for one
// reason: a turn that reads pr://49 twice must shell out once.
type githubCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	size    int
	entries map[string]githubCacheEntry
}

type githubCacheEntry struct {
	text string
	at   time.Time
}

func newGithubCache(ttl time.Duration, size int) *githubCache {
	return &githubCache{ttl: ttl, size: size, entries: map[string]githubCacheEntry{}}
}

func (c *githubCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if c.ttl > 0 && time.Since(e.at) > c.ttl {
		delete(c.entries, key)
		return "", false
	}
	return e.text, true
}

func (c *githubCache) put(key, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.size > 0 && len(c.entries) >= c.size {
		// Evict the oldest row. LRU bookkeeping would be nicer; with 32 rows
		// this scan is a handful of comparisons and the cap is what matters.
		oldestKey := ""
		var oldest time.Time
		for k, v := range c.entries {
			if oldestKey == "" || v.at.Before(oldest) {
				oldestKey, oldest = k, v.at
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = githubCacheEntry{text: text, at: time.Now()}
}

// invalidateNumber drops every cached row for one PR/issue number (any repo
// form), so a push is visible to the next read.
func (c *githubCache) invalidateNumber(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	suffix := strconv.Itoa(n)
	for k := range c.entries {
		if strings.HasSuffix(k, "://"+suffix) || strings.HasSuffix(k, "/"+suffix) {
			delete(c.entries, k)
		}
	}
}

// githubURI is a parsed pr:// or issue:// reference.
type githubURI struct {
	Scheme string // "pr" | "issue"
	Number int
	Repo   string // "" = current checkout, else [host/]owner/repo
}

// cacheKey canonicalizes equivalent URIs (pr://49 and pr://049) to one row.
func (r githubURI) cacheKey() string {
	if r.Repo == "" {
		return r.Scheme + "://" + strconv.Itoa(r.Number)
	}
	return r.Scheme + "://" + r.Repo + "/" + strconv.Itoa(r.Number)
}

func parseGithubURI(uri string) (githubURI, error) {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return githubURI{}, fmt.Errorf("github: not a github URL: %s", quote(uri))
	}
	scheme = strings.ToLower(scheme)
	if scheme != "pr" && scheme != "issue" {
		return githubURI{}, fmt.Errorf("github: unsupported scheme %q (want pr:// or issue://)", scheme)
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	num := parts[len(parts)-1]
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return githubURI{}, fmt.Errorf("github: %s:// wants a numeric id, got %q", scheme, num)
	}
	return githubURI{Scheme: scheme, Number: n, Repo: normalizeRepo(strings.Join(parts[:len(parts)-1], "/"))}, nil
}

// RegisterURISchemes installs the pr:// and issue:// read resolvers that
// share this tool's cache. Idempotent: re-registering replaces the resolver.
func (t *GithubTool) RegisterURISchemes() {
	RegisterURIScheme("pr", t.readGithubURI)
	RegisterURIScheme("issue", t.readGithubURI)
}

// readGithubURI resolves a pr:// or issue:// URL to reader-mode text,
// caching the rendered text so a turn that reads the same URL twice shells
// out once. The URI seam has no context, so the command's own timeout is
// the bound.
func (t *GithubTool) readGithubURI(uri string) (string, error) {
	ref, err := parseGithubURI(uri)
	if err != nil {
		return "", err
	}
	key := ref.cacheKey()
	if text, ok := t.cache.get(key); ok {
		return text, nil
	}
	argv := []string{"pr", "view"}
	fields := ghPRReaderFields
	if ref.Scheme == "issue" {
		argv, fields = []string{"issue", "view"}, ghIssueReaderFields
	}
	argv = append(argv, strconv.Itoa(ref.Number))
	if ref.Repo != "" {
		argv = append(argv, "--repo", ref.Repo)
	}
	argv = append(argv, "--json", fields)
	out, err := t.gh(context.Background(), argv...)
	if err != nil {
		return "", err
	}
	var v ghPRView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("github: %s: unexpected gh output: %s", uri, capText(strings.TrimSpace(out), 512))
	}
	text := renderGithubReader(ref.Scheme, v)
	t.cache.put(key, text)
	return text, nil
}

type ghAuthor struct {
	Login string `json:"login"`
}

type ghComment struct {
	Author    ghAuthor `json:"author"`
	Body      string   `json:"body"`
	CreatedAt string   `json:"createdAt"`
}

// ghPRView is the shared shape of `gh pr view --json` (checkout metadata and
// the reader path use different field subsets of it) and `gh issue view`.
type ghPRView struct {
	Number            int      `json:"number"`
	Title             string   `json:"title"`
	State             string   `json:"state"`
	IsDraft           bool     `json:"isDraft"`
	Author            ghAuthor `json:"author"`
	URL               string   `json:"url"`
	CreatedAt         string   `json:"createdAt"`
	UpdatedAt         string   `json:"updatedAt"`
	Body              string   `json:"body"`
	BaseRefName       string   `json:"baseRefName"`
	HeadRefName       string   `json:"headRefName"`
	HeadRefOid        string   `json:"headRefOid"`
	IsCrossRepository bool     `json:"isCrossRepository"`
	HeadRepository    struct {
		Name string `json:"name"`
	} `json:"headRepository"`
	HeadRepositoryOwner ghAuthor `json:"headRepositoryOwner"`
	Labels              []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Comments []ghComment `json:"comments"`
}

// renderGithubReader formats an issue or PR as reader-mode text.
func renderGithubReader(kind string, v ghPRView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", v.Title)
	meta := []string{fmt.Sprintf("%s #%d", kind, v.Number), strings.ToLower(orDash(v.State))}
	if v.IsDraft {
		meta = append(meta, "draft")
	}
	if v.Author.Login != "" {
		meta = append(meta, "opened by "+v.Author.Login)
	}
	b.WriteString(strings.Join(meta, " · ") + "\n")
	row := func(k, val string) {
		if val != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, val)
		}
	}
	row("url", v.URL)
	row("created", v.CreatedAt)
	row("updated", v.UpdatedAt)
	switch {
	case v.BaseRefName != "" && v.HeadRefName != "":
		row("branches", v.BaseRefName+" <- "+v.HeadRefName)
	case v.HeadRefName != "":
		row("branch", v.HeadRefName)
	}
	row("head", shortSHA(v.HeadRefOid))
	var labels []string
	for _, l := range v.Labels {
		if l.Name != "" {
			labels = append(labels, l.Name)
		}
	}
	if len(labels) > 0 {
		row("labels", strings.Join(labels, ", "))
	}
	if body := strings.TrimSpace(v.Body); body != "" {
		b.WriteString("\n" + capText(body, githubReaderBlockCap) + "\n")
	}
	if n := len(v.Comments); n > 0 {
		fmt.Fprintf(&b, "\n## comments (%d)\n", n)
		shown := n
		if shown > githubReaderCommentCap {
			shown = githubReaderCommentCap
		}
		for _, c := range v.Comments[:shown] {
			fmt.Fprintf(&b, "\n### %s %s\n%s\n", orDash(c.Author.Login), c.CreatedAt,
				capText(strings.TrimSpace(c.Body), githubReaderBlockCap))
		}
		if n > shown {
			fmt.Fprintf(&b, "\n… %d more comments\n", n-shown)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
