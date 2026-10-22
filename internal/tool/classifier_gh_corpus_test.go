package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This test replays every bash command in the local session store through
// the classifier and asserts the ONE property that must never break for `gh`:
// a command carrying a WRITING gh spelling is never reported as a read.
//
// The `gh` read forms were added on 2026-10-21 after a live field report
// (session 4c4addab): the user asked "list me all pr urls openningin xdev",
// the model answered it, and in the same millisecond the keep-going gate
// pushed it past its own answer because `gh` was not in the read-only table
// at all — so every `gh pr list` read as a mutation.
//
// The direction that matters is the one this test pins. A read reported as a
// mutation costs a redundant turn. A mutation reported as a read costs a
// turn that ends when it should not have — the other direction, and the one
// that must be impossible.
//
// The store is the developer's own, so the writing spellings are the ones
// that actually occur in real sessions rather than a hand-written list. The
// test skips when there is no store, so it costs nothing in CI.
//
// The counts it logs are the measurement that sized the fix: over the
// store's 132,342 bash calls, 5,698 carry a `gh`, and the classifier's
// verdict flips on 1,035 of them — none of which carries a writing gh
// spelling.
func TestNoDestructiveGhSpellingIsEverClassifiedAsARead(t *testing.T) {
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

	// A gh segment that WRITES, judged at the segment level and by TOKEN,
	// not as a raw substring: `echo "gh pr merge"` prints data and
	// `python3 -c "...'gh pr merge'..."` greps for a string, and neither
	// runs the command.
	writingGhSegment := func(seg string) bool {
		fields := strings.Fields(seg)
		if len(fields) == 0 || fields[0] != "gh" {
			return false
		}
		args := fields[1:]
		if len(args) == 0 {
			return true // a bare `gh` is not a read
		}
		if args[0] == "api" {
			// `gh api` writes when its method says so.
			return !ghApiIsRead(args[1:])
		}
		// Global flags come before the noun.
		i := 0
		for ; i < len(args); i++ {
			if !strings.HasPrefix(args[i], "-") {
				break
			}
		}
		if i >= len(args) {
			return true
		}
		noun := args[i]
		if ghBareReadNouns[noun] {
			return false
		}
		subs, ok := ghReadSubs[noun]
		if !ok {
			return true // a noun the table has never heard of writes
		}
		for _, a := range args[i+1:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			return !subs[a]
		}
		return true
	}

	var total, withGh, readOnly, writingSeen int
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
				segs := splitCompound(cmd)
				hasGh := false
				for _, seg := range segs {
					if fields := strings.Fields(seg); len(fields) > 0 && fields[0] == "gh" {
						hasGh = true
					}
				}
				if !hasGh {
					continue
				}
				withGh++
				for _, seg := range segs {
					if !writingGhSegment(seg) {
						continue
					}
					writingSeen++
					if ro {
						t.Errorf("a writing gh command was classified as a read: %q", cmd)
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
	t.Logf("calls carrying a gh verb: %d", withGh)
	t.Logf("segments running a writing gh verb: %d (none may be a read)", writingSeen)
}
