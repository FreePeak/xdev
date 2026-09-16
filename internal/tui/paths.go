package tui

import (
	"strings"
)

// @path completion (M7 #8, PRD §IV.6): typing `@` in the composer suggests
// project files, and Tab replaces the token.
//
// Completion is directory-scoped, the way omp's is: the typed token names ONE
// directory plus a prefix inside it, so a keystroke costs one readdir of that
// directory — never a walk of the repo. That is what makes offering hidden and
// gitignored paths affordable: the walk was the cost, and there is no longer a
// walk per keystroke. The whole-repo scan survives only as the bare-`@`
// fallback (nothing to scope to), and it rides the shared FS-scan cache grep
// and glob already use, so it is one walk per TTL rather than one per keystroke.
//
// Sources are injected so this package stays free of tool/cache imports.

// maxPathCandidates bounds the candidate pool the dropdown is built from.
const maxPathCandidates = 200

// PathEntry is one directory entry offered by the completion source. A caller
// names the directory and returns its raw entries; scoping, prefix matching and
// ordering are this file's job, so the two never disagree about what the menu
// shows.
type PathEntry struct {
	Name  string
	IsDir bool
}

// skipName reports whether a single entry name never reaches the dropdown:
// VCS metadata is machine state, never a path anyone means to mention. Hidden
// and gitignored names are deliberately NOT skipped — offering those is the
// point of reading the directory ourselves instead of walking it.
func skipName(name string) bool {
	switch name {
	case ".git", ".hg", ".svn":
		return true
	}
	return false
}

// skipScanRel reports whether a relative path from the whole-repo fallback is
// dropped. It is stricter than skipName: a dependency tree may be named
// explicitly (`@node_modules/…` reads that one directory), but letting every
// vendored file into the bare-`@` list would bury the project's own paths.
func skipScanRel(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if skipName(part) || part == "node_modules" {
			return true
		}
	}
	return false
}

// pathMention completes name as an `@` mention on top of prefix, resolving
// from the completion root: name is one entry inside dir, so the mention has to
// carry the directory back. A path with spaces is quoted (omp does the same) —
// otherwise the space would end the mention and the completion would be a lie
// about what gets read. A directory leaves its quote OPEN (and keeps its
// trailing slash) so drilling inside it keeps working; only a file ends the
// mention, with a closing quote.
func pathMention(prefix, dir, name string) string {
	full := name
	if dir != "" && dir != "." {
		full = dir + "/" + name
	}
	if !strings.ContainsAny(full, " \t") {
		return prefix + "@" + full
	}
	if strings.HasSuffix(full, "/") {
		return prefix + `@"` + full // still typing inside the directory
	}
	return prefix + `@"` + full + `"`
}

// SetPathCompletion enables `@`-file completion: root is the directory shown in
// the menu header, list reads ONE directory (relative to root, "" for the root
// itself), scan is the whole-repo fallback for a bare `@`. A nil list disables
// the feature; a nil scan only disables the bare-`@` listing.
func (a *App) SetPathCompletion(root string, list func(dir string) []PathEntry, scan func() []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pathRoot, a.pathList, a.pathScan = root, list, scan
}

// pathCandidates returns the menu items for the path dropdown as typed so far.
// An empty token (`@` with nothing after it) names no directory, so it falls
// back to the whole-repo scan (omp answers the same token with a global fuzzy
// find); `@/` does name one — the root — and lists it. With no scan wired the
// empty token simply lists the root directory rather than showing nothing.
func (a *App) pathCandidates(query string) []suggestion {
	if a.pathList == nil {
		return nil
	}
	dir, seg := splitPathQuery(query)
	// Only an empty token falls back to the scan. `@/` names the root, so it
	// must list the root (splitPathQuery strips the slash to ("", "")) — keying
	// this on dir/seg instead would send an explicit root at the whole repo.
	if strings.TrimSpace(query) == "" && a.pathScan != nil {
		return a.scanCandidates()
	}
	lower := strings.ToLower(seg)
	var dirs, files []string
	for _, e := range a.pathList(dir) {
		if e.Name == "" || skipName(e.Name) {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(e.Name), lower) {
			continue
		}
		if e.IsDir {
			dirs = append(dirs, e.Name+"/") // the slash is how drilling in reads
		} else {
			files = append(files, e.Name)
		}
	}
	// omp order: directories first, then files, each already sorted by the
	// reader (os.ReadDir sorts).
	out := make([]suggestion, 0, min(len(dirs)+len(files), maxPathCandidates))
	for _, name := range append(dirs, files...) {
		if len(out) == maxPathCandidates {
			break
		}
		out = append(out, suggestion{Name: name, Tag: "path", kind: kindPath})
	}
	return out
}

// scanCandidates is the bare-`@` fallback: the whole-repo listing, capped.
// ponytail: it shows the first entries the walk reaches rather than the best
// ones — ranking 100k+ paths on every keystroke is exactly the cost this file
// exists to avoid. Upgrade path: have the scanner keep an mtime-ordered index.
func (a *App) scanCandidates() []suggestion {
	if a.pathScan == nil {
		return nil
	}
	files := a.pathScan()
	out := make([]suggestion, 0, min(len(files), maxPathCandidates))
	for _, rel := range files {
		if len(out) == maxPathCandidates {
			break
		}
		if rel == "" || rel == "." || skipScanRel(rel) {
			continue
		}
		out = append(out, suggestion{Name: rel, Tag: "path", kind: kindPath})
	}
	return out
}

// splitPathQuery splits the typed token into the directory to read and the
// prefix to match inside it: "internal/tu" -> ("internal", "tu"), and a
// trailing slash ("internal/") lists the directory it names.
//
// Paths stay root-relative: a leading "/" is stripped. ".." is left alone and
// resolves against the root the way the shell would — omp's completion offers
// `../` too, and refusing it would strand anyone completing out of a subdir.
func splitPathQuery(q string) (dir, seg string) {
	q = strings.TrimPrefix(strings.TrimSpace(q), "/")
	if i := strings.LastIndexByte(q, '/'); i >= 0 {
		return q[:i], q[i+1:]
	}
	return "", q
}

// pathToken splits the editor text at the `@` token under the cursor.
// Returns (prefix, query, ok): the text so far is prefix + "@" + query.
// A `@"…"` token carries a path with spaces; an unclosed quote is still being
// typed, a closed one has finished the token.
func pathToken(text string) (prefix, query string, ok bool) {
	i := strings.LastIndexByte(text, '@')
	if i < 0 {
		return "", "", false
	}
	if i > 0 {
		switch text[i-1] {
		case ' ', '\t', '\n':
		default:
			return "", "", false // mid-word @ (an email, prose) — never a path
		}
	}
	tail := text[i+1:]
	if strings.HasPrefix(tail, `"`) {
		if strings.ContainsRune(tail[1:], '"') {
			return "", "", false // the closing quote ended the token
		}
		return text[:i], tail[1:], true
	}
	if strings.ContainsAny(tail, " \t\n") {
		return "", "", false // the token is already finished
	}
	return text[:i], tail, true
}
