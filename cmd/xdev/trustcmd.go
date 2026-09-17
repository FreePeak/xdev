package main

// `xdev trust` / `xdev distrust` (#241): the review, and the decision, for
// hooks that arrived inside a repository. Cloning a project is a code-delivery
// event — `.xdev/hooks/agent_start.sh` fires on every session start with the
// user's environment — so xdev withholds those hooks until this command records
// a human reading of them, and re-withholds them the moment their bytes change.

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	hookbus "github.com/FreePeak/xdev/internal/hooks"
)

// runTrust dispatches both verbs through one implementation so path resolution
// and reporting cannot drift apart between them.
func runTrust(verb string, args []string) int {
	return trustCmd(verb, args, mustGetwd(), os.Stdin, os.Stdout, os.Stderr)
}

func trustCmd(verb string, args []string, cwd string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	list := fs.Bool("list", false, "show recorded decisions and what this directory is waiting on")
	yes := fs.Bool("yes", false, "record the decision without asking (for scripts; prints what it approved)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	target := cwd
	if rest := fs.Args(); len(rest) > 0 {
		target = rest[0]
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		fmt.Fprintln(stderr, "xdev", verb+":", err)
		return 2
	}
	switch verb {
	case "trust":
		return trustWorkspace(abs, *list, *yes, stdin, stdout, stderr)
	case "distrust":
		return distrustWorkspace(abs, stdout, stderr)
	}
	fmt.Fprintln(stderr, "xdev: unknown verb", verb)
	return 2
}

// trustWorkspace reviews, then records. --list reports and changes nothing,
// which is how a user finds out what a clone is asking for before deciding.
func trustWorkspace(root string, list, yes bool, stdin io.Reader, stdout, stderr io.Writer) int {
	pending, err := hookbus.PendingProjectHooks(root)
	if err != nil {
		fmt.Fprintln(stderr, "xdev trust:", err)
		return 1
	}
	recorded, err := hookbus.TrustedList()
	if err != nil {
		fmt.Fprintln(stderr, "xdev trust:", err)
		return 1
	}
	if list {
		fmt.Fprintf(stdout, "trust file: %s\n", hookbus.TrustPath())
		if len(recorded) == 0 {
			fmt.Fprintln(stdout, "no workspace is trusted")
		} else {
			fmt.Fprintf(stdout, "trusted workspaces (%d):\n", len(recorded))
			for _, rec := range recorded {
				fmt.Fprintf(stdout, "  %s  (%d hook(s): %s; decided %s)\n",
					rec.Root, len(rec.Hooks), strings.Join(rec.Hooks, ", "),
					time.Unix(rec.DecidedAt, 0).Format("2006-01-02 15:04"))
			}
		}
		if len(pending) == 0 {
			fmt.Fprintf(stdout, "%s: nothing pending\n", root)
			return 0
		}
		fmt.Fprintf(stdout, "%s: %d repository hook(s) waiting:\n", root, len(pending))
		reportHooks(stdout, pending)
		return 0
	}

	if len(pending) == 0 {
		fmt.Fprintf(stdout, "%s: no repository hooks to trust\n", root)
		return 0
	}
	// Say what is about to be allowed before allowing it, in every mode: an
	// approved-but-never-read hook list is the same failure as running it blind.
	fmt.Fprintf(stdout, "%s ships %d hook(s) that xdev is not running:\n", filepath.Join(root, ".xdev", "hooks"), len(pending))
	reportHooks(stdout, pending)
	if !yes {
		if !readerIsTerminal(stdin) {
			fmt.Fprintln(stderr, "xdev trust: not an interactive terminal; re-run with --yes to record the decision")
			return 2
		}
		fmt.Fprint(stdout, "trust this workspace and run these hooks? [y/N] ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if !answersYes(line) {
			fmt.Fprintln(stdout, "not trusted — the hooks stay withheld, and nothing was recorded")
			return 1
		}
	}
	approved, err := hookbus.TrustWorkspace(root)
	if err != nil {
		fmt.Fprintln(stderr, "xdev trust:", err)
		return 1
	}
	fmt.Fprintf(stdout, "trusted %d hook(s) from %s; recorded in %s\n", len(approved), root, hookbus.TrustPath())
	fmt.Fprintln(stdout, "they are withheld again automatically if any of them is edited")
	return 0
}

func distrustWorkspace(root string, stdout, stderr io.Writer) int {
	removed, err := hookbus.UntrustWorkspace(root)
	if err != nil {
		fmt.Fprintln(stderr, "xdev distrust:", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(stdout, "%s was not trusted; nothing changed\n", root)
		return 0
	}
	fmt.Fprintf(stdout, "distrusted %s — its repository hooks are withheld from now on\n", root)
	return 0
}

// reportHooks is the review surface: the event each hook fires on, the command
// it runs, the file it came from, and the file's contents. The last part is not
// optional: a shell hook's command is just its own path, so showing the path is
// showing nothing.
func reportHooks(w io.Writer, hooks []hookbus.Hook) {
	for _, h := range hooks {
		fmt.Fprintf(w, "  %s → %s\n      command: %s\n      file:    %s\n", h.Name, h.Event, h.Command, h.Path)
		for _, line := range previewFile(h.Path) {
			fmt.Fprintf(w, "      %s\n", line)
		}
	}
}

// maxPreviewLines bounds the review: enough to see what a hook does, not enough
// for a repository to bury the answer in noise.
const maxPreviewLines = 12

// previewFile returns the head of a hook file with line numbers, or a line
// saying it could not be read.
func previewFile(path string) []string {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{"(could not read the file: " + err.Error() + ")"}
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	shown := min(len(lines), maxPreviewLines)
	out := make([]string, 0, shown+1)
	for _, l := range lines[:shown] {
		out = append(out, fmt.Sprintf("%3d| %s", len(out)+1, l))
	}
	if len(lines) > shown {
		out = append(out, fmt.Sprintf("     … %d more line(s) — read the whole file before trusting it", len(lines)-shown))
	}
	return out
}

// readerIsTerminal reports whether a human can be asked. The launch path has a
// matching check for the process stdin; this is the injected-reader form.
func readerIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// answersYes accepts the two spellings and nothing else: an empty line, a typo,
// or an EOF at the prompt all mean "do not trust".
func answersYes(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
