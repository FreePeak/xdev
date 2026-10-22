package tool

import (
	"regexp"
	"sort"
	"strings"
)

// CanonicalCommand renders a shell line in a form that groups the same WORK
// written differently.
//
// It exists for the keep-going gate's repeat NOTICE, and only for that. The
// notice is TEXT: it tells the model "you have already run this". A coarse
// key cannot hand the model a wrong answer — the tool-result cache keeps its
// own precise key (internal/agent/loop.go), because a result for one call is
// not the result for another.
//
// The problem it solves, measured on a live run (2026-10-22, session
// adf5029b): a model answering "list me all pr urls opening xdev" asked the
// same question in many spellings — `gh pr list`, `gh api repos/…/pulls`,
// `gh api graphql`, `gh search prs`, each with a different projection, a
// different page size and a different pipe. The repeat guard's key is the
// tool name plus `canonicalArgs`, which normalizes nothing but JSON key
// order, so every one of them was a DIFFERENT call: 28 tool calls, 28
// distinct keys, and not one repeat noticed.
//
// A canonical form has two parts: the PROGRAM PATH and the TARGETS.
//
//   - The PROGRAM PATH is the leading words of the segment — `gh api`,
//     `gh pr list`, `git`, `grep` — up to the first flag. Two different
//     programs are different work, and a hard task legitimately runs both
//     many times.
//   - The TARGETS are the remaining positional words and the values of the
//     flags that take one. They are SORTED, so the order does not matter.
//
// What is normalized inside a target:
//
//   - A PROJECTION flag's value is dropped (`--jq`, `--json`, `--template`,
//     `--field`, `--output`, `-o`, `--tsv`). The projection changes how the
//     result is RENDERED, not what was asked: `gh api X --jq 'length'` and
//     `gh api X --jq '.[]'` are one question, asked twice.
//   - A NUMBER becomes a placeholder. A page size, a limit, a line count is
//     a MAGNITUDE, not a target: `--limit 100` and `--limit 2000` are one
//     question at two depths.
//
// What is NOT normalized, and must not be:
//
//   - A flag's value, when it names the TARGET: `--repo A` and `--repo B` are
//     two different questions, and a notice that merged them would be
//     wrong.
//   - The program path, as above.
//
// A notice is a nudge, never a stop, so a coarse key that occasionally
// groups two similar-but-distinct calls costs a line of context, not a
// wrong answer.
func CanonicalCommand(command string) string {
	segs := splitCompound(command)
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		if fields := splitShellWords(seg); len(fields) > 0 && canonicalNoise[fields[0]] {
			continue
		}
		if c := canonicalSegment(seg); c != "" {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ; ")
}

// canonicalNumber matches a run of digits in a value.
var canonicalNumber = regexp.MustCompile(`\d+`)

// canonicalProjectionFlags are the flags that change HOW a result is
// rendered, not WHAT was asked. Their VALUE is dropped from the canonical
// form: the two calls are one question, asked twice.
var canonicalProjectionFlags = map[string]bool{
	"--jq": true, "--json": true, "--template": true, "--field": true,
	"--output": true, "--tsv": true, "-o": true, "-t": true,
}

// canonicalSegment renders one segment in canonical form.
func canonicalSegment(seg string) string {
	words := splitShellWords(seg)
	if len(words) == 0 {
		return ""
	}
	// The program path: the leading words up to the first flag.
	prog := []string{words[0]}
	i := 1
	for ; i < len(words); i++ {
		w := words[i]
		if strings.HasPrefix(w, "-") && w != "-" {
			break
		}
		// A bare word here is either a subcommand (`gh api`) or a
		// positional target. A subcommand is part of the program; a
		// positional is a target. The two are told apart by the word
		// AFTER: a subcommand is followed by a flag or another word of the
		// program, and a target is a value of a flag. The first word
		// after the program name is a subcommand when the program is a
		// known multi-word tool, and a target otherwise.
		prog = append(prog, w)
	}
	// The words after the program path: flags and their values, plus any
	// remaining positionals.
	var targets []string
	for ; i < len(words); i++ {
		w := words[i]
		if !strings.HasPrefix(w, "-") || w == "-" {
			// A POSITIONAL keeps its numbers: `sed -n '1,50p' f` and
			// `sed -n '200,250p' f` are the same file and DIFFERENT
			// ranges, and a notice that called them one question would
			// nag on a model reading a file in chunks.
			targets = append(targets, w)
			continue
		}
		if i+1 < len(words) && !strings.HasPrefix(words[i+1], "-") {
			if !canonicalProjectionFlags[w] {
				// A flag's VALUE may carry a magnitude: `--limit 100` and
				// `--limit 2000` are one question at two depths.
				targets = append(targets, canonicalValue(words[i+1]))
			}
			i++
		}
	}
	sort.Strings(targets)
	return strings.Join(prog, " ") + " | " + strings.Join(targets, " ")
}

// canonicalValue normalizes a value so a MAGNITUDE difference does not read
// as a different question.
//
// Only a PURE number is a magnitude: `--limit 100` and `--limit 2000` are one
// question at two depths, and `| head -40` and `| head -50` are the same
// pipe. A value that carries anything else is an identifier or a range and
// is left alone: `sed -n '1,50p' f` and `sed -n '200,250p' f` are the same
// file and DIFFERENT ranges, and a notice that called them one question
// would nag on a model reading a file in chunks.
func canonicalValue(v string) string {
	if canonicalPureNumber.MatchString(v) {
		return "N"
	}
	return v
}

// canonicalPureNumber matches a value that is only a number, with an
// optional unit suffix ("30s", "500m").
var canonicalPureNumber = regexp.MustCompile(`^\d+[smh]?$`)

// splitShellWords splits a segment into words, keeping a quoted word whole
// and keeping the quotes in it.
//
// The repo's splitter (splitCompound) is quote-aware about OPERATORS; this is
// the same care applied to WORDS, so `--jq '.[] | "x"'` is one word and not
// three.
func splitShellWords(seg string) []string {
	var words []string
	var cur strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			started = true
			cur.WriteByte(c)
		case c == ' ' || c == '\t':
			if started {
				words = append(words, cur.String())
				cur.Reset()
				started = false
			}
		default:
			started = true
			cur.WriteByte(c)
		}
	}
	if started {
		words = append(words, cur.String())
	}
	return words
}

// canonicalNoise are the commands that are NOT the work: the model's own
// labels and the shaping of their output. A call that pipes into `head` and
// labels its sections with `echo` is ONE question, and the noise must not be
// part of its identity — or every spelling of it is a different key and
// nothing groups.
//
// `cd` is NOT noise: it names the context the work happens in, and two
// commands in two repositories are two questions.
var canonicalNoise = map[string]bool{
	"echo": true, "printf": true, "true": true, "false": true, ":": true,
	"head": true, "tail": true, "wc": true, "bc": true, "sleep": true,
	"continue": true, "break": true, "exit": true,
}
