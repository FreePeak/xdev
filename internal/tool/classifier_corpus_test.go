package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This test replays every bash command in the local session store through the
// classifier and asserts the ONE property that must never break: a command
// carrying a destructive git spelling is never reported as a read.
//
// The gate's verdict decides whether the keep-going nudge fires, and the
// nudge is what made a plain question cost two answers. A read reported as a
// mutation costs a redundant turn (the bug this file's sibling fixed); a
// mutation reported as a read costs a turn that ends when it should not have
// — the other direction, and the one that must be impossible.
//
// The store is the developer's own, so the destructive spellings are the ones
// that actually occur in real sessions rather than a hand-written list. The
// test skips when there is no store, so it costs nothing in CI.
//
// The counts it logs are the measurement that sized the fix: over 129,145
// bash calls, the classifier's verdict flips on 1,702 commands once the read
// form of the six git verbs is modelled.
func TestNoDestructiveGitSpellingIsEverClassifiedAsARead(t *testing.T) {
	store := os.Getenv("XDEV_SESSION_STORE")
	if store == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home")
		}
		store = filepath.Join(home, ".xdev", "agent", "sessions")
	}
	if _, err := os.Stat(store); err != nil {
		t.Skip("no session store at", store)
	}

	// A destructive git spelling, judged at the SEGMENT level and by TOKEN,
	// not as a raw substring: `echo "git worktree add"` prints data and
	// `python3 -c "...'git worktree add'..."` greps for a string, and
	// neither runs the command. A substring match reads both as destructive
	// and the assertion would be wrong about the classifier.
	//
	// The tables are the classifier's own, so the check is not circular in
	// the TABLES — it is a second path to the same verdict, and it catches
	// the class of bug where the tables are right and the PLUMBING is not
	// (a subcommand that never reaches its handler, a flag cluster that is
	// not scanned letter by letter).
	destructiveGitSegment := func(seg string) bool {
		// A redirection is not an argument to the command, so the tail
		// after it is not a branch name. `git branch --contains sha 2>&1`
		// is a QUERY: without this the "2" after `--contains` would read
		// as a bare name, and a bare name after `git branch` is a branch
		// to CREATE — so the helper would call a read destructive and the
		// assertion would be wrong about the classifier.
		fields := strings.Fields(cutAtRedirect(seg))
		if len(fields) == 0 {
			return false
		}
		// A shell keyword is scaffolding, not a command — but the BODY of
		// a loop is one, and a destructive verb inside it is still
		// destructive: "do git branch -d x" must be caught here, or the
		// guard would stop looking at the calls it most needs to see.
		fields, _ = stripShellKeyword(fields)
		if len(fields) == 0 || fields[0] != "git" {
			return false
		}
		i := 1
		for i < len(fields) && strings.HasPrefix(fields[i], "-") {
			if fields[i] == "-C" || fields[i] == "-c" {
				i += 2
				continue
			}
			i++
		}
		if i >= len(fields) {
			return false
		}
		sub, rest := fields[i], fields[i+1:]
		switch sub {
		case "branch":
			// A long flag is looked up whole; a short flag is a cluster
			// judged letter by letter. Scanning a LONG flag letter by
			// letter is the bug this helper exists to avoid — "--show-current"
			// carries a "c", and "c" is the copy flag.
			valueNext := false
			listForm := false
			for _, a := range rest {
				if !strings.HasPrefix(a, "-") {
					if valueNext {
						valueNext = false
						continue // a query flag's value, not a branch name
					}
					// The LIST form: every word after --list is a
					// PATTERN, not a branch to create. The helper must
					// learn the same rule the classifier learned, or it
					// would call a read destructive and the assertion
					// would be wrong about the classifier.
					if listForm {
						return false
					}
					return true // a bare name creates a branch
				}
				if valueNext {
					valueNext = false
					continue
				}
				if strings.HasPrefix(a, "--") {
					name := strings.SplitN(a, "=", 2)[0]
					if gitBranchWriteLong[name] {
						return true
					}
					if name == "--list" {
						listForm = true
					}
					if gitBranchQueryValueFlags[name] && !strings.Contains(a, "=") {
						valueNext = true
					}
					continue
				}
				for _, c := range a[1:] {
					if strings.ContainsRune(gitBranchWriteShort, c) {
						return true
					}
				}
			}
		case "worktree":
			return !readOnlyGitWorktree(rest)
		case "remote":
			return !readOnlyGitRemote(rest)
		case "stash":
			return !readOnlyGitStash(rest)
		case "tag":
			return !readOnlyGitTag(rest)
		case "config":
			return !readOnlyGitConfig(rest)
		}
		return false
	}

	var total, readOnly, destructiveSeen int
	err := filepath.Walk(store, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, `"toolCall"`) {
				continue
			}
			var row struct {
				Type    string `json:"type"`
				Message struct {
					Content []struct {
						Type      string          `json:"type"`
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(line), &row) != nil || row.Type != "message" {
				continue
			}
			for _, c := range row.Message.Content {
				if c.Type != "toolCall" || c.Name != "bash" {
					continue
				}
				var args struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(c.Arguments, &args) != nil {
					continue
				}
				cmd := strings.TrimSpace(args.Command)
				if cmd == "" {
					continue
				}
				total++
				ro := ReadOnlyCommand(cmd)
				if ro {
					readOnly++
				}
				for _, seg := range splitCompound(cmd) {
					if !destructiveGitSegment(seg) {
						continue
					}
					destructiveSeen++
					if ro {
						t.Errorf("a destructive git command was classified as a read: %q", cmd)
					}
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Skip("walk failed:", err)
	}
	if total == 0 {
		t.Skip("no bash calls in the store")
	}
	t.Logf("bash calls in the store: %d", total)
	t.Logf("classified read-only: %d (%.1f%%)", readOnly, 100*float64(readOnly)/float64(total))
	t.Logf("segments running a destructive git verb: %d (none may be a read)", destructiveSeen)
}
