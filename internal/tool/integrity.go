package tool

// The integrity floor for harness state (#192).
//
// Claude Code refuses its model's writes into its own orchestration state —
// `**/.claude/mailbox/`, `agent-registry.json`, `worktrees/`, `checkpoints/` —
// on the reasoning that state the harness owns through an API is not the
// model's to hand-edit. xdev had no such rule: `write`/`edit` happily wrote the
// session JSONL, the blob store, or the user's config.
//
// That is not tidiness. The model's own history, the approval policy and the
// credential store all live under the data dir, so writing them is the
// escalation the rest of this repo's security work assumes away: a session that
// can edit `~/.xdev/agent/config.yml` can set `approvalMode: yolo` and undo
// #114 from the inside, and one that can edit its own JSONL can rewrite what the
// user is shown as their own prior turn. In the checkout the same holds for
// `<repo>/.xdev/agents/*.md`, which is spawn-policy input.
//
// So this is a fixed rule list, deliberately *not* configurable: no setting,
// approval mode or policy hook can loosen it, because a floor the guarded party
// can lift is approval theater. Harness writers (the session store, hub,
// checkpoints, the config CLI) do not go through the file tools, so they are
// unaffected — the floor is on the model-facing seam.
//
// Every comparison is made on symlink-resolved paths. That is not pedantry: on
// macOS `/tmp` is `/private/tmp`, so a guard comparing the shapes it was handed
// would report "protected" while refusing nothing at all — the exact silent
// pass that makes a security test worth writing.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ProtectedStateReason is the stable phrase every denial carries. It is one
// constant rather than prose to grep for because the tests and any caller that
// has to recognise the floor need an exact handle, and a refusal the model can
// pattern-match has to stay distinguishable from an ordinary tool error.
const ProtectedStateReason = "harness state"

// repoHarnessDir is the directory a repository keeps xdev's own state in:
// config.yml, agents/, commands/, skills/, hooks/, models.yml.
const repoHarnessDir = ".xdev"

var (
	protectedMu   sync.RWMutex
	protectedRoot = func() []string { return nil }
)

// SetProtectedRoots registers the function answering "where does the harness
// keep its state right now" — the data/state/cache roots, which move with the
// active profile, XDG initialization and the test environment.
//
// A function, not a slice: a snapshot would quietly stop protecting the
// directory actually in use the moment a profile is switched, and a guard that
// protects nothing while looking installed is the fail-open shape this repo has
// been bitten by repeatedly. internal/config installs it from an init (config →
// tool is the only legal import direction), so no entry point can forget it.
func SetProtectedRoots(f func() []string) {
	protectedMu.Lock()
	defer protectedMu.Unlock()
	if f == nil {
		protectedRoot = func() []string { return nil }
		return
	}
	protectedRoot = f
}

// currentProtectedRoots reads the registered roots under the read lock, so a
// concurrent profile switch cannot tear the slice a check is walking.
func currentProtectedRoots() []string {
	protectedMu.RLock()
	defer protectedMu.RUnlock()
	return protectedRoot()
}

// ProtectedRoots reports what the floor covers right now, as configured.
// Exported for diagnostics and for tests that need to assert the wiring
// installed something at all.
func ProtectedRoots() []string {
	out := make([]string, 0, 4)
	for _, r := range currentProtectedRoots() {
		if r != "" {
			out = append(out, r)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, repoHarnessDir))
	}
	return out
}

// CheckProtectedPath returns an error when resolved is harness state. written
// is the path as the caller named it (absolute, links unresolved) and only
// feeds the message: a denial must say when the path was reached through a
// link, which resolved alone cannot show. resolved is what the tool already
// cleaned and absolutized; the check follows symlinks itself, so
// `notes.json -> ~/.xdev/agent/credentials.json` is not a way through the floor.
func CheckProtectedPath(written, resolved string) error {
	forms := pathForms(resolved)
	for _, root := range currentProtectedRoots() {
		if root == "" {
			continue
		}
		if hit := containedBy(root, forms); hit != "" {
			return protectedError(written, hit, absPath(root))
		}
	}
	// The repository's own .xdev, resolved through any link above it: a checkout
	// reached by a symlinked path is the same checkout.
	if wd, err := os.Getwd(); err == nil {
		repo := filepath.Join(wd, repoHarnessDir)
		if hit := containedBy(repo, forms); hit != "" {
			return protectedError(written, hit, repo)
		}
	}
	return nil
}

// pathForms is the path as written, its symlink-resolved form, and — for a file
// that does not exist yet — the resolved nearest existing ancestor re-joined,
// because the link can sit above the target directory.
func pathForms(resolved string) []string {
	out := []string{filepath.Clean(resolved)}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		return append(out, absPath(real))
	}
	dir := filepath.Dir(resolved)
	for i := 0; i < 8; i++ {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if rest, relErr := filepath.Rel(dir, resolved); relErr == nil {
				out = append(out, absPath(filepath.Join(real, rest)))
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

// containedBy reports which form of `forms` lies at or below root (either of
// root's own forms), or "" when none does.
func containedBy(root string, forms []string) string {
	rootForms := []string{absPath(root)}
	if real, err := filepath.EvalSymlinks(absPath(root)); err == nil {
		rootForms = append(rootForms, real)
	}
	for _, f := range forms {
		for _, r := range rootForms {
			if f == r || under(f, r) != "" {
				return f
			}
		}
	}
	return ""
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// under returns path's position relative to root, or "" when it is not below
// it. The separator-joined prefix is the point: `/home/u/.xdev-agent` must not
// read as contained by `/home/u/.xdev`, and neither must `/home/u/.xdevx/…`.
func under(path, root string) string {
	p, r := filepath.Clean(path), filepath.Clean(root)
	if !strings.HasPrefix(p, r+string(filepath.Separator)) {
		return ""
	}
	rel, err := filepath.Rel(r, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return rel
}

// protectedError is the message the tests and the model both read: the stable
// token, what kind of state it is, and the way to change it legitimately. The
// route matters as much as the refusal — a bare "no" sends the model to bash,
// where this floor does not reach.
func protectedError(written, real, root string) error {
	via := ""
	if filepath.Clean(written) != real {
		via = fmt.Sprintf(", reached through a symlink at %s", real)
	}
	rel := under(real, absPath(root))
	what, how := "harness state", "It is written by xdev itself; there is no hand-edit path, and none is needed."
	switch {
	case filepath.Base(root) == repoHarnessDir:
		what = "this repository's .xdev config (config.yml, agents/, commands/, skills/, hooks/)"
		how = "Ask the user to change it. A repository-supplied config.yml is refused at load anyway (#114), and a hook file additionally needs `xdev trust` (#241)."
	case strings.HasSuffix(real, string(filepath.Separator)+"config.yml"),
		strings.HasSuffix(real, string(filepath.Separator)+"models.yml"),
		strings.HasSuffix(real, string(filepath.Separator)+"credentials.json"),
		strings.HasSuffix(real, string(filepath.Separator)+"keybindings.yml"),
		strings.HasSuffix(real, string(filepath.Separator)+"trusted-workspaces.yml"):
		what = "the user's xdev configuration"
		how = "Ask the user to edit it, or run `xdev config set <key> <value>` — that is the supported path and it is checked."
	case rel == "sessions" || strings.HasPrefix(rel, "sessions"+string(filepath.Separator)):
		what = "stored session history"
		how = "The session API appends to it; nothing edits it in place, and a transcript the model could rewrite is not evidence."
	case rel == "blobs" || strings.HasPrefix(rel, "blobs"+string(filepath.Separator)):
		what = "the content-addressed blob store"
		how = "Blob files are written by the tool layer when it truncates large output; hand-writing one breaks the sha-addressing that lets a re-read find it."
	}
	return fmt.Errorf("%s is %s under %s%s — %s that xdev owns, so the file tools will not write it. %s",
		written, what, root, via, ProtectedStateReason, how)
}
