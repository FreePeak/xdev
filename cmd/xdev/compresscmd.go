package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// runCompress implements `xdev compress` (issue #34): compact one session file
// in place through the compaction ladder and report the before/after context
// size. Appending a compaction entry is the same thing a live boundary does —
// the raw entries stay in the file, only the reconstructed context shrinks —
// so the operation is recorded and inspectable rather than a rewrite.
func runCompress(args []string) int {
	return compressCmd(args, mustGetwd(), lastSettings(), os.Stdout, os.Stderr)
}

type compressStats struct {
	Path         string `json:"path"`
	SessionID    string `json:"sessionId"`
	Method       string `json:"method"`
	Anchor       string `json:"anchorEntryId"`
	Dropped      int    `json:"droppedMessages"`
	Messages     int    `json:"messages"`
	TokensBefore int64  `json:"tokensBefore"`
	TokensAfter  int64  `json:"tokensAfter"`
	SummaryHead  string `json:"summaryHead"`
	Applied      bool   `json:"applied"`
	EntryID      string `json:"entryId,omitempty"`
}

// defaultCompressMethod picks the ladder member an unattended CLI compaction
// runs: the first product of the configured order that needs no model call.
// The provider summarize (handoff) is deliberately NOT the default — a CLI
// command must not silently spend a provider round-trip; `--method handoff`
// opts in, and the fallback stays deterministic (shake) when the configured
// order names triggers only.
func defaultCompressMethod(settings *config.Settings) string {
	order := agent.HandoffOrder(settings.CompactionMethodOrder())
	for _, m := range order {
		if agent.IsDeterministicMethod(m) {
			return m
		}
	}
	return "shake"
}

func compressCmd(args []string, cwd string, settings *config.Settings, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("compress", flag.ContinueOnError)
	fs.SetOutput(errOut)
	sel := fs.String("session", "", "session to compact: id prefix or file path (default: newest session in this directory)")
	method := fs.String("method", "", "ladder member: shake | soft | snapcompact | handoff (default: the first deterministic member of compaction.methodOrder)")
	keep := fs.Int64("keep-recent", 0, "recent tail to preserve, in tokens (0 = agent default)")
	model := fs.String("model", "", "model for --method handoff (default: the resolved default model)")
	dryRun := fs.Bool("dry-run", false, "report what would be compacted without writing the entry")
	force := fs.Bool("force", false, "compact even a session that looks live (see the guard below)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev compress [flags]

  xdev compress                  compact this directory's newest session
  xdev compress --dry-run        report the plan only, write nothing
  xdev compress --method handoff summarize with the model instead of eliding

A session written in the last two minutes is refused: it probably belongs to a
running xdev (this directory usually has several), and appending to a file a
live store holds an append handle on would fork its history. --force overrides.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := strings.TrimSpace(*sel)
	if path == "" {
		p, err := resolveResumeID(cwd, "")
		if err != nil {
			fmt.Fprintln(errOut, "xdev compress:", err)
			return 1
		}
		path = p
	} else if st, err := os.Stat(path); err != nil || st.IsDir() {
		p, rerr := resolveResumeID(cwd, path)
		if rerr != nil {
			fmt.Fprintln(errOut, "xdev compress:", rerr)
			return 1
		}
		path = p
	}

	// A dry run only reads, so the live-session guard applies to the write.
	if !*dryRun {
		if err := sessionLiveGuard(path, *force, "compress", errOut); err != nil {
			return 1
		}
	}

	name := strings.TrimSpace(*method)
	if name == "" {
		name = defaultCompressMethod(settings)
	}
	name = strings.ToLower(name)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var provider ai.Provider
	provModel := ""
	if name == agent.MethodHandoff {
		p, m, err := compressProvider(*model)
		if err != nil {
			fmt.Fprintln(errOut, "xdev compress:", err)
			return 1
		}
		provider, provModel = p, m
	}

	stats, err := compressSession(ctx, path, name, *keep, *dryRun, provider, provModel)
	if err != nil {
		fmt.Fprintln(errOut, "xdev compress:", err)
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(stats, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	verb := "compacted"
	if !stats.Applied {
		verb = "would compact"
	}
	fmt.Fprintf(out, "%s %s (method %s)\n", verb, compressShort(stats.SessionID), stats.Method)
	fmt.Fprintf(out, "  dropped      %d of %d messages\n", stats.Dropped, stats.Messages)
	fmt.Fprintf(out, "  context      %d -> %d tokens\n", stats.TokensBefore, stats.TokensAfter)
	fmt.Fprintf(out, "  anchor       %s\n", compressShort(stats.Anchor))
	if stats.Applied {
		fmt.Fprintf(out, "  entry        %s\n", compressShort(stats.EntryID))
	} else {
		fmt.Fprintln(out, "  (dry run — pass no --dry-run to write the entry)")
	}
	fmt.Fprintf(out, "\n%s\n", stats.SummaryHead)
	if stats.Applied {
		fmt.Fprintln(out, "\nraw history is retained: the compaction entry only changes what the context rebuild keeps.")
	}
	return 0
}

// compressProvider resolves the model a handoff compaction summarizes with.
func compressProvider(explicit string) (ai.Provider, string, error) {
	cfg, err := config.LoadModelsLayered()
	if err != nil {
		return nil, "", fmt.Errorf("--method handoff: %w", err)
	}
	ref, _, err := resolveModel(explicit, cfg, lastSettings())
	if err != nil {
		return nil, "", fmt.Errorf("--method handoff: %w", err)
	}
	pName, mName, err := config.ParseModelRef(ref)
	if err != nil {
		return nil, "", err
	}
	pc, ok := cfg.Providers[pName]
	if !ok {
		return nil, "", fmt.Errorf("--method handoff: unknown provider %q", pName)
	}
	prov, err := buildProvider(pName, pc, mName, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("--method handoff: %w", err)
	}
	return prov, mName, nil
}

// compressSession is the engine both the CLI and its tests exercise: plan the
// compaction, run the ladder member, and (unless dryRun) append the
// CompactionEntry the live boundary would have written.
func compressSession(ctx context.Context, path, method string, keepRecent int64, dryRun bool, provider ai.Provider, model string) (*compressStats, error) {
	store, err := session.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "xdev compress: session close: %v\n", cerr)
		}
	}()
	ctxRes, err := session.BuildContext(store.Entries(), store.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil, fmt.Errorf("rebuild context: %w", err)
	}
	plan, err := agent.PlanCompaction(ctxRes.Messages, ctxRes.EntryIDs, keepRecent)
	if err != nil {
		return nil, err
	}
	summary, err := plan.Compact(ctx, method, provider, model)
	if err != nil {
		return nil, err
	}
	stats := &compressStats{
		Path:         store.Path(),
		SessionID:    store.ID(),
		Method:       method,
		Anchor:       plan.Anchor(),
		Dropped:      plan.Dropped(),
		Messages:     len(plan.Messages),
		TokensBefore: plan.TokensBefore,
		TokensAfter:  plan.TokensAfter(summary),
		SummaryHead:  summaryHead(summary.Text(), 12),
	}
	if dryRun {
		return stats, nil
	}
	anchor := plan.Anchor()
	entry := &session.CompactionEntry{
		Summary:          summary,
		FirstKeptEntryID: &anchor,
		TokensBefore:     plan.TokensBefore,
		Method:           method,
	}
	if err := store.Append(entry); err != nil {
		return nil, fmt.Errorf("persist compaction: %w", err)
	}
	stats.Applied = true
	stats.EntryID = entry.Env.ID
	return stats, nil
}

// summaryHead renders the first n lines of the retained summary for the
// terminal report (the full text lives in the session entry).
func summaryHead(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("…[%d more lines]", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}

// sessionLiveWindow is how recently a session file must have been written
// before compress/cleanse treat it as owned by a running process.
const sessionLiveWindow = 2 * time.Minute

// sessionLiveGuard refuses to modify a session file another process is likely
// writing. Sessions are per-directory and a working directory usually has
// several xdev processes: appending to a file a live store holds an append
// handle on forks its history, and atomically replacing one (cleanse) loses
// that process's next write to an unlinked inode.
//
// ponytail: mtime is a proxy for liveness — the honest check is a lock the
// store takes while a session is live; --force is the escape hatch until then.
func sessionLiveGuard(path string, force bool, cmd string, errOut io.Writer) error {
	if force {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil // the caller's read/open reports a missing file precisely
	}
	if age := time.Since(info.ModTime()); age < sessionLiveWindow {
		fmt.Fprintf(errOut, "xdev %s: %s was written %s ago — that looks like a live session\n", cmd, path, age.Round(time.Second))
		fmt.Fprintln(errOut, "  pass --force to modify it anyway")
		return fmt.Errorf("session %s looks live", path)
	}
	return nil
}

// compressShort abbreviates a session/entry id for the report (8 chars, the
// same short form --resume accepts).
func compressShort(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
