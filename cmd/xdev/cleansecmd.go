package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
)

// runCleanse implements `xdev cleanse` (issue #34): redact secrets from a
// session transcript in place, keeping the original as <path>.bak. Redaction
// is the same mechanism the agent applies to provider-visible text
// (config.Redactor), so a cleansed transcript is shareable without inventing a
// second, weaker secret scanner.
func runCleanse(args []string) int {
	cwd := mustGetwd()
	return cleanseCmd(args, cwd, os.Stdout, os.Stderr, func() *config.Redactor {
		return config.OpenRedactor(cwd, func(msg string) { fmt.Fprintln(os.Stderr, "xdev cleanse:", msg) })
	})
}

// cleanseStats is the report (and the --json payload).
type cleanseStats struct {
	Path         string `json:"path"`
	SessionID    string `json:"sessionId,omitempty"`
	Lines        int    `json:"lines"`
	LinesChanged int    `json:"linesChanged"`
	Placeholders int    `json:"placeholders"`
	Backup       string `json:"backup,omitempty"`
	Applied      bool   `json:"applied"`
}

func cleanseCmd(args []string, cwd string, out, errOut io.Writer, newRedactor func() *config.Redactor) int {
	fs := flag.NewFlagSet("cleanse", flag.ContinueOnError)
	fs.SetOutput(errOut)
	sel := fs.String("session", "", "session to cleanse: id prefix or file path (default: newest session in this directory)")
	dryRun := fs.Bool("dry-run", false, "report what would be redacted without writing")
	force := fs.Bool("force", false, "cleanse even a session that looks live (see the guard in compress)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev cleanse [flags]

  xdev cleanse                 redact secrets from this directory's newest session
  xdev cleanse --session 01a0  pick a session by id prefix or path
  xdev cleanse --dry-run       report only, write nothing

The original is kept as <session>.bak; placeholders ($$HASH$$) are the same
reversible markers the agent writes, so a cleansed transcript stays readable
in this install and carries no secret value. A session written in the last two
minutes is refused (it probably belongs to a running xdev): --force overrides.

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
			fmt.Fprintln(errOut, "xdev cleanse:", err)
			return 1
		}
		path = p
	} else if st, err := os.Stat(path); err != nil || st.IsDir() {
		p, rerr := resolveResumeID(cwd, path)
		if rerr != nil {
			fmt.Fprintln(errOut, "xdev cleanse:", rerr)
			return 1
		}
		path = p
	}

	if !*dryRun {
		if err := sessionLiveGuard(path, *force, "cleanse", errOut); err != nil {
			return 1
		}
	}

	red := newRedactor()
	if red == nil {
		fmt.Fprintln(errOut, "xdev cleanse: no secrets configured")
		return 1
	}
	stats, err := cleanseFile(path, red, *dryRun)
	if err != nil {
		fmt.Fprintln(errOut, "xdev cleanse:", err)
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(stats, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	fmt.Fprint(out, cleanseRender(stats))
	return 0
}

// cleanseFile rewrites one transcript line by line. The redaction is applied
// to the raw line rather than to a re-marshalled entry: the session format
// packs the title slot at a fixed width, and re-encoding lines would silently
// reformat the file.
func cleanseFile(path string, red *config.Redactor, dryRun bool) (*cleanseStats, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	stats := &cleanseStats{Path: path, SessionID: cleanseSessionID(path), Lines: len(lines)}
	redacted := make([]string, len(lines))
	for i, line := range lines {
		cleaned := red.Apply(line)
		redacted[i] = cleaned
		if cleaned != line {
			stats.LinesChanged++
			stats.Placeholders += len(cleansePlaceholderRE.FindAllString(cleaned, -1))
		}
	}
	if stats.LinesChanged == 0 || dryRun {
		return stats, nil
	}
	backup := path + ".bak"
	if err := os.WriteFile(backup, data, info.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("write backup: %w", err)
	}
	stats.Backup = backup
	if err := cleanseWriteAtomic(path, strings.Join(redacted, "\n"), info.Mode().Perm()); err != nil {
		return nil, err
	}
	stats.Applied = true
	return stats, nil
}

// cleanseWriteAtomic replaces the transcript without ever leaving it half
// written (a crash mid-write must not cost a session).
func cleanseWriteAtomic(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cleanse-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace: %w", err)
	}
	return nil
}

var cleansePlaceholderRE = regexp.MustCompile(`\$\$[0-9A-F]{8,64}\$\$`)

// cleanseSessionID recovers the session id from the canonical file name.
func cleanseSessionID(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndexByte(base, '_'); i >= 0 {
		return base[i+1:]
	}
	return base
}

func cleanseRender(stats *cleanseStats) string {
	var b strings.Builder
	verb := "redacted"
	if !stats.Applied {
		verb = "would redact"
	}
	fmt.Fprintf(&b, "%s %s: %d of %d lines", verb, compressShort(stats.SessionID), stats.LinesChanged, stats.Lines)
	if stats.Placeholders > 0 {
		fmt.Fprintf(&b, " (%d placeholders)", stats.Placeholders)
	}
	b.WriteString("\n")
	switch {
	case stats.LinesChanged == 0:
		b.WriteString("  nothing matched: no secrets.yml value or secret-shaped env value appears in this transcript\n")
	case stats.Applied:
		fmt.Fprintf(&b, "  backup: %s\n", stats.Backup)
		b.WriteString("  placeholders ($$HASH$$) expand back to the values only in this install\n")
	default:
		b.WriteString("  (dry run — no backup written, transcript untouched)\n")
	}
	return b.String()
}
