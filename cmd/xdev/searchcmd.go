package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// runSearch implements `xdev search <query>` (issue #34): full-text search over
// the local session store. It reads the same reconstructed message stream a
// resumed run would see (session.BuildContext), so a hit means "this session's
// conversation contains it", not "some JSONL byte matched".
func runSearch(args []string) int {
	return searchCmd(args, config.DataDir(), mustGetwd(), os.Stdout, os.Stderr)
}

// searchHit is one matching turn.
type searchHit struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Path      string `json:"path"`
	Role      string `json:"role"`
	Seq       int    `json:"seq"`
	Snippet   string `json:"snippet"`
}

// searchResult groups the hits of one session.
type searchResult struct {
	SessionID string      `json:"sessionId"`
	Title     string      `json:"title"`
	Path      string      `json:"path"`
	Matches   []searchHit `json:"matches"`
}

// searchQuery is the resolved request (the CLI flags, validated).
type searchQuery struct {
	Text     string
	Re       *regexp.Regexp
	Limit    int
	MaxHits  int
	Here     bool
	Subagent bool
	cwd      string
	// Scanned bounds the whole-store walk: search reads every session it
	// inspects, so a huge store must still answer in a bounded time.
	Scanned int
}

// parseFlagRounds parses args in rounds, peeling one positional at a time.
// Go's flag package stops at the first positional, so a plain fs.Parse would
// turn the natural `xdev search foo --limit 2` into a usage error; this keeps
// the flag vocabulary order-independent.
func parseFlagRounds(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
		if len(args) == 0 {
			return positional, nil
		}
	}
}

func searchCmd(args []string, dataDir, cwd string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(errOut)
	limit := fs.Int("limit", 10, "max sessions to print")
	maxHits := fs.Int("max-hits", 3, "max matching turns per session")
	maxSessions := fs.Int("max-sessions", 500, "max session files to scan (newest first)")
	useRegex := fs.Bool("regex", false, "treat the query as a Go regexp")
	here := fs.Bool("here", false, "only this directory's sessions")
	subs := fs.Bool("subagents", false, "include subagent child sessions")
	asJSON := fs.Bool("json", false, "print the matches as JSON")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev search [flags] <query>

  xdev search deploy           sessions whose messages mention "deploy"
  xdev search --regex 'v\d+\.\d+'   regexp form
  xdev search --here timeout   only this directory's sessions

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlagRounds(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fs.Usage()
		return 2
	}
	q := searchQuery{
		Text:     positional[0],
		Limit:    *limit,
		MaxHits:  *maxHits,
		Here:     *here,
		Subagent: *subs,
		cwd:      cwd,
		Scanned:  *maxSessions,
	}
	if strings.TrimSpace(q.Text) == "" {
		fmt.Fprintln(errOut, "xdev search: empty query")
		return 2
	}
	if *useRegex {
		re, err := regexp.Compile(q.Text)
		if err != nil {
			fmt.Fprintln(errOut, "xdev search:", err)
			return 2
		}
		q.Re = re
	}

	results, scanned, total, err := searchStore(dataDir, q)
	if err != nil {
		fmt.Fprintln(errOut, "xdev search:", err)
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(results, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	fmt.Fprint(out, searchRender(q.Text, results, scanned, total))
	return 0
}

// searchStore walks the store newest-first and returns the sessions with hits.
func searchStore(dataDir string, q searchQuery) ([]searchResult, int, int, error) {
	metas, err := session.List(dataDir)
	if err != nil {
		return nil, 0, 0, err
	}
	total := len(metas)
	var results []searchResult
	scanned := 0
	for _, m := range metas {
		if scanned >= q.Scanned || len(results) >= q.Limit {
			break
		}
		if q.Here && m.CWD != q.cwd {
			continue
		}
		if !q.Subagent && m.TitleSource == session.TitleSourceSubagent {
			continue
		}
		scanned++
		hits, err := searchSession(m, q)
		if err != nil {
			// A session that no longer parses must not abort the search:
			// the store holds history written by older versions.
			continue
		}
		if len(hits) == 0 {
			continue
		}
		results = append(results, searchResult{SessionID: m.ID, Title: m.Title, Path: m.Path, Matches: hits})
	}
	return results, scanned, total, nil
}

// searchSession matches one session's reconstructed messages. A message the
// store synthesized (a compaction summary) has no entry id and still counts:
// the summary is part of what the conversation now contains.
func searchSession(meta session.SessionMeta, q searchQuery) ([]searchHit, error) {
	store, err := session.Open(meta.Path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	ctxRes, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil, err
	}
	var hits []searchHit
	for i, msg := range ctxRes.Messages {
		text := msg.Text()
		if text == "" {
			continue
		}
		if _, ok := searchMatch(q, text); !ok {
			continue
		}
		hits = append(hits, searchHit{
			SessionID: meta.ID,
			Title:     meta.Title,
			Path:      meta.Path,
			Role:      string(msg.Role),
			Seq:       i,
			Snippet:   searchSnippet(q, text),
		})
		if len(hits) >= q.MaxHits {
			break
		}
	}
	return hits, nil
}

// searchMatch reports whether text matches, case-insensitively for the
// substring form.
func searchMatch(q searchQuery, text string) (int, bool) {
	if q.Re != nil {
		loc := q.Re.FindStringIndex(text)
		if loc == nil {
			return 0, false
		}
		return loc[0], true
	}
	idx := strings.Index(strings.ToLower(text), strings.ToLower(q.Text))
	return idx, idx >= 0
}

// searchSnippet centers a one-line window on the first match.
func searchSnippet(q searchQuery, text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	idx, ok := searchMatch(q, flat)
	if !ok {
		return truncate(flat, 120)
	}
	const window = 60
	start := idx - window
	if start < 0 {
		start = 0
	}
	end := idx + window
	if end > len(flat) {
		end = len(flat)
	}
	out := flat[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(flat) {
		out += "…"
	}
	return out
}

func searchRender(query string, results []searchResult, scanned, total int) string {
	var b strings.Builder
	matches := 0
	for _, r := range results {
		matches += len(r.Matches)
	}
	fmt.Fprintf(&b, "SEARCH %q — %d sessions, %d matches (scanned %d of %d)\n", query, len(results), matches, scanned, total)
	if len(results) == 0 {
		b.WriteString("\nno session matched.\n")
		return b.String()
	}
	for _, r := range results {
		fmt.Fprintf(&b, "\n%s  %s\n  %s\n", compressShort(r.SessionID), r.Title, r.Path)
		for _, h := range r.Matches {
			fmt.Fprintf(&b, "  %-10s %s\n", h.Role, h.Snippet)
		}
	}
	return b.String()
}
