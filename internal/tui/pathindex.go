package tui

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// The background half of `@`-completion (paths.go): a listing of every FILE
// under the completion root, root-relative and slash-separated, so a bare name
// fragment (`@pa`) can offer `internal/tui/paths.go` instead of making the
// user type the directories in front of it.
//
// This is the walk #329 deleted from the keystroke path, moved off it: the
// same traversal, started once at wiring while the welcome screen is still
// up, and published atomically. A keystroke after that is a binary search in
// memory — never a filesystem call, and never a scan.

// pathHit is one indexed file: the path, and its base name already lowercased
// so matching does no per-keystroke allocation.
type pathHit struct {
	lower string // lowercased base name — the sort key and the match key
	path  string // root-relative, slash-separated
}

// pathIndex is that listing, sorted by base name. The build goroutine writes
// it, the UI thread reads it, so it is an atomic pointer rather than a
// mutex-guarded field: nothing on the keystroke path may wait on a writer.
type pathIndex struct {
	hits  atomic.Pointer[[]pathHit]
	built atomic.Bool // set when the walk has published
}

// start builds the index in the background and returns immediately, so
// wiring completion never blocks the caller. It is not cancellable and does
// not need to be: the walk holds no lock, writes once, and exits.
func (idx *pathIndex) start(root string) {
	go func() {
		hits := indexPathHits(root)
		idx.hits.Store(&hits)
		idx.built.Store(true)
	}()
}

// takeBuilt reports the index arriving exactly once — the UI tick asks, so an
// open dropdown is re-queried the moment the walk lands, with no channel, no
// lock, and no callback from the writer into the UI thread.
func (idx *pathIndex) takeBuilt() bool { return idx.built.CompareAndSwap(true, false) }

// match returns the indexed files whose base name starts with seg (already
// lowercased), capped at maxPathCandidates. The hits are sorted by that base
// name, so the answer is one binary search plus a walk of its run — a linear
// scan of 65k entries cost 2.4ms per keystroke, and would be ~30ms on the
// 811k-file polyrepo the original stall came from.
//
// A root-level file is skipped: the readdir the dropdown reads for the same
// query already offers it, and two rows for one path is a bug, not a feature.
//
// A nil index — nothing built yet — matches nothing, which is exactly what
// the dropdown showed before the index landed.
func (idx *pathIndex) match(seg string) []string {
	all := idx.hits.Load()
	if all == nil || seg == "" {
		return nil
	}
	lo, hi := 0, len(*all)
	for lo < hi { // first hit whose base name is >= seg
		if mid := (lo + hi) / 2; (*all)[mid].lower < seg {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	var out []string
	for _, hit := range (*all)[lo:] {
		if !strings.HasPrefix(hit.lower, seg) {
			break // the run of matches is over: the next name sorts higher
		}
		if strings.IndexByte(hit.path, '/') < 0 {
			continue // at the root already: the readdir offered it
		}
		out = append(out, hit.path)
		if len(out) == maxPathCandidates {
			break
		}
	}
	return out
}

// indexPathHits lists every file under root, root-relative and
// slash-separated, sorted by lowercased base name (ties by path, so the menu
// order is total and the tests can pin it).
//
// Directories are not indexed: the dropdown lists a directory through the
// readdir the token names — one call, always live, no staleness — so the
// index only has to answer what a readdir cannot: which file ANYWHERE in the
// tree carries this name.
//
// Hidden and gitignored paths ARE indexed, exactly as they are offered one
// directory at a time (paths.go skipName): a file you can drill into is a file
// you should be able to name.
//
// ponytail: one traversal, held in memory, never refreshed — the ceiling is
// named in indexSkipDir (vendor trees) and in the stamp block (a tree far
// larger than this one should share fscache's cached walk instead of keeping
// a second index). A file created after startup is still reachable, through
// the directory drill.
func indexPathHits(root string) []pathHit {
	if root == "" {
		return nil
	}
	var out []pathHit
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir // unreadable subtree: skip it, keep walking
			}
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if skipName(d.Name()) || indexSkipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		out = append(out, pathHit{lower: strings.ToLower(filepath.Base(rel)), path: rel})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].lower != out[j].lower {
			return out[i].lower < out[j].lower
		}
		return out[i].path < out[j].path
	})
	return out
}

// indexSkipDir reports a directory the index never walks into. These are
// dependency and build trees: measured on this polyrepo they are 95% of the
// files (811k of them) and every one is still reachable by naming its
// directory, so indexing them costs 227 MB against a hard 100 MB process
// budget and buys nothing a drill cannot reach.
//
// Deliberately NOT skipName's rule — node_modules/ and friends still appear
// in the dropdown, one readdir away, so a dependency path stays typeable.
func indexSkipDir(name string) bool {
	switch name {
	case "node_modules", ".worktrees", "vendor", "dist", "build", "target",
		".next", ".nuxt", ".turbo", ".venv", "venv", "__pycache__", ".gradle":
		return true
	}
	return false
}
