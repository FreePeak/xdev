package tool

// The hashline patch grammar — omp's edit input format, which is what a
// model's muscle memory actually writes (PRD §3.6). One header line names the
// file (optionally with the snapshot tag the read or the last edit printed),
// then op lines name 1-based line ranges, then "+"-prefixed body rows carry
// the final content.
//
// The structured {path, ops} encoding still parses (decodeEditArgs in
// edit.go): it is the same verb vocabulary, and it is what the harness's own
// tests speak. What changed is the model-facing contract — the JSON-ops form
// was the only one advertised, and live sessions showed the model sending
// "ops" as a JSON string, or a whole patch as a bare string, and being
// answered "cannot unmarshal string into Go struct field".

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// hashlineSection is one file's patch: the header it came from plus its ops.
type hashlineSection struct {
	path string // header content: "file.go" or "file.go#1a2b" ("" = none)
	ops  []editOp
}

var (
	// hashlineHeaderRe matches the "[path]" / "[path#tag]" section header.
	hashlineHeaderRe = regexp.MustCompile(`^\[\s*([^\[\]]*?)\s*\]\s*$`)
	// hashlinePutRe matches "PUT 3:", "PUT 3.=5:", "PUT 3-5:", "PUT 3..5:",
	// the same with no trailing colon, an optional "@register" suffix, and
	// "PUT <3:" / "PUT >3:" (insert before / after the named line, which the
	// anchor keeps). A row may sit on the op line only after a colon and only
	// with its "+"; anything else ("PUT 2*:") is not a range this grammar has,
	// so it falls through to the message that says so instead of being read as
	// inline content.
	hashlinePutRe = regexp.MustCompile(`^PUT\s+([<>])?\s*(\d*)\s*(?:[.=]{1,2}\s*(\d*))?\s*(?:@\S+)?\s*(?::\s*(\+.*?)?)?\s*$`)
	// hashlineCutRe matches "CUT 3" / "REM 3.=5" (colon tolerated, body not).
	hashlineCutRe = regexp.MustCompile(`^(CUT|REM)\s+([<>])?\s*(\d+)\s*(?:[.=]{1,2}\s*(\d+))?\s*(?:@\S+)?\s*:?\s*$`)
	// hashlineMvRe matches "MV dest" or "MV src dest".
	hashlineMvRe = regexp.MustCompile(`^MV\s+(\S.*?)\s*$`)
	// hashlineColonRe catches the colon range spellings models carry over
	// from other hashline dialects — "PUT 15:=24:" (live bench trace,
	// 2026-09-15: one wasted edit round per run) and "PUT 15:24" — and is
	// normalized to the canonical "15.=24" below before matching. It only
	// fires when digits stand after the colon, so "PUT 3:" keeps its
	// single-line meaning and "PUT 3: +row" keeps its inline body.
	hashlineColonRe = regexp.MustCompile(`^(PUT\s+(?:[<>]\s*)?\d+)\s*:(=?)\s*(\d+)`)
	// hashlineDiffRe matches a unified-diff hunk header, "@@ -1,3 +1,3 @@".
	// Some models reach for the diff dialect regardless of the advertised
	// schema; naming it turns a parser complaint into a one-line correction.
	hashlineDiffRe = regexp.MustCompile(`^@@\s*-?\d`)
)

// looksLikeHashline reports whether s reads as patch text rather than some
// other string a model crammed into an argument: it needs a section header
// naming a plausible file, or an op line.
func looksLikeHashline(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimLeft(strings.TrimRight(line, " \t"), " \t")
		if m := hashlineHeaderRe.FindStringSubmatch(line); m != nil {
			if plausibleHeaderPath(strings.TrimSpace(m[1])) {
				return true
			}
			continue
		}
		if hashlinePutRe.MatchString(line) || hashlineCutRe.MatchString(line) || hashlineMvRe.MatchString(line) {
			return true
		}
	}
	return false
}

// plausibleHeaderPath screens a "[...]" header's content. The one shape that
// must not be read as a path is the double-encoded op array — a model that
// quotes the whole JSON value gets "[{\"op\": ...]", whose brackets make it
// look like a header to a regex alone.
func plausibleHeaderPath(s string) bool {
	if s == "" || strings.ContainsAny(s, "\"{}") {
		return false
	}
	return !strings.HasPrefix(s, "[") && !strings.HasPrefix(s, "{")
}

// parseHashline reads one patch document. defaultPath (may be "") is the path
// the call carried outside the text, used when there is no header line. Errors
// name the offending input line so the model fixes that line instead of
// re-deriving the format from scratch.
func parseHashline(text, defaultPath string) (hashlineSection, error) {
	var sec hashlineSection
	sec.path = defaultPath

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	// A document that carries a hunk header is a unified diff, not a patch in
	// this grammar: say so once, on the hunk line, before the parse loop turns
	// the diff's "--- a/f" first line into a confusing "[path] header" error.
	for n, l := range lines {
		if hashlineDiffRe.MatchString(strings.TrimLeft(l, " \t")) {
			return sec, fmt.Errorf("edit: line %d: %q is a unified-diff hunk header — this tool does not take diffs. Send the hashline form: a \"[path]\" header, then \"PUT 3.=5:\" / \"CUT 5\" / \"REM 5.=7\" / \"MV new.go\" over 1-based line numbers, each PUT followed by \"+\" body rows", n+1, truncateOneLine(l, 40))
		}
	}
	i := 0
	headerSeen := false
	for i < len(lines) {
		lineNo := i + 1
		raw := strings.TrimLeft(strings.TrimRight(lines[i], " \t"), " \t")
		i++
		if raw == "" {
			continue
		}
		if m := hashlineHeaderRe.FindStringSubmatch(raw); m != nil {
			head := strings.TrimSpace(m[1])
			if !plausibleHeaderPath(head) {
				return sec, fmt.Errorf("edit: line %d: [%s] is not a file header — expected [path] or [path#tag], and a JSON-encoded op list belongs in ops, not quoted as text", lineNo, truncateOneLine(raw, 60))
			}
			if headerSeen {
				return sec, fmt.Errorf("edit: line %d: one [path] header per edit call — send the second file as its own edit", lineNo)
			}
			sec.path, headerSeen = head, true
			continue
		}
		// A file path on its own line (no brackets) is the other way a model
		// names the file when the call carries no path field.
		if !headerSeen && defaultPath == "" && looksLikeBarePath(raw) && i < len(lines) {
			next := strings.TrimLeft(lines[i], " \t")
			if hashlinePutRe.MatchString(next) || hashlineCutRe.MatchString(next) || hashlineMvRe.MatchString(next) {
				sec.path, headerSeen = raw, true
				continue
			}
		}
		op, n, err := parseHashlineOp(raw, i, lines, lineNo)
		if err != nil {
			return sec, err
		}
		i = n
		if op.Op == "PUT" && op.Anchor != "" && len(op.Lines) == 0 {
			return sec, fmt.Errorf("edit: line %d: %s inserts, so it needs \"+\" body rows (an insert of nothing changes nothing)", lineNo, raw)
		}
		sec.ops = append(sec.ops, op)
	}
	if len(sec.ops) == 0 {
		return sec, fmt.Errorf("edit: the patch names no ops — expected a \"[path]\" header and lines like \"PUT 3.=5:\" followed by \"+content\" rows, \"CUT 5\", \"REM 5.=7\" or \"MV new/path\"")
	}
	return sec, nil
}

// parseHashlineOp parses one op line at input line lineNo and, for a PUT,
// consumes the body rows under it. n is the index of the next unconsumed line.
func parseHashlineOp(raw string, next int, lines []string, lineNo int) (editOp, int, error) {
	raw = hashlineColonRe.ReplaceAllString(raw, "$1.=$3")
	if m := hashlinePutRe.FindStringSubmatch(raw); m != nil {
		op := editOp{Op: "PUT"}
		switch m[1] {
		case "<":
			op.Anchor = "before"
		case ">":
			op.Anchor = "after"
		}
		start, end, err := hashlineRange(m[2], m[3], lineNo, raw)
		if err != nil {
			return op, next, err
		}
		op.Range = &editRange{Start: start, End: end}
		body, consumed, berr := hashlineBody(next, lines, lineNo)
		if berr != nil {
			return op, consumed, berr
		}
		// "PUT 3: +content" — one row on the op line itself. The "+" stays on
		// exactly as in the block form; the executor strips one.
		if m[4] != "" && len(body) == 0 {
			op.Lines = []string{m[4]}
		} else {
			op.Lines = body
		}
		return op, consumed, nil
	}
	if m := hashlineCutRe.FindStringSubmatch(raw); m != nil {
		if m[2] != "" {
			return editOp{}, next, fmt.Errorf("edit: line %d: %s deletes; only PUT takes the %q insert anchor", lineNo, m[1], m[2])
		}
		start, end, err := hashlineRange(m[3], m[4], lineNo, raw)
		if err != nil {
			return editOp{}, next, err
		}
		return editOp{Op: m[1], Range: &editRange{Start: start, End: end}}, next, nil
	}
	if m := hashlineMvRe.FindStringSubmatch(raw); m != nil {
		fields := strings.Fields(m[1])
		switch len(fields) {
		case 1:
			return editOp{Op: "MV", Dest: fields[0]}, next, nil
		case 2:
			// "MV src dest": the section header already names the source, so
			// only the destination is used.
			return editOp{Op: "MV", Dest: fields[1]}, next, nil
		default:
			return editOp{}, next, fmt.Errorf("edit: line %d: MV takes the destination path, got %d words", lineNo, len(fields))
		}
	}
	if isOpWord(firstWord(raw)) {
		return editOp{}, next, fmt.Errorf("edit: line %d: %q names a range this grammar cannot express (want e.g. \"PUT 3.=5:\" or \"CUT 5\")", lineNo, truncateOneLine(raw, 40))
	}
	return editOp{}, next, fmt.Errorf("edit: line %d: expected a PUT/CUT/REM/MV op line or a [path] header, got %q — a header is the path INSIDE the brackets (\"[src/a.go#1a2b]\"), on its own line, before any op", lineNo, truncateOneLine(raw, 40))
}

// hashlineBody consumes the rows under a PUT op line. A row is passed through
// verbatim, leading "+" included: the executor strips exactly one "+" from
// every body row, which is what makes "++x" a literal "+x" and a lone "+" a
// blank line in both encodings. A blank line between rows is a separator, not
// content. A line that is neither a row, an op, nor a header is reported — the
// "-" row in particular is the diff habit this grammar deliberately does not
// have (the range an op names is what says what disappears).
func hashlineBody(i int, lines []string, lineNo int) ([]string, int, error) {
	var body []string
	for i < len(lines) {
		raw := lines[i]
		if strings.TrimSpace(raw) == "" {
			i++
			continue
		}
		// Whitespace before a row's "+" is layout, not content; whitespace
		// after it is preserved (that is where a model puts indentation).
		head := strings.TrimLeft(raw, " \t")
		if strings.HasPrefix(head, "+") {
			body = append(body, head)
			i++
			continue
		}
		// Anything that starts a new section ends this one.
		if hashlineHeaderRe.MatchString(head) || isOpWord(firstWord(head)) {
			break
		}
		if strings.HasPrefix(head, "-") {
			return body, i + 1, fmt.Errorf("edit: line %d (under the op on line %d): \"-\" rows are not how this grammar deletes — leave the line out of the range the op names and give only \"+\" content rows", i+1, lineNo)
		}
		return body, i + 1, fmt.Errorf("edit: line %d (under the op on line %d): body rows start with \"+\", got %q — every row under a PUT carries exactly one leading \"+\" (it is transport: \"++\" writes a literal \"+\")", i+1, lineNo, truncateOneLine(raw, 40))
	}
	return body, i, nil
}

// hashlineRange resolves the two numbers of an op line into a 1-based
// inclusive range, defaulting the end to the start.
func hashlineRange(a, b string, lineNo int, raw string) (start, end int, err error) {
	if a == "" && b == "" {
		return 0, 0, fmt.Errorf("edit: line %d: %q names no line number — an op line reads \"PUT 3.=5:\", \"PUT 3:\", \"PUT <3:\" or \"CUT 5\"", lineNo, truncateOneLine(raw, 40))
	}
	if a == "" {
		a, b = b, ""
	}
	start, err = strconv.Atoi(a)
	if err != nil || start < 1 {
		return 0, 0, fmt.Errorf("edit: line %d: line numbers are 1-based integers, got %q", lineNo, truncateOneLine(raw, 40))
	}
	if b == "" {
		return start, start, nil
	}
	end, err = strconv.Atoi(b)
	if err != nil || end < 1 {
		return 0, 0, fmt.Errorf("edit: line %d: line numbers are 1-based integers, got %q", lineNo, truncateOneLine(raw, 40))
	}
	if end < start {
		start, end = end, start
	}
	return start, end, nil
}

// looksLikeBarePath reports whether a line plausibly names a file rather than
// carrying prose: one line, no spaces, and a path separator or an extension.
func looksLikeBarePath(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\"'`") || len(s) > 512 {
		return false
	}
	return strings.ContainsAny(s, "/\\") || filepath.Ext(s) != ""
}

// isOpWord reports whether a bare word is one of the grammar's op verbs.
func isOpWord(w string) bool {
	switch strings.ToUpper(w) {
	case "PUT", "CUT", "REM", "MV":
		return true
	}
	return false
}

// firstWord returns the leading word of a line, up to whitespace or ":".
func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t:"); i >= 0 {
		return s[:i]
	}
	return s
}

// truncateOneLine keeps an echoed input line to one row of error text, on a
// rune boundary because the echoed text is the model's own (and may be
// non-ASCII).
func truncateOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) > max-1 {
		r = r[:max-1]
	}
	return string(r) + "…"
}
