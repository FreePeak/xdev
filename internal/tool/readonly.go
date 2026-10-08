package tool

import (
	"encoding/json"
	"strings"
)

// ReadOnlyCommand reports whether a shell line is one the keep-going gate may
// treat as a pure read: every segment of the line names a command that cannot
// change the workspace.
//
// CONTRACT: this is a heuristic for asking "did this run change anything?"
// (agent.callMutated). It is NOT an approval decision and must never reach
// ApprovalPolicy.Decide — approval fails closed, and this fails open by
// construction. An unrecognised command is reported false, but a command it
// does model could still write, so treating true as "safe to run" would be
// wrong in the dangerous direction.
//
// The failure that matters here is the other way round. Reading a
// workspace-changing command as read-only costs one lost keep-going nudge
// (the run ends, the user re-asks); reading a pure read as mutating is what
// kept the loop nudging a finished run — measured at 649 prompt-continuations
// across 80 sessions, 3.6 per real user turn, because `bash` was hardcoded
// mutating and almost every session shells out for `git status` or `wc -c`.
//
// The segment splitter is policy.go's, already quote-aware and already the
// thing deny rules are judged with: a quoted `>` is data, and `$(rm -rf /)`
// splits into its own segment and fails the check.
func ReadOnlyCommand(command string) bool {
	segs := splitCompound(command)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		if hasWriteRedirect(seg) || !readOnlySegment(seg) {
			return false
		}
	}
	return true
}

// BashCallIsReadOnly is ReadOnlyCommand for a bash tool call's arguments. The
// argument shape stays here, where the bash tool and the policy resolver
// already decode it, so a second package never re-guesses the JSON.
func BashCallIsReadOnly(args json.RawMessage) bool {
	return ReadOnlyCommand(bashCommand(args))
}

// readOnlyHeads are the commands whose every invocation is a read. Commands
// with a write flag (sed -i, sort -o, curl -o) or a mutating subcommand (git)
// are handled by name in readOnlySegment, not here.
var readOnlyHeads = map[string]bool{
	"ls": true, "pwd": true, "cat": true, "head": true, "tail": true,
	"wc": true, "rg": true, "grep": true, "egrep": true, "fgrep": true,
	"find": true, "fd": true, "which": true, "type": true, "stat": true,
	"file": true, "du": true, "df": true, "env": true, "printenv": true,
	"date": true, "uname": true, "whoami": true, "id": true, "ps": true,
	"echo": true, "true": true, "false": true, "basename": true,
	"dirname": true, "realpath": true, "readlink": true, "jq": true,
	"uniq": true, "cut": true, "tr": true, "tac": true, "nl": true,
	"tree": true, "diff": true, "cmp": true, "md5": true, "shasum": true,
	"sha256sum": true, "hexdump": true, "xxd": true, "strings": true,
	"seq": true, "test": true, "hostname": true, "lsof": true, "sw_vers": true,
	// cd changes only the shell's own working directory, and the bash tool
	// gives every call a fresh process (a caller sets one with the workdir
	// argument), so it persists nothing across calls. It is also the single
	// highest-value entry here: measured over the 40 newest session files, 69%
	// of bash calls begin `cd <dir> && …`, so without it every such line failed
	// the whole-line check at its first segment and only 3.7% of real calls
	// classified read-only — the fix would have been invisible in practice.
	"cd": true,
}

// readOnlyGitSubs are the git subcommands with no mutating form worth
// modelling. Anything that can write from its own arguments is left out on
// purpose — `branch`, `tag`, `remote`, `config`, `stash`, `worktree` and
// `reflog` all have a destructive spelling, and parsing their flags to tell
// `git branch` from `git branch -D` is not worth the risk in a gate whose
// whole job is a heuristic.
var readOnlyGitSubs = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"rev-parse": true, "rev-list": true, "describe": true, "blame": true,
	"shortlog": true, "ls-files": true, "ls-remote": true, "ls-tree": true,
	"show-ref": true, "cat-file": true, "name-rev": true,
	"for-each-ref": true, "count-objects": true, "grep": true,
	"annotate": true, "whatchanged": true, "diff-tree": true,
	"diff-index": true, "diff-files": true, "cherry": true,
	"merge-base": true, "verify-commit": true, "verify-tag": true,
}

// readOnlySegment judges one already-split segment.
func readOnlySegment(seg string) bool {
	fields := strings.Fields(seg)
	// Leading VAR=value assignments do not change the command: `FOO=1 ls` is
	// an ls. A segment that is ONLY assignments is a write (it exports state),
	// so it falls through to the false below.
	for len(fields) > 0 && isAssignment(fields[0]) {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false
	}
	switch head := fields[0]; head {
	case "git":
		return readOnlyGit(fields[1:])
	case "sed":
		return !hasFlagContaining(fields[1:], 'i')
	case "sort":
		return !hasFlagContaining(fields[1:], 'o')
	case "curl", "curl.exe":
		return readOnlyCurl(fields[1:])
	case "python", "python3", "python2":
		return readOnlyPython(fields[1:])
	default:
		return readOnlyHeads[head]
	}
}

// readOnlyGit skips git's global flags (`-C dir`, `-c k=v`, `--no-pager`) to
// reach the subcommand, then judges the subcommand by name.
func readOnlyGit(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" ||
			a == "--namespace" || a == "--exec-path":
			i++ // takes a value
		case strings.HasPrefix(a, "-"):
			continue // a valueless global flag (--no-pager, --bare, -P)
		default:
			return readOnlyGitSubs[a]
		}
	}
	return false
}

// hasFlagContaining reports whether args carry a short flag cluster naming
// c, or its long spelling. `sed -ni`, `sed -i.bak` and `sort -o out` all
// match; `sed -e s/a/b/` does not.
func hasFlagContaining(args []string, c byte) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "--" {
			continue
		}
		if a == "--" {
			return true
		}
		if strings.HasPrefix(a, "--") {
			switch {
			case c == 'i' && a == "--in-place":
				return true
			case c == 'o' && (a == "--output" || strings.HasPrefix(a, "--output=")):
				return true
			}
			continue
		}
		if strings.ContainsRune(a, rune(c)) {
			return true
		}
	}
	return false
}

// isAssignment reports whether a field is a leading NAME=value shell
// assignment rather than a command.
func isAssignment(f string) bool {
	name, _, ok := strings.Cut(f, "=")
	if !ok || name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// hasWriteRedirect reports whether a segment redirects output to a real file.
// A quoted `>` is data (`grep "a>b" f`). `>/dev/null`, `2>/dev/null`, `&>/dev/null`
// and bare fd shuffles (`2>&1`, `>&2`) discard or re-pipe streams — they do not
// change the workspace, so they must not trip the keep-going gate. Session
// 724e3fbb's entire research bash surface used `2>/dev/null` on every probe,
// and the old "any > is a write" rule classified all 32 of them as mutations.
func hasWriteRedirect(seg string) bool {
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '>':
			// Walk the redirect operator: [n]>, [n]>>, [n]>&m, &>, &>>.
			j := i + 1
			if j < len(seg) && seg[j] == '>' {
				j++ // >>
			}
			if j < len(seg) && seg[j] == '&' {
				// `>&1`, `2>&1`, `&>` — fd shuffle / merge, not a file write.
				return false
			}
			for j < len(seg) && (seg[j] == ' ' || seg[j] == '\t') {
				j++
			}
			rest := seg[j:]
			switch {
			case rest == "":
				return false
			case strings.HasPrefix(rest, "/dev/null"):
				return false
			default:
				return true
			}
		}
	}
	return false
}

// readOnlyCurl is true when curl is used as a pure HTTP GET probe: no
// body/upload/output-file flags. Session research calls `curl -s URL` constantly;
// those must not buy a keep-going nudge. `-o`/`-O`/`-d`/`-T`/`--data*` write
// or POST, so they stay mutating.
func readOnlyCurl(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "--output" || a == "-O" || a == "--remote-name" ||
			a == "-d" || a == "--data" || a == "--data-raw" || a == "--data-binary" ||
			a == "--data-urlencode" || a == "-F" || a == "--form" || a == "-T" ||
			a == "--upload-file" || a == "-J" || a == "--remote-header-name":
			return false
		case strings.HasPrefix(a, "-o") && a != "-o" && !strings.HasPrefix(a, "--"):
			// curl allows -oFILE glued.
			return false
		case strings.HasPrefix(a, "--output="):
			return false
		case a == "-X" || a == "--request":
			if i+1 < len(args) {
				m := strings.ToUpper(args[i+1])
				if m != "GET" && m != "HEAD" && m != "OPTIONS" {
					return false
				}
				i++
			}
		case strings.HasPrefix(a, "-X") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			m := strings.ToUpper(a[2:])
			if m != "GET" && m != "HEAD" && m != "OPTIONS" {
				return false
			}
		}
	}
	return true
}

// readOnlyPython is true for the research shape `python3 -c '…'` / `python -c "…"`.
// A heredoc (`python3 - <<'PY' …`) or a script path is opaque and stays mutating:
// the keep-going gate cannot see whether the body writes files, and session
// 724e3fbb's probes used both forms — classifying heredocs as RO would hide a
// real `open(path,'w')`. `-c` is the one form whose whole program is in argv.
func readOnlyPython(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--command":
			return true
		case strings.HasPrefix(a, "-") && a != "-" && !strings.HasPrefix(a, "--"):
			// Short flag cluster: -u -c etc. Keep scanning.
			if strings.ContainsRune(a, 'c') {
				return true
			}
			continue
		case a == "-":
			// stdin / heredoc program — opaque.
			return false
		case strings.HasPrefix(a, "-"):
			continue
		default:
			// A script path is opaque.
			return false
		}
	}
	return false
}
