package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/FreePeak/xdev/internal/tool"
)

// runWorktree implements `xdev worktree` (alias `xdev wt`): manage git
// worktrees from any xdev working directory. Every operation goes through
// tool.GitCLI — the same plumbing the github tool's pr_checkout uses — so the
// CLI and the agent tool agree on environment, timeouts and error shape.
func runWorktree(args []string) int {
	return worktreeCmd(args, mustGetwd(), os.Stdout, os.Stderr)
}

// worktreeUsage is the `usage:` block printed for a bare `xdev worktree`, an
// unknown subcommand, or a missing required path. It is a single const so
// there is exactly one text to keep honest.
const worktreeUsage = `usage: xdev worktree <subcommand> [flags] [args]

  xdev worktree list [--json]            registered worktrees (default subcommand)
  xdev worktree add <path> [-b branch] [ref]
                                         create a worktree (ref defaults to HEAD)
  xdev worktree remove <path> [--force]  delete a worktree
  xdev worktree prune [--dry-run]        drop stale worktree registrations

Every git call runs from the current directory, which must be inside a
repository. Alias: xdev wt.
`

// worktreeCmd dispatches the worktree subcommands. args[0] is the subcommand
// name; it defaults to list because `xdev worktree` with no arguments should
// show the user where their checkouts live rather than dump usage.
func worktreeCmd(args []string, cwd string, out, errOut io.Writer) int {
	sub, rest := "list", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "list":
		return worktreeList(rest, cwd, out, errOut)
	case "add":
		return worktreeAdd(rest, cwd, out, errOut)
	case "remove":
		return worktreeRemove(rest, cwd, out, errOut)
	case "prune":
		return worktreePrune(rest, cwd, out, errOut)
	case "help", "-h", "--help":
		fmt.Fprint(errOut, worktreeUsage)
		return 0
	}
	fmt.Fprintf(errOut, "xdev worktree: unknown subcommand %q\n\n%s", sub, worktreeUsage)
	return 2
}

// worktreeRow is one parsed `git worktree list --porcelain` record. Git
// separates records with a blank line and every attribute is a `key value`
// line: `locked` and `prunable` carry an optional reason, `bare` and
// `detached` stand alone.
type worktreeRow struct {
	Path     string `json:"path"`
	Head     string `json:"head"`
	Branch   string `json:"branch"`
	Detached bool   `json:"detached"`
	Bare     bool   `json:"bare"`
	Locked   bool   `json:"locked"`
	Prunable bool   `json:"prunable"`
}

// parseWorktreePorcelain splits the porcelain stream into records, tolerating
// a trailing blank line (git always emits one) and `\r\n` line endings. A
// record with no `worktree <path>` line carries nothing usable, so it is
// dropped rather than surfacing as a row with an empty path.
func parseWorktreePorcelain(s string) []worktreeRow {
	var rows []worktreeRow
	var cur *worktreeRow
	flush := func() {
		if cur != nil && cur.Path != "" {
			rows = append(rows, *cur)
		}
		cur = nil
	}
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			flush()
			continue
		}
		if cur == nil {
			cur = &worktreeRow{}
		}
		// A locked/prunable reason can contain spaces, so only the FIRST
		// space separates key from value — never split further.
		key, val := ln, ""
		if i := strings.IndexByte(ln, ' '); i >= 0 {
			key, val = ln[:i], ln[i+1:]
		}
		switch key {
		case "worktree":
			cur.Path = val
		case "HEAD":
			cur.Head = val
		case "branch":
			// Refs arrive as refs/heads/<name>; only the name belongs in a
			// table, and an empty value (detached) keeps the field empty so
			// the detached flag is the only signal.
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "detached":
			cur.Detached = true
		case "bare":
			cur.Bare = true
		case "locked":
			cur.Locked = true
		case "prunable":
			cur.Prunable = true
		}
	}
	flush()
	return rows
}

// worktreeBranchText is the BRANCH cell: the branch name, `(detached)` for a
// detached HEAD, and `-` for a bare repo (porcelain prints no branch line).
func worktreeBranchText(r worktreeRow) string {
	switch {
	case r.Detached:
		return "(detached)"
	case r.Branch != "":
		return r.Branch
	default:
		return "-"
	}
}

// worktreeShortHead shortens the HEAD sha the way git prints it everywhere.
func worktreeShortHead(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// worktreeMarkerText is the FLAGS cell: bare/locked/prunable are footguns, so
// they are named instead of shown as booleans.
func worktreeMarkerText(r worktreeRow) string {
	var marks []string
	if r.Bare {
		marks = append(marks, "(bare)")
	}
	if r.Locked {
		marks = append(marks, "(locked)")
	}
	if r.Prunable {
		marks = append(marks, "(prunable)")
	}
	return strings.Join(marks, " ")
}

// worktreeList prints the registered worktrees. Column widths come from the
// rows themselves so no path is ever truncated (a truncated path cannot be
// pasted back into `xdev worktree remove`).
func worktreeList(args []string, cwd string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("worktree list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "print the worktree list as a JSON array")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev worktree list [--json]

Lists every registered worktree with its branch name and short HEAD.
--json prints the same records as an array of objects.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(errOut, "xdev worktree list: unexpected argument %q\n\n", rest[0])
		fs.Usage()
		return 2
	}
	res, err := tool.GitCLI(context.Background(), cwd, "worktree", "list", "--porcelain")
	if err != nil {
		fmt.Fprintln(errOut, "xdev worktree list:", err)
		return 1
	}
	rows := parseWorktreePorcelain(res)
	if *asJSON {
		if rows == nil {
			// An empty repository must encode as [] — `null` breaks every
			// jq/script consumer.
			rows = []worktreeRow{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintln(errOut, "xdev worktree list:", err)
			return 1
		}
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no worktrees")
		return 0
	}
	// ponytail: widths are byte counts, so a path in a double-width script
	// (CJK) shifts the column. Upgrade path: pad by display width with a
	// small rune-width table — not worth a dependency for a path table.
	pcw, bcw, hcw := len("PATH"), len("BRANCH"), len("HEAD")
	for _, r := range rows {
		pcw = max(pcw, len(r.Path))
		bcw = max(bcw, len(worktreeBranchText(r)))
		hcw = max(hcw, len(r.Head))
	}
	fmt.Fprintf(out, "%-*s  %-*s  %-*s  %s\n", pcw, "PATH", bcw, "BRANCH", hcw, "HEAD", "FLAGS")
	for _, r := range rows {
		fmt.Fprintf(out, "%-*s  %-*s  %-*s  %s\n", pcw, r.Path, bcw, worktreeBranchText(r),
			hcw, worktreeShortHead(r.Head), worktreeMarkerText(r))
	}
	return 0
}

// worktreeAdd creates a worktree. `git worktree add <path>` checks out the
// current HEAD; `-b <branch>` creates the branch on the way, and <ref> pins
// the checkout. Git's own output is echoed verbatim (it says which branch was
// created) and the last line names the path for scripts to consume.
func worktreeAdd(args []string, cwd string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("worktree add", flag.ContinueOnError)
	fs.SetOutput(errOut)
	newBranch := fs.String("b", "", "create <branch> in the new worktree")
	force := fs.Bool("force", false, "add even if <path> is registered or the branch is checked out elsewhere")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev worktree add <path> [-b branch] [ref]

Registers a new worktree at <path>, checking out <ref> (default HEAD).
A relative <path> resolves against the directory the command runs from.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 || rest[0] == "" {
		fmt.Fprintln(errOut, "xdev worktree add: missing required path")
		fs.Usage()
		return 2
	}
	if len(rest) > 2 {
		fmt.Fprintf(errOut, "xdev worktree add: unexpected argument %q (want <path> [ref])\n\n", rest[2])
		fs.Usage()
		return 2
	}
	path := worktreeAbsPath(cwd, rest[0])
	gitArgs := []string{"worktree", "add"}
	if *force {
		gitArgs = append(gitArgs, "--force")
	}
	if *newBranch != "" {
		gitArgs = append(gitArgs, "-b", *newBranch)
	}
	gitArgs = append(gitArgs, path)
	if len(rest) == 2 {
		gitArgs = append(gitArgs, rest[1])
	}
	res, err := tool.GitCLI(context.Background(), cwd, gitArgs...)
	if err != nil {
		fmt.Fprintln(errOut, "xdev worktree add:", err)
		return 1
	}
	fmt.Fprint(out, res)
	fmt.Fprintf(out, "worktree created at %s\n", path)
	return 0
}

// worktreeRemove deletes a worktree. `--force` covers the two cases git
// refuses by default: a dirty tree and a locked worktree.
func worktreeRemove(args []string, cwd string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("worktree remove", flag.ContinueOnError)
	fs.SetOutput(errOut)
	force := fs.Bool("force", false, "remove even if the worktree is dirty or locked")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev worktree remove <path> [--force]

Unregisters and deletes the worktree at <path>.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 || rest[0] == "" {
		fmt.Fprintln(errOut, "xdev worktree remove: missing required path")
		fs.Usage()
		return 2
	}
	if len(rest) > 1 {
		fmt.Fprintf(errOut, "xdev worktree remove: unexpected argument %q\n\n", rest[1])
		fs.Usage()
		return 2
	}
	path := worktreeAbsPath(cwd, rest[0])
	gitArgs := []string{"worktree", "remove"}
	if *force {
		gitArgs = append(gitArgs, "--force")
	}
	gitArgs = append(gitArgs, path)
	res, err := tool.GitCLI(context.Background(), cwd, gitArgs...)
	if err != nil {
		fmt.Fprintln(errOut, "xdev worktree remove:", err)
		return 1
	}
	fmt.Fprint(out, res)
	fmt.Fprintf(out, "removed %s\n", path)
	return 0
}

// worktreePrune drops registrations whose directories have vanished. Without
// --dry-run it actually prunes; git prints nothing either way, so silence is
// the success signal here.
func worktreePrune(args []string, cwd string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("worktree prune", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dryRun := fs.Bool("dry-run", false, "report stale worktrees without pruning")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev worktree prune [--dry-run]

Removes registrations of worktree directories that no longer exist.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(errOut, "xdev worktree prune: unexpected argument %q\n\n", rest[0])
		fs.Usage()
		return 2
	}
	gitArgs := []string{"worktree", "prune"}
	if *dryRun {
		gitArgs = append(gitArgs, "--dry-run")
	}
	res, err := tool.GitCLI(context.Background(), cwd, gitArgs...)
	if err != nil {
		fmt.Fprintln(errOut, "xdev worktree prune:", err)
		return 1
	}
	fmt.Fprint(out, res)
	return 0
}

// worktreeAbsPath resolves an operand against the directory the command runs
// from rather than the process working directory: the seam contract is "run
// from cwd", so a relative path must land beside it even when the caller (a
// test, or a future daemon) has a different process cwd.
func worktreeAbsPath(cwd, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cwd, p)
}
