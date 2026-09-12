package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/stats"
)

// defaultDashboardAddr is the loopback-only dashboard bind (omp-stats keeps
// the same port).
const defaultDashboardAddr = "127.0.0.1:3847"

// dashboardDays and dashboardTools bound the tables; the JSON view is
// unbounded.
const (
	dashboardDays  = 30
	dashboardTools = 20
)

// runStats implements `xdev stats` (M15 #72): a local usage dashboard over
// the session JSONL store.
func runStats(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return statsCmd(ctx, args, os.Stdout, os.Stderr)
}

func statsCmd(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	summary := fs.Bool("summary", false, "print the plain-text table (default)")
	serve := fs.Bool("serve", false, "serve the local dashboard instead of printing")
	addr := fs.String("addr", defaultDashboardAddr, "dashboard bind address (loopback only)")
	interval := fs.Duration("interval", stats.DefaultRefresh, "dashboard rescan interval")
	limit := fs.Int("limit", stats.DefaultMaxFiles, "max session files to scan")
	maxBytes := fs.Int64("max-bytes", stats.DefaultMaxBytes, "max session JSONL bytes to scan")
	since := fs.String("since", "", "only sessions touched in this window (a duration like 168h, or a date like 2026-09-01)")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev stats [flags]

  xdev stats                      usage table over the local session store
  xdev stats --json               same report as JSON
  xdev stats --serve              live dashboard on 127.0.0.1:3847

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(errOut, `
The scan is bounded: the newest --limit sessions up to --max-bytes of JSONL
are read, and a tiny rollup under <agent dir>/stats/rollup.json makes repeat
scans cheap. Cost is what the providers reported, not a bill.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintln(errOut, "xdev stats: unexpected argument", rest[0])
		return 2
	}
	if *asJSON && *summary {
		fmt.Fprintln(errOut, "xdev stats: --json and --summary are mutually exclusive")
		return 2
	}
	opts, err := statsOptions(*since, *limit, *maxBytes)
	if err != nil {
		fmt.Fprintln(errOut, "xdev stats:", err)
		return 2
	}

	if *serve {
		if *asJSON {
			fmt.Fprintln(errOut, "xdev stats: --json has no effect with --serve (use /api/report)")
		}
		fmt.Fprintf(errOut, "xdev stats: dashboard on http://%s (Ctrl-C to stop)\n", *addr)
		if err := stats.Serve(ctx, *addr, opts, *interval); err != nil {
			fmt.Fprintln(errOut, "xdev stats:", err)
			return 1
		}
		return 0
	}

	rep, err := stats.Scan(opts)
	if err != nil {
		fmt.Fprintln(errOut, "xdev stats:", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(errOut, "xdev stats:", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(out, formatStatsReport(rep))
	return 0
}

// statsOptions converts the flags into a scan bound.
func statsOptions(since string, limit int, maxBytes int64) (stats.Options, error) {
	opts := stats.Options{MaxFiles: limit, MaxBytes: maxBytes}
	if since == "" {
		return opts, nil
	}
	if d, err := time.ParseDuration(since); err == nil {
		opts.Since = time.Now().Add(-d)
		return opts, nil
	}
	if t, err := time.Parse("2006-01-02", since); err == nil {
		opts.Since = t
		return opts, nil
	}
	return opts, fmt.Errorf("cannot parse --since %q (want a duration like 168h or a date like 2026-09-01)", since)
}

// formatStatsReport renders the --summary table.
func formatStatsReport(rep *stats.Report) string {
	var b strings.Builder
	t := rep.Totals
	fmt.Fprintf(&b, "xdev stats — %s\n", rep.DataDir)
	fmt.Fprintf(&b, "  scanned %d file(s)", rep.FilesScanned)
	if rep.FilesFiltered > 0 {
		fmt.Fprintf(&b, " · %d filtered by --since", rep.FilesFiltered)
	}
	if rep.FilesOmitted > 0 {
		fmt.Fprintf(&b, " · %d over the cap", rep.FilesOmitted)
	}
	if rep.FilesUnreadable > 0 {
		fmt.Fprintf(&b, " · %d unreadable", rep.FilesUnreadable)
	}
	fmt.Fprintf(&b, " · %d from the rollup\n", rep.CacheHits)
	if rep.Truncated {
		fmt.Fprintf(&b, "  note: scan hit the cap (%d files / %d MiB of JSONL) — raise --limit/--max-bytes for full history\n",
			rep.MaxFiles, rep.MaxBytes>>20)
	}
	if !t.FirstSession.IsZero() {
		fmt.Fprintf(&b, "  window %s → %s\n", t.FirstSession.UTC().Format("2006-01-02"), t.LastSession.UTC().Format("2006-01-02"))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  %-14s %d (%d subagent)\n", "sessions", t.Sessions, t.Subagents)
	fmt.Fprintf(&b, "  %-14s %d\n", "turns", t.Turns)
	fmt.Fprintf(&b, "  %-14s %d\n", "user messages", t.UserMessages)
	fmt.Fprintf(&b, "  %-14s %d (%d errors)\n", "tool calls", t.ToolCalls, t.ToolErrors)
	fmt.Fprintf(&b, "  %-14s in %s · out %s · cache read %s · cache write %s · total %s\n", "tokens",
		stats.HumanTokens(t.Input), stats.HumanTokens(t.Output), stats.HumanTokens(t.CacheRead),
		stats.HumanTokens(t.CacheWrite), stats.HumanTokens(t.TotalTokens))
	fmt.Fprintf(&b, "  %-14s %s reported over %d priced turns\n", "cost", stats.HumanMoney(t.CostUSD), t.PricedTurns)
	if t.PricedTurns < t.Turns {
		fmt.Fprintf(&b, "  %-14s %d of %d turns carried no provider price (counted as 0)\n", "", t.Turns-t.PricedTurns, t.Turns)
	}
	d := rep.Distribution
	fmt.Fprintf(&b, "  %-14s turns p50 %d · p90 %d · max %d\n", "per session", d.TurnsP50, d.TurnsP90, d.TurnsMax)
	fmt.Fprintf(&b, "  %-14s tokens p50 %s · p90 %s · max %s\n", "", stats.HumanTokens(d.TokensP50),
		stats.HumanTokens(d.TokensP90), stats.HumanTokens(d.TokensMax))

	if len(rep.Models) > 0 {
		fmt.Fprintf(&b, "\n  by model\n  %-28s %8s %8s %10s %10s\n", "MODEL", "SESSIONS", "TURNS", "TOKENS", "COST")
		for _, m := range rep.Models {
			fmt.Fprintf(&b, "  %-28s %8d %8d %10s %10s\n", truncate(m.Model, 28), m.Sessions, m.Turns,
				stats.HumanTokens(m.TotalTokens), stats.HumanMoney(m.CostUSD))
		}
	}
	if len(rep.Tools) > 0 {
		fmt.Fprintf(&b, "\n  top tools\n  %-28s %8s %8s\n", "TOOL", "CALLS", "ERRORS")
		for i, tool := range rep.Tools {
			if i >= dashboardTools {
				fmt.Fprintf(&b, "  ... %d more\n", len(rep.Tools)-dashboardTools)
				break
			}
			fmt.Fprintf(&b, "  %-28s %8d %8d\n", truncate(tool.Name, 28), tool.Calls, tool.Errors)
		}
	}
	if len(rep.Days) > 0 {
		days := rep.Days
		if len(days) > dashboardDays {
			fmt.Fprintf(&b, "\n  per day (UTC, last %d of %d)\n", dashboardDays, len(days))
			days = days[len(days)-dashboardDays:]
		} else {
			b.WriteString("\n  per day (UTC)\n")
		}
		fmt.Fprintf(&b, "  %-12s %8s %8s %10s %10s\n", "DAY", "SESSIONS", "TURNS", "TOKENS", "COST")
		for _, day := range days {
			fmt.Fprintf(&b, "  %-12s %8d %8d %10s %10s\n", day.Day, day.Sessions, day.Turns,
				stats.HumanTokens(day.Tokens), stats.HumanMoney(day.CostUSD))
		}
	}
	return b.String()
}

// truncate shortens a table cell without splitting a rune.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
