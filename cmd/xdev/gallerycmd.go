// `xdev gallery` (alias `render`) lists the local session store and turns one
// transcript into the self-contained HTML that /export writes (issue #34).
// The renderer itself is internal/share: this file only resolves the selector,
// pins the output path and prints the table, so the transcript markup keeps
// exactly one implementation.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
	"github.com/FreePeak/xdev/internal/share"
)

// galleryLimitDefault bounds the listing: the store can hold thousands of
// sessions and a terminal is not a pager (--limit raises it).
const galleryLimitDefault = 20

// Column widths for the listing; the title/cwd cells truncate to fit.
const (
	galleryTitleWidth = 32
	galleryCWDWidth   = 36
)

// galleryTimeLayout is the table's MODIFIED cell (local time, minute
// resolution — the second is noise in a gallery).
const galleryTimeLayout = "2006-01-02 15:04"

// galleryRow is one listing row: the table cell values plus the fields a
// --json consumer (a wrapper script, the future picker) needs.
type galleryRow struct {
	ShortID     string    `json:"shortId"`
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	CWD         string    `json:"cwd"`
	Path        string    `json:"path"`
	SizeBytes   int64     `json:"sizeBytes"`
	ModTime     time.Time `json:"modified"`
	TitleSource string    `json:"titleSource"`
	Subagent    bool      `json:"subagent"`
	Parent      string    `json:"parentSession,omitempty"`
}

// runGallery is the main() entry for both `xdev gallery` and its `render`
// alias. main() dispatches both names here with the same args (see the
// subcommands table in main.go), so the alias is recognized from os.Args[1]:
// `render` is exactly `gallery --html`. Passing --html explicitly stays a
// no-op (repeating a bool flag is legal), so a main.go that also prepends it
// keeps working.
func runGallery(args []string) int {
	mode := "gallery"
	if len(os.Args) > 1 && os.Args[1] == "render" {
		mode = "render"
	}
	return galleryCmdAs(mode, args, config.DataDir(), mustGetwd(), os.Stdout, os.Stderr)
}

// galleryCmd is the first-arg dispatch target for `xdev gallery`; the mode
// split lives in galleryCmdAs so tests (and `render`) drive one code path.
func galleryCmd(args []string, dataDir, cwd string, out, errOut io.Writer) int {
	return galleryCmdAs("gallery", args, dataDir, cwd, out, errOut)
}

// galleryCmdAs implements the listing, the metadata view and the HTML render.
// mode is "gallery" or "render": it only selects the default for --html and
// the name in usage/error lines, so the two commands can never drift apart.
func galleryCmdAs(mode string, args []string, dataDir, cwd string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(errOut)
	asHTML := fs.Bool("html", mode == "render", "render the selected transcript to a self-contained HTML file")
	outPath := fs.String("out", "", "HTML output path (default <cwd>/<shortid>.html)")
	limit := fs.Int("limit", galleryLimitDefault, "max sessions to list, newest first")
	asJSON := fs.Bool("json", false, "print the session list as JSON")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev gallery [selector] [flags]
       xdev render <selector> [flags]

  xdev gallery                      list the newest sessions in the local store
  xdev gallery <id|path>            print one session's metadata
  xdev gallery <id|path> --html     render that transcript to HTML
  xdev render <id|path>             same as gallery --html --out ...

A selector is a session id prefix (case-insensitive, newest match wins) or the
path of a session .jsonl file. Rendering reuses the /export renderer: one
self-contained HTML file, nothing to fetch.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(errOut, `
--limit/--json apply to the listing only (no selector).
`)
	}
	// Go's flag package stops at the first positional, so a selector written
	// before its flags (`gallery <id> --html`) would leave those flags
	// unparsed. Parse in rounds: each round peels one leading selector off
	// fs.Args() and re-parses the rest, so flag/selector order never matters.
	selector := ""
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		if selector != "" {
			fmt.Fprintln(errOut, "xdev "+mode+": unexpected argument", rest[0])
			return 2
		}
		selector = rest[0]
		rest = rest[1:]
	}
	// Validate the flag combinations before touching the store: every failure
	// below is a usage error (2), not a runtime one (1).
	if *asJSON && *asHTML {
		fmt.Fprintln(errOut, "xdev "+mode+": --json and --html are mutually exclusive")
		return 2
	}
	if *outPath != "" && !*asHTML {
		fmt.Fprintln(errOut, "xdev "+mode+": --out needs --html (or use `xdev render`)")
		return 2
	}
	if *asJSON && selector != "" {
		fmt.Fprintln(errOut, "xdev "+mode+": --json lists sessions; drop the selector")
		return 2
	}
	if *limit <= 0 {
		fmt.Fprintln(errOut, "xdev "+mode+": --limit must be positive")
		return 2
	}
	if selector == "" {
		if *asHTML {
			// `render` without a selector has nothing to export.
			fs.Usage()
			return 2
		}
		return galleryList(dataDir, *limit, *asJSON, out, errOut, mode)
	}

	meta, err := galleryResolve(dataDir, selector)
	if err != nil {
		fmt.Fprintln(errOut, "xdev "+mode+":", err)
		return 2
	}
	if !*asHTML {
		fmt.Fprint(out, galleryMetaBlock(meta))
		return 0
	}

	target := *outPath
	if target == "" {
		target = filepath.Join(cwd, galleryShortID(meta.ID)+".html")
	}
	store, err := session.Open(meta.Path)
	if err != nil {
		fmt.Fprintln(errOut, "xdev "+mode+":", err)
		return 1
	}
	// An open store only releases a read handle here: no appended entry, so
	// the Close error is a lost flush at worst and worth no code path.
	defer store.Close()
	written, err := share.Export(store, share.Options{}, target)
	if err != nil {
		fmt.Fprintln(errOut, "xdev "+mode+":", err)
		return 1
	}
	fmt.Fprintln(out, "Rendered: "+written)
	return 0
}

// galleryList prints the newest sessions (table or JSON). An empty store gets
// a one-line explanation instead of a header with no rows — "no sessions" is
// a fact about the store, and an empty table hides it.
func galleryList(dataDir string, limit int, asJSON bool, out, errOut io.Writer, mode string) int {
	metas, err := session.List(dataDir)
	if err != nil {
		fmt.Fprintln(errOut, "xdev "+mode+":", err)
		return 1
	}
	rows := galleryRows(metas)
	if asJSON {
		if limit < len(rows) {
			rows = rows[:limit]
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintln(errOut, "xdev "+mode+":", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(out, galleryTable(dataDir, rows, limit))
	return 0
}

// galleryRows maps store metadata onto listing rows (one shape for the table,
// the JSON view and the selector echo).
func galleryRows(metas []session.SessionMeta) []galleryRow {
	rows := make([]galleryRow, 0, len(metas))
	for _, m := range metas {
		rows = append(rows, galleryRow{
			ShortID:     galleryShortID(m.ID),
			ID:          m.ID,
			Title:       m.Title,
			CWD:         m.CWD,
			Path:        m.Path,
			SizeBytes:   m.SizeBytes,
			ModTime:     m.ModTime,
			TitleSource: m.TitleSource,
			Subagent:    m.TitleSource == session.TitleSourceSubagent,
			Parent:      m.ParentSession,
		})
	}
	return rows
}

// galleryTable renders the listing: the store root first (so a surprising
// listing is obviously the wrong data dir), then the fixed-width columns.
func galleryTable(dataDir string, rows []galleryRow, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "xdev gallery — %s\n", dataDir)
	if len(rows) == 0 {
		fmt.Fprintf(&b, "no sessions under %s (run xdev in a directory to create one)\n", session.SessionsRoot(dataDir))
		return b.String()
	}
	shown := rows
	if limit < len(shown) {
		shown = shown[:limit]
	}
	fmt.Fprintf(&b, "  %-8s %-*s %-*s %9s  %-16s\n",
		"ID", galleryTitleWidth, "TITLE", galleryCWDWidth, "CWD", "SIZE", "MODIFIED")
	for _, r := range shown {
		marker := ""
		if r.Subagent {
			marker = "  [subagent]"
		}
		fmt.Fprintf(&b, "  %-8s %-*s %-*s %9s  %-16s%s\n",
			r.ShortID,
			galleryTitleWidth, truncate(galleryOrUntitled(r.Title), galleryTitleWidth),
			galleryCWDWidth, truncate(r.CWD, galleryCWDWidth),
			galleryBytes(r.SizeBytes),
			r.ModTime.Local().Format(galleryTimeLayout),
			marker)
	}
	if len(rows) > len(shown) {
		fmt.Fprintf(&b, "  ... %d more (raise --limit)\n", len(rows)-len(shown))
	}
	return b.String()
}

// galleryMetaBlock is the non-HTML view of one session: the identity fields,
// the full .jsonl path (so the file can be opened by hand) and how to render
// it. The hint names `gallery --html` because `render` already implies it.
func galleryMetaBlock(m session.SessionMeta) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s\n", galleryShortID(m.ID))
	fmt.Fprintf(&b, "  title    %s\n", galleryOrUntitled(m.Title))
	fmt.Fprintf(&b, "  cwd      %s\n", m.CWD)
	fmt.Fprintf(&b, "  id       %s\n", m.ID)
	fmt.Fprintf(&b, "  path     %s\n", m.Path)
	fmt.Fprintf(&b, "  size     %s\n", galleryBytes(m.SizeBytes))
	fmt.Fprintf(&b, "  modified %s\n", m.ModTime.Local().Format(galleryTimeLayout))
	if m.TitleSource == session.TitleSourceSubagent {
		fmt.Fprintf(&b, "  kind     subagent")
		if m.ParentSession != "" {
			fmt.Fprintf(&b, " (parent %s)", galleryShortID(m.ParentSession))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nrender it with: xdev gallery %s --html [--out FILE]\n", galleryShortID(m.ID))
	return b.String()
}

// galleryResolve turns a selector into store metadata: an existing file path
// wins (that is how an exported/foreign .jsonl is addressed), otherwise a
// case-insensitive id-prefix match, newest first — the same rule --resume
// uses, so a selector copied from the table always resolves.
func galleryResolve(dataDir, selector string) (session.SessionMeta, error) {
	if st, err := os.Stat(selector); err == nil && !st.IsDir() {
		abs, aerr := filepath.Abs(selector)
		if aerr != nil {
			abs = selector
		}
		metas, lerr := session.List(dataDir)
		if lerr == nil {
			for _, m := range metas {
				if m.Path == abs {
					return m, nil
				}
			}
		}
		// A file the scan does not know (a copy outside the store root):
		// read its header for the identity fields, stat for size/mtime.
		store, oerr := session.Open(abs)
		if oerr != nil {
			return session.SessionMeta{}, oerr
		}
		defer store.Close()
		id := store.ID()
		if id == "" {
			// An id-less header: the file name is the only identity left.
			id = strings.TrimSuffix(filepath.Base(abs), ".jsonl")
		}
		return session.SessionMeta{
			Path: abs, ID: id, Title: store.Title(), CWD: store.CWD(),
			SizeBytes: st.Size(), ModTime: st.ModTime(),
		}, nil
	}
	metas, err := session.List(dataDir)
	if err != nil {
		return session.SessionMeta{}, err
	}
	q := strings.ToLower(selector)
	for _, m := range metas {
		if m.ID != "" && strings.HasPrefix(strings.ToLower(m.ID), q) {
			return m, nil
		}
	}
	return session.SessionMeta{}, fmt.Errorf("no session matching %q (id prefix or a .jsonl path under %s)",
		selector, session.SessionsRoot(dataDir))
}

// galleryShortID is the 8-char form the listing, pins and /resume share.
func galleryShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// galleryOrUntitled keeps an empty title from looking like a formatting bug.
func galleryOrUntitled(title string) string {
	if strings.TrimSpace(title) == "" {
		return "(untitled)"
	}
	return title
}

// galleryBytes formats a byte count for the SIZE cell. stats.HumanTokens
// counts tokens (1000-based, k/M suffixes) — a file size reads better
// binary-scaled, so it gets its own four-line formatter.
func galleryBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1024))
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1024*1024*1024))
	}
}
