package tui

import (
	"sort"
	"strings"
)

// @path completion (M7 #8, PRD §IV.6): typing `@` in the composer suggests
// project files, fuzzy-ranked, and Tab replaces the token. Candidates come
// from an injected scanner so this package stays free of tool/cache
// imports — cmd wires the SAME shared FS-scan cache grep and glob use, so
// opening the menu costs one walk per TTL rather than one per keystroke.

// maxPathCandidates bounds the candidate pool before ranking.
const maxPathCandidates = 200

// SetPathCompletion enables `@`-file completion: root is the directory
// shown in the menu header, scan returns project-relative paths (newest
// first). A nil scan disables the feature.
func (a *App) SetPathCompletion(root string, scan func() []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pathRoot, a.pathScan = root, scan
}

// pathCandidates returns the menu items for the path dropdown, ranked by
// the same fuzzy scorer the slash menu uses.
func (a *App) pathCandidates(query string) []suggestion {
	if a.pathScan == nil {
		return nil
	}
	type hit struct {
		rel   string
		score int
	}
	var hits []hit
	for _, rel := range a.pathScan() {
		if rel == "" || rel == "." {
			continue
		}
		s := -1
		if strings.TrimSpace(query) != "" {
			// Rank on the basename first (that is how people type paths),
			// falling back to the full relative path. A shallower path wins
			// the tie, so `cache` offers cache.go before deep/cache.go.
			if v := fuzzyScore(filepathBase(rel), query); v >= 0 {
				s = v + shallownessBonus(rel)
			} else if v := fuzzyScore(rel, query); v >= 0 {
				s = v
			}
		}
		if s >= 0 || strings.TrimSpace(query) == "" {
			hits = append(hits, hit{rel: rel, score: s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > maxPathCandidates/4 {
		hits = hits[:maxPathCandidates/4]
	}
	out := make([]suggestion, 0, len(hits))
	for _, h := range hits {
		out = append(out, suggestion{Name: h.rel, Tag: "path", kind: kindPath})
	}
	return out
}

// pathToken splits the editor text at the `@` token under the cursor.
// Returns (prefix, query, ok): the text so far is prefix + "@" + query.
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
	if strings.ContainsAny(tail, " \t\n") {
		return "", "", false // the token is already finished
	}
	return text[:i], tail, true
}

// shallownessBonus prefers root-level and shallow files on score ties:
// fewer separators means a shorter path to type and read.
func shallownessBonus(rel string) int {
	return 8 - strings.Count(rel, "/")
}

// filepathBase avoids importing path/filepath in this file's hot path.
func filepathBase(rel string) string {
	if j := strings.LastIndexByte(rel, '/'); j >= 0 {
		return rel[j+1:]
	}
	return rel
}
