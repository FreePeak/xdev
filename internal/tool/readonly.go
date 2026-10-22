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
	// sleep does nothing, and expr evaluates an expression and prints it.
	// Neither can change anything.
	"sleep": true, "expr": true,
	// A shell TEST is a read: `[ "$n" != "0" ]` looks at values and returns
	// a status. A session that guards a loop with one is still only
	// READING, and a test classified as a write arms the keep-going gate
	// on a turn that changed nothing — the live 2026-10-22 case: "do n=$(gh
	// api …); if [ "$n" != "0" ]; then … fi" was read as a mutation.
	"[": true, "[[": true,
	// The shell's CONTROL WORDS name no program and change no file. `read`
	// consumes a line of stdin, `continue` and `break` move the loop, `exit`
	// ends the shell.
	"continue": true, "break": true, "read": true, "exit": true, "shift": true,
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
// `reflog` all have a destructive spelling. The ones that DO have a read-only
// form are judged by their arguments in readOnlyGitBranch / readOnlyGitWorktree
// below rather than being listed here whole-sale.
var readOnlyGitSubs = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"rev-parse": true, "rev-list": true, "describe": true, "blame": true,
	"shortlog": true, "ls-files": true, "ls-remote": true, "ls-tree": true,
	"show-ref": true, "cat-file": true, "name-rev": true,
	"for-each-ref": true, "count-objects": true, "grep": true,
	"annotate": true, "whatchanged": true, "diff-tree": true,
	"diff-index": true, "diff-files": true, "cherry": true,
	"merge-base": true, "verify-commit": true, "verify-tag": true,
	"check-ignore": true, "symbolic-ref": true, "merge-tree": true,
}

// gitRemoteReadSubs are the `git remote` subcommands that only read. The
// others — add, remove, rename, set-url, prune, update, set-head — write.
var gitRemoteReadSubs = map[string]bool{
	"":     true, // bare `git remote` lists
	"show": true, "get-url": true,
}

// gitStashReadSubs are the `git stash` subcommands that only read. The bare
// form (`git stash`) PUSHES the working tree, so it is not one of them.
var gitStashReadSubs = map[string]bool{
	"list": true, "show": true,
}

// readOnlyGitRemote reports whether a `git remote` invocation is a pure read.
// `git remote -v` is the form a session runs to see where the repo points.
func readOnlyGitRemote(args []string) bool {
	if len(args) == 0 {
		return true // bare `git remote` lists
	}
	sub := args[0]
	if strings.HasPrefix(sub, "-") {
		return true // -v, --verbose and friends only change the listing
	}
	return gitRemoteReadSubs[sub]
}

// readOnlyGitStash reports whether a `git stash` invocation is a pure read.
// `git stash list` is the form a session runs; the bare `git stash` stashes,
// which is a write.
func readOnlyGitStash(args []string) bool {
	if len(args) == 0 {
		return false // bare `git stash` pushes the working tree
	}
	sub := args[0]
	if strings.HasPrefix(sub, "-") {
		return false
	}
	return gitStashReadSubs[sub]
}

// readOnlyGitTag reports whether a `git tag` invocation is a pure read.
// `git tag` and `git tag -l <pattern>` list; `git tag -d` removes and a bare
// `git tag <name>` creates.
func readOnlyGitTag(args []string) bool {
	for _, a := range args {
		if a == "-d" || a == "--delete" {
			return false
		}
		if a == "-l" || a == "--list" {
			return true
		}
	}
	// A bare `git tag` lists. A bare `git tag <name>` creates.
	return len(args) == 0
}

// readOnlyGitConfig reports whether a `git config` invocation is a pure read.
// `git config --get <key>` and a bare `git config <key>` read one value;
// `git config <key> <value>` WRITES one, which is the shape the table in
// this file's tests pins as a write.
func readOnlyGitConfig(args []string) bool {
	positional := 0
	for _, a := range args {
		switch a {
		case "--get", "--get-all", "--get-regexp", "--list", "-l", "--get-color", "--get-colorbool":
			continue // read verbs
		case "--unset", "--unset-all", "--add", "--remove-section", "--rename-section", "--edit":
			return false // write verbs
		}
		if strings.HasPrefix(a, "-") {
			continue // a display flag
		}
		positional++
	}
	// One key is a read; a key and a value is a write.
	return positional <= 1
}

// gitBranchWriteShort are the SHORT flag letters of `git branch` that write.
// A cluster is judged letter by letter, so `-dv` (delete and list) is a write
// even though neither "dv" nor "d" alone would be a plausible flag.
var gitBranchWriteShort = "dDmMcCf"

// gitBranchWriteLong are the LONG `git branch` flags that write: the
// deletions, the renames, the copies, and the upstream setters.
var gitBranchWriteLong = map[string]bool{
	"--delete": true, "--move": true, "--copy": true,
	"--force": true, "--set-upstream-to": true, "--unset-upstream": true,
	"--edit-description": true,
}

// gitBranchQueryValueFlags are the read-only `git branch` flags that take a
// value. The value is not a branch name, so it must not be read as one:
// `git branch --contains HEAD` is a query, while `git branch HEAD~1` CREATES a
// branch called HEAD~1.
var gitBranchQueryValueFlags = map[string]bool{
	"--contains": true, "--no-contains": true,
	"--merged": true, "--no-merged": true,
	"--points-at": true,
	"--format":    true, "--sort": true,
	"--color": true, "--no-color": true,
}

// readOnlyGitBranch reports whether a `git branch` invocation is a pure read.
//
// `git branch` with no arguments lists branches, and the list forms only read.
// The destructive spellings (-d/--delete, -m/--move, -c/--copy, -f/--force)
// and the upstream setters are NOT reads. A bare branch NAME is a CREATE
// (`git branch feat/x`), not a list.
//
// The destructive set is a DENYLIST, and that is deliberate. An allowlist
// would have to name every display flag git grows — --no-abbrev, --column,
// --sort, --format, --contains — and the live test showed what that costs:
// the agent's own orientation command
// `git branch -a --verbose --no-abbrev` missed the allowlist, was read as a
// mutation, and the keep-going gate fired on a question that was already
// answered. A denylist names the flags that WRITE, and everything else is
// the read it looks like.
//
// The dangerous direction is still covered: the write flags are enumerated,
// a short cluster is judged letter by letter, and a bare name is a create.
func readOnlyGitBranch(args []string) bool {
	valueNext := false
	listForm := false
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			if valueNext {
				valueNext = false
				continue // this argument is the previous flag's value
			}
			// A bare name creates a branch — UNLESS --list is in play.
			// `git branch --list 'origin/*'` LISTS the branches matching a
			// pattern, and the pattern is not a name to create. Live
			// 2026-10-22: `git branch -r --list 'origin/*'` was read as a
			// CREATE, armed the keep-going gate on a turn that only read,
			// and pushed the model past the answer it had.
			if listForm {
				return true
			}
			return false // a bare name creates a branch
		}
		if valueNext {
			valueNext = false
			continue
		}
		if strings.HasPrefix(a, "--") {
			name := a
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			if gitBranchWriteLong[name] {
				return false
			}
			if name == "--list" {
				// The list form: every word after it is a PATTERN.
				listForm = true
			}
			if gitBranchQueryValueFlags[name] && !strings.Contains(a, "=") {
				valueNext = true
			}
			continue
		}
		// A short flag or a cluster of them: judge every letter.
		for _, c := range a[1:] {
			if strings.ContainsRune(gitBranchWriteShort, c) {
				return false
			}
		}
	}
	return true
}

// readOnlyGitWorktreeSubs is the one `git worktree` subcommand that only
// reads. Every other one writes: add creates a worktree, remove and prune
// delete one, move relocates one, and lock/unlock/repair touch its metadata.
var readOnlyGitWorktreeSubs = map[string]bool{"list": true}

// readOnlyGitWorktree reports whether a `git worktree` invocation is a pure
// read. `git worktree list` is the form a session actually runs while
// orienting itself, and it is the exact command that armed the keep-going
// gate in the field report this file was written for.
func readOnlyGitWorktree(args []string) bool {
	if len(args) == 0 {
		return false // bare `git worktree` is not a read
	}
	return readOnlyGitWorktreeSubs[args[0]]
}

// ghReadSubs are the `gh` subcommands that only READ, per noun.
//
// `gh` is the same shape as `git`: a small set of nouns, each with read forms
// and write forms that share the first two words. `gh pr list` and `gh pr
// merge` differ by one word, so the noun alone cannot decide — the subcommand
// does.
//
// A noun that is not in this table is not a read. gh grows nouns, and a noun
// this table has never heard of is more likely to be a write (`pr`, `issue`
// and `repo` are the write-heavy ones), so the unknown direction is the
// mutating one — the same conservative default an unrecognised command gets.
var ghReadSubs = map[string]map[string]bool{
	"pr":        {"list": true, "view": true, "status": true, "diff": true, "checks": true},
	"issue":     {"list": true, "view": true, "status": true},
	"repo":      {"list": true, "view": true},
	"search":    {"prs": true, "issues": true, "code": true, "commits": true, "repos": true},
	"run":       {"list": true, "view": true},
	"release":   {"list": true, "view": true, "download": true},
	"label":     {"list": true},
	"secret":    {"list": true},
	"gist":      {"list": true, "view": true},
	"workflow":  {"list": true, "view": true},
	"cache":     {"list": true},
	"alias":     {"list": true},
	"extension": {"list": true},
	"auth":      {"status": true},
	"config":    {"get": true},
}

// ghBareReadNouns are the `gh` nouns that read with no subcommand at all.
var ghBareReadNouns = map[string]bool{"status": true, "browse": true}

// readOnlyGh reports whether a `gh` invocation is a pure read.
//
// Live 2026-10-21 (session 4c4addab): the user asked "list me all pr urls
// openningin xdev" and the model answered it — the open PR list, with the
// URLs — and in the same millisecond the harness pushed it past its own
// answer, because `gh` was not in the read-only table at all and every
// `gh pr list` was therefore read as a mutation. The model then spent three
// minutes reading the diffs of two unrelated worktrees.
//
// `gh api` is the one noun whose verb is a flag, not a word: it defaults to
// GET and a method flag is the only thing that makes it write.
func readOnlyGh(args []string) bool {
	if len(args) == 0 {
		return false
	}
	// gh's global flags come before the noun and take no value.
	i := 0
	for ; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			break
		}
	}
	if i >= len(args) {
		return false
	}
	noun := args[i]
	rest := args[i+1:]
	if noun == "api" {
		return ghApiIsRead(rest)
	}
	if ghBareReadNouns[noun] {
		return true
	}
	subs, ok := ghReadSubs[noun]
	if !ok {
		return false
	}
	// The subcommand is the first non-flag word after the noun.
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return subs[a]
	}
	return false
}

// ghApiIsRead reports whether a `gh api` call is a read.
//
// `gh api` sends a GET unless a method flag says otherwise, and the flag has
// three spellings: `-X POST`, `--method POST` and `--method=POST`.
func ghApiIsRead(args []string) bool {
	method := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-X" || a == "--method":
			if i+1 < len(args) {
				method = strings.ToUpper(args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--method="):
			method = strings.ToUpper(strings.TrimPrefix(a, "--method="))
		case strings.HasPrefix(a, "-X") && len(a) > 2:
			method = strings.ToUpper(a[2:])
		}
	}
	switch method {
	case "", "GET", "HEAD", "OPTIONS":
		return true
	}
	return false
}

// cutAtRedirect returns the segment up to the first unquoted '<' or '>'.
//
// A redirection is not an argument to the command. `git branch --contains
// 01671eb 2>&1` passes --contains and 01671eb to git and 2>&1 to the shell,
// but the classifier saw "2>&1" as a third argument — and a bare argument
// after `git branch` is a branch name to CREATE, so the query became a
// write. Cutting at the redirect leaves the command's own arguments, which
// is what the caller wants.
//
// The cut takes the file-descriptor number with it: the operator is `[n]>`,
// so "2>&1" is one redirect, not an argument "2" followed by a redirect.
//
// The write check is separate and runs on the WHOLE segment
// (hasWriteRedirect), so nothing is lost by cutting here: a segment that
// redirects is still a write.
func cutAtRedirect(seg string) string {
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
		case c == '>' || c == '<':
			j := i
			for j > 0 && seg[j-1] >= '0' && seg[j-1] <= '9' {
				j-- // the file-descriptor number belongs to the operator
			}
			return seg[:j]
		}
	}
	return seg
}

// shellKeywordNeutral are the shell keywords that name no program AND are
// followed by nothing: a conditional's end, a loop's end, and a for loop's
// header.
//
//	"for x in a b"   the loop header: a variable and a word list
//	"fi" "done"     the ends
//
// The header of a for loop cannot write: the shell expands its word list,
// and a command substitution inside it is a segment of its own (the splitter
// cuts at `(`), so the command inside it is still judged.
var shellKeywordNeutral = map[string]bool{
	"for": true, "fi": true, "done": true, "end": true,
}

// shellKeywordCommand are the shell keywords whose segment CONTINUES with a
// command or an expression, so the rest of the segment is what has to be
// judged.
//
//	"do rm -rf /"     the body of a loop
//	"then git merge"  a branch of a conditional
//	"while COND"      a condition, which can name a command
//	"until COND"      the condition
//	"if COND"         the condition
//
// "then" and "else" belong here, not with the neutral keywords: in
// `if COND; then git worktree remove "$p"; else echo skipped; fi` the
// branch IS the command, and treating it as scaffolding hid a real
// `git worktree remove` from the classifier.
var shellKeywordCommand = map[string]bool{
	"do": true, "then": true, "else": true, "elif": true,
	"while": true, "until": true, "if": true,
}

// stripShellKeyword removes a leading shell keyword from a segment's fields
// and reports which kind it was. The keyword itself is not a command.
func stripShellKeyword(fields []string) ([]string, string) {
	if len(fields) == 0 {
		return fields, ""
	}
	k := fields[0]
	if shellKeywordNeutral[k] || shellKeywordCommand[k] {
		return fields[1:], k
	}
	return fields, ""
}

// readOnlyTimeout judges a `timeout` invocation.
//
//	timeout 5 gh pr list    the wrapped command is what runs
//	timeout 5               no command: a no-op
//
// The wrapped command is judged on its own, so `timeout 5 rm -rf x` is still
// a write. The wrapper is worth modelling: a research turn bounds a slow
// call with it constantly, and an unmodelled `timeout` made every bounded
// read look like a mutation.
func readOnlyTimeout(args []string) bool {
	rest := args
	// Flags first; -k/--kill-after and -s/--signal take a value.
	for len(rest) > 0 {
		a := rest[0]
		if !strings.HasPrefix(a, "-") {
			break // the duration
		}
		rest = rest[1:]
		switch a {
		case "-k", "--kill-after", "-s", "--signal":
			if len(rest) > 0 {
				rest = rest[1:]
			}
		}
	}
	if len(rest) == 0 {
		return true // `timeout` with no duration and no command
	}
	rest = rest[1:] // the duration
	if len(rest) == 0 {
		return true // `timeout 5` with no command does nothing
	}
	return readOnlySegment(strings.Join(rest, " "))
}

// readOnlyGofmt reports whether a `gofmt` invocation only prints.
//
//	gofmt file.go      print the formatted source
//	gofmt -l ./...     list the files that need formatting
//	gofmt -d file.go   print a diff
//	gofmt -w file.go   REWRITE the file
//
// -w is the whole of the write surface, so it is a denylist of one — the
// same shape as `sed -i`.
func readOnlyGofmt(args []string) bool {
	for _, a := range args {
		if a == "-w" || a == "--write" {
			return false
		}
	}
	return true
}

// readOnlySegment judges one already-split segment.
func readOnlySegment(seg string) bool {
	fields := strings.Fields(cutAtRedirect(seg))
	// Leading VAR=value assignments do not change the command: `FOO=1 ls` is
	// an ls.
	//
	// A segment that is ONLY assignments changes nothing either. The bash
	// tool gives every call a fresh process, so a variable set by a call
	// dies with that call — the same argument that puts `cd` in the read
	// set. A command substitution inside an assignment is a segment of its
	// own (the splitter cuts at `(`), so the command inside it is still
	// judged.
	//
	// It used to be a WRITE, on the reasoning that an assignment exports
	// state. But the splitter cuts `d=$(cmd)` in half, and the half it
	// leaves — "d=$" — is not a command at all: it read as an unknown one,
	// so the loop `for b in $(git for-each-ref …); do d=$(git rev-list …);
	// done` was a mutation on a turn that had only read.
	for len(fields) > 0 && isAssignment(fields[0]) {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return true
	}
	// A shell keyword is scaffolding, not a command. The body of a loop and
	// the condition of a while are commands, so they are judged as one.
	if rest, kw := stripShellKeyword(fields); kw != "" {
		if len(rest) == 0 {
			return true // the keyword alone names no program
		}
		if shellKeywordCommand[kw] {
			return readOnlySegment(strings.Join(rest, " "))
		}
		return true
	}
	switch head := fields[0]; head {
	case "git":
		return readOnlyGit(fields[1:])
	case "gh":
		return readOnlyGh(fields[1:])
	case "timeout":
		return readOnlyTimeout(fields[1:])
	case "gofmt":
		return readOnlyGofmt(fields[1:])
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
			return readOnlyGitSub(a, args[i+1:])
		}
	}
	return false
}

// readOnlyGitSub judges one git subcommand by name, dispatching the verbs
// whose read-only form has to be told apart from their mutating one.
func readOnlyGitSub(sub string, rest []string) bool {
	switch sub {
	case "branch":
		return readOnlyGitBranch(rest)
	case "worktree":
		return readOnlyGitWorktree(rest)
	case "remote":
		return readOnlyGitRemote(rest)
	case "stash":
		return readOnlyGitStash(rest)
	case "tag":
		return readOnlyGitTag(rest)
	case "config":
		return readOnlyGitConfig(rest)
	default:
		return readOnlyGitSubs[sub]
	}
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
				//
				// The scan CONTINUES: this operator is not the only one in
				// the segment, and the line may still write a file later —
				// "cmd 2>&1 > out" writes out. Returning here would report
				// that line as a read, which is the one direction that must
				// be impossible: a write read as a read costs a turn that
				// ends when it should not have.
				continue
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
