package tool

// Unified-diff generation for tool results. A file change's diff rides in
// Result.Details (NEVER the model-visible Text), so the TUI and print modes
// can colour it. The shape is what `git diff` prints ("--- / +++ / @@" plus
// +/-/context rows), so one renderer downstream covers a tool's own edit and
// a captured `git diff` alike.

import (
	"strconv"
	"strings"
)

// diffContext is how many unchanged lines a hunk shows on each side.
const diffContext = 3

// diffSizeCeiling bounds how big a change may carry a diff: past it,
// building and painting the diff costs more than it shows, and the session
// store would bloat with a payload nobody reads.
//
// ponytail: one global ceiling, no per-hunk budgeting. Upgrade path if a
// huge legitimate refactor ever needs colouring: stream the diff in windows.
const diffSizeCeiling = 4000

// diffDPBudget caps the LCS table (cells) for the changed middle. Beyond it
// the middle is emitted as delete-all-then-add-all: still a correct diff,
// just not a minimal one.
//
// ponytail: real edits — after the prefix/suffix trim — fit far inside a
// million cells, and a whole-file rewrite whose middle blocks match in order
// is rare enough that rendering it as one -/+ block is the honest picture
// anyway. Upgrade path: Myers with linear space (git's default) if minimal
// middle diffs ever matter.
const diffDPBudget = 1_000_000

// UnifiedDiff renders before → after as a one-file unified diff
// ("--- path" / "+++ path" headers plus @@-hunks with diffContext lines of
// context). ok is false when the sides are identical (nothing to show) or the
// change is past the size ceiling.
func UnifiedDiff(path string, before, after []string) (string, bool) {
	if EqualLines(before, after) || len(before)+len(after) > diffSizeCeiling {
		return "", false
	}
	ops := diffOps(before, after)
	var sb strings.Builder
	// The conventional a/ b/ prefixes, and /dev/null for the side that has no
	// lines: git's own spelling, so a captured diff and a generated one classify
	// the same way in the renderer.
	beforeName, afterName := "a/"+path, "b/"+path
	if len(before) == 0 {
		beforeName = devNull
	}
	if len(after) == 0 {
		afterName = devNull
	}
	sb.WriteString("--- " + beforeName + "\n")
	sb.WriteString("+++ " + afterName + "\n")
	for _, h := range buildHunks(ops) {
		sb.WriteString("@@ -")
		sb.WriteString(formatRange(h.aStart, h.aCount))
		sb.WriteString(" +")
		sb.WriteString(formatRange(h.bStart, h.bCount))
		sb.WriteString(" @@\n")
		for _, o := range ops[h.lo:h.hi] {
			sb.WriteByte(o.kind)
			if o.kind == '+' {
				sb.WriteString(after[o.b])
			} else {
				sb.WriteString(before[o.a])
			}
			sb.WriteByte('\n')
		}
	}
	return sb.String(), true
}

// EqualLines reports whether two line slices hold exactly the same lines.
func EqualLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SplitLines converts file bytes to lines the way ReadLines does: a single
// trailing newline is not a line, so "a\n" is one line and "" is none.
func SplitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// --- the edit script -------------------------------------------------------------

// diffOp is one emitted row: ' ' context (a and b both name a line), '-'
// deletion (b is -1), '+' insertion (a is -1). Indices are 0-based.
// devNull is the side of a diff for a file that does not exist yet (or any
// more).
const devNull = "/dev/null"

type diffOp struct {
	kind byte
	a, b int
}

// diffOps expands before → after into a flat op sequence. Common prefix and
// suffix lines are taken as context; the remaining middle gets a minimal LCS
// diff while it fits the table budget, a whole-middle replace beyond it.
func diffOps(a, b []string) []diffOp {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head &&
		a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	ops := make([]diffOp, 0, head+len(a)+len(b))
	for i := 0; i < head; i++ {
		ops = append(ops, diffOp{' ', i, i})
	}
	midA, midB := a[head:len(a)-tail], b[head:len(b)-tail]
	if len(midA)*len(midB) <= diffDPBudget {
		for _, s := range lcsOps(midA, midB) {
			// -1 means "this op has no line on that side". Offsetting the
			// sentinel turns it into a real index and inflates the header of
			// every hunk that follows a common prefix.
			a, b := s.a, s.b
			if a >= 0 {
				a += head
			}
			if b >= 0 {
				b += head
			}
			ops = append(ops, diffOp{s.kind, a, b})
		}
	} else {
		for i := range midA {
			ops = append(ops, diffOp{'-', head + i, -1})
		}
		for j := range midB {
			ops = append(ops, diffOp{'+', -1, head + j})
		}
	}
	for i := 0; i < tail; i++ {
		ops = append(ops, diffOp{' ', len(a) - tail + i, len(b) - tail + i})
	}
	return ops
}

// lcsOps is the classic LCS dynamic program over a middle section, indices
// relative to the section. Ties walk deletions first, so a replace reads
// "-then-+" the way every diff viewer renders one.
func lcsOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	stride := m + 1
	dp := make([]int32, (n+1)*stride) // row n and column m stay 0
	at := func(i, j int) int32 { return dp[i*stride+j] }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i*stride+j] = at(i+1, j+1) + 1
			case at(i+1, j) >= at(i, j+1):
				dp[i*stride+j] = at(i+1, j)
			default:
				dp[i*stride+j] = at(i, j+1)
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', i, j})
			i++
			j++
		case at(i+1, j) >= at(i, j+1):
			ops = append(ops, diffOp{'-', i, -1})
			i++
		default:
			ops = append(ops, diffOp{'+', -1, j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', i, -1})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', -1, j})
	}
	return ops
}

// --- hunk assembly ---------------------------------------------------------------

// hunk is the op-index range [lo, hi) plus the 1-based line ranges the @@
// header reports.
type hunk struct {
	lo, hi         int
	aStart, aCount int
	bStart, bCount int
}

// buildHunks segments the op stream into hunks: each change reaches out
// diffContext context lines, and hunks whose windows touch merge into one.
func buildHunks(ops []diffOp) []hunk {
	touched := make([]bool, len(ops))
	for i, o := range ops {
		if o.kind == ' ' {
			continue
		}
		lo, hi := i-diffContext, i+diffContext
		if lo < 0 {
			lo = 0
		}
		if hi > len(ops)-1 {
			hi = len(ops) - 1
		}
		for k := lo; k <= hi; k++ {
			touched[k] = true
		}
	}
	var out []hunk
	for i := 0; i < len(ops); {
		if !touched[i] {
			i++
			continue
		}
		h := hunk{lo: i}
		for ; i < len(ops) && touched[i]; i++ {
			o := ops[i]
			if o.a >= 0 {
				if h.aCount == 0 {
					h.aStart = o.a + 1
				}
				h.aCount++
			}
			if o.b >= 0 {
				if h.bCount == 0 {
					h.bStart = o.b + 1
				}
				h.bCount++
			}
		}
		h.hi = i
		out = append(out, h)
	}
	return out
}

// formatRange renders a unified-diff "start,count" range: a one-line range
// drops its ",1", an empty range is the line before plus ",0".
func formatRange(start, count int) string {
	switch {
	case count == 0:
		// The line before the insertion point; at the very start of a file
		// that is line 0, never the negative start-1 would report.
		return strconv.Itoa(max(start-1, 0)) + ",0"
	case count == 1:
		return strconv.Itoa(start)
	default:
		return strconv.Itoa(start) + "," + strconv.Itoa(count)
	}
}
