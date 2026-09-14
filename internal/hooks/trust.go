// Workspace trust for repository-supplied hooks (#241).
//
// A cloned repository can ship `.xdev/hooks/*.sh`, and the file name *is* the
// event it fires on: `agent_start.sh` runs on every session start, with the
// user's environment and credentials, before the first prompt. That is code
// execution granted by a stranger, and it was the one hole left after #114
// closed the configuration half — a repository may decide how xdev looks and
// how much a session may spend, and may not decide that it runs.
//
// So project hooks load only when the workspace has been explicitly trusted,
// and only while each hook still holds the bytes that were approved: the digest
// is per hook, so a repository that edits an approved script re-asks instead of
// inheriting the earlier yes. Trust is stored in the profile
// (<dataDir>/trusted-workspaces.yml), never in the repository, because a
// decision recorded inside the thing being constrained is not a constraint.
//
// Hooks from the CLI, the user's settings record, ~/.xdev/agent/hooks, trusted
// extensions and installed plugins are unaffected: each already has an owner
// who chose it.
package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/config"
)

// trustRecord is one reviewed workspace: the hook names that were approved and
// the digest of each at that moment.
type trustRecord struct {
	Root      string            `yaml:"root"`
	DecidedAt int64             `yaml:"decidedAt"`
	Hooks     map[string]string `yaml:"hooks"`
}

// trustFile is the shape of <dataDir>/trusted-workspaces.yml. A list rather
// than a map so a hand-edited file stays readable and a path with slashes
// needs no quoting.
type trustFile struct {
	Workspaces []trustRecord `yaml:"workspaces"`
}

// TrustPath is <dataDir>/trusted-workspaces.yml.
func TrustPath() string { return filepath.Join(config.DataDir(), "trusted-workspaces.yml") }

// WorkspaceRoot resolves cwd to the identity a trust decision is recorded
// against, so a symlinked checkout and its target share one decision rather
// than needing two approvals for the same files.
func WorkspaceRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return cwd
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// Digest identifies the exact bytes of one hook declaration. File-backed hooks
// hash the file; CLI and settings hooks, which have no file, hash their
// declaration so the record still describes what was approved.
func Digest(h Hook) string {
	if h.Path != "" {
		if raw, err := os.ReadFile(h.Path); err == nil {
			return digestOf(raw)
		}
	}
	return digestOf([]byte(h.Event + "\x00" + h.Command + "\x00" + h.Matcher + "\x00" + h.If))
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// loadTrust reads the record set. An absent file means nothing is trusted,
// which is the safe direction; a malformed one is an error rather than a silent
// "nothing approved", because a corrupt trust file could otherwise be used to
// quietly re-prompt a user into approving something new.
func loadTrust() (map[string]trustRecord, error) {
	raw, err := os.ReadFile(TrustPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]trustRecord{}, nil
		}
		return nil, err
	}
	var f trustFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("trust: %s is unreadable: %w", TrustPath(), err)
	}
	out := make(map[string]trustRecord, len(f.Workspaces))
	for _, w := range f.Workspaces {
		if w.Root == "" {
			continue
		}
		if w.Hooks == nil {
			w.Hooks = map[string]string{}
		}
		out[w.Root] = w
	}
	return out, nil
}

func saveTrust(records map[string]trustRecord) error {
	f := trustFile{Workspaces: make([]trustRecord, 0, len(records))}
	for _, r := range records {
		f.Workspaces = append(f.Workspaces, r)
	}
	// Deterministic order, so a re-write of unchanged content is a no-op diff.
	for i := range f.Workspaces {
		for j := i + 1; j < len(f.Workspaces); j++ {
			if f.Workspaces[j].Root < f.Workspaces[i].Root {
				f.Workspaces[i], f.Workspaces[j] = f.Workspaces[j], f.Workspaces[i]
			}
		}
	}
	raw, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(TrustPath()), 0o700); err != nil {
		return err
	}
	// 0600 and created private: this file is an authority list, and a temporary
	// window where others could read it is a window where they could learn which
	// repositories you have decided to trust.
	tmp := TrustPath() + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(raw); err != nil {
		fh.Close()
		os.Remove(tmp)
		return err
	}
	if err := fh.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, TrustPath())
}

// PendingProjectHooks returns the repository hooks that would not run right
// now: never approved, or changed since approval. Discovery is ungated, so
// this is also what `xdev trust --list` shows before anything is recorded.
func PendingProjectHooks(cwd string) ([]Hook, error) {
	disc, _ := Discover(cwd, nil)
	records, err := loadTrust()
	if err != nil {
		return nil, err
	}
	rec, trusted := records[WorkspaceRoot(cwd)]
	var out []Hook
	for _, h := range disc {
		if h.Source != "project" {
			continue
		}
		if trusted && rec.Hooks[h.Name] == Digest(h) {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// TrustWorkspace records the workspace's current project hooks as approved and
// returns what was recorded. An empty result is not an error: the caller says
// so, and writes nothing.
func TrustWorkspace(cwd string) ([]Hook, error) {
	disc, _ := Discover(cwd, nil)
	var project []Hook
	for _, h := range disc {
		if h.Source == "project" {
			project = append(project, h)
		}
	}
	if len(project) == 0 {
		return nil, nil
	}
	records, err := loadTrust()
	if err != nil {
		return nil, err
	}
	root := WorkspaceRoot(cwd)
	rec := records[root]
	if rec.Hooks == nil {
		rec.Hooks = map[string]string{}
	}
	rec.Root = root
	rec.DecidedAt = time.Now().Unix()
	for _, h := range project {
		rec.Hooks[h.Name] = Digest(h)
	}
	records[root] = rec
	return project, saveTrust(records)
}

// UntrustWorkspace drops the record for cwd, so its hooks are withheld again.
// It reports whether anything was recorded before.
func UntrustWorkspace(cwd string) (bool, error) {
	records, err := loadTrust()
	if err != nil {
		return false, err
	}
	root := WorkspaceRoot(cwd)
	if _, ok := records[root]; !ok {
		return false, nil
	}
	delete(records, root)
	return true, saveTrust(records)
}

// TrustedWorkspace is one recorded decision, in the shape a report wants:
// the workspace root, when it was decided, and which hook names were approved.
type TrustedWorkspace struct {
	Root      string
	DecidedAt int64
	Hooks     []string
}

// TrustedList returns the recorded workspaces, by root, for `xdev trust --list`.
func TrustedList() ([]TrustedWorkspace, error) {
	records, err := loadTrust()
	if err != nil {
		return nil, err
	}
	out := make([]TrustedWorkspace, 0, len(records))
	for _, r := range records {
		names := make([]string, 0, len(r.Hooks))
		for name := range r.Hooks {
			names = append(names, name)
		}
		slices.Sort(names)
		out = append(out, TrustedWorkspace{Root: r.Root, DecidedAt: r.DecidedAt, Hooks: names})
	}
	slices.SortFunc(out, func(a, b TrustedWorkspace) int { return strings.Compare(a.Root, b.Root) })
	return out, nil
}

// gateProjectHooks removes the repository hooks that are not approved and
// returns one warning describing what was withheld and how to allow it. The
// warning is deliberately single and count-bearing: buildHookBus logs it once
// per session, and a headless run must learn the fact without blocking on it.
func gateProjectHooks(hooks []Hook, cwd string, warns *[]string) []Hook {
	var project, kept []Hook
	for _, h := range hooks {
		if h.Source == "project" {
			project = append(project, h)
			continue
		}
		kept = append(kept, h)
	}
	if len(project) == 0 {
		return kept
	}
	records, err := loadTrust()
	if err != nil {
		*warns = append(*warns, fmt.Sprintf("%v; repository hooks are withheld until it is readable", err))
		return kept
	}
	rec, trusted := records[WorkspaceRoot(cwd)]
	var withheld []string
	for _, h := range project {
		if trusted && rec.Hooks[h.Name] == Digest(h) {
			kept = append(kept, h)
			continue
		}
		// The command is named because the whole point of the review is to
		// read what a stranger asked xdev to execute.
		withheld = append(withheld, h.Name+" → "+h.Event+" (`"+h.Command+"`)")
	}
	if len(withheld) == 0 {
		return kept
	}
	reason := "this workspace is not trusted"
	if trusted {
		reason = "they changed since they were approved"
	}
	*warns = append(*warns, fmt.Sprintf(
		"%d repository hook(s) under %s were NOT run because %s: %s — review with `xdev trust --list`, then run `xdev trust` in this directory to allow them",
		len(withheld), filepath.Join(cwd, ".xdev", "hooks"), reason, strings.Join(withheld, ", ")))
	return kept
}
