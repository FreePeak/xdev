package marketplace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Options tunes a catalog load and an install.
type Options struct {
	// Marketplaces are catalog locations in precedence order (settings
	// plugins.marketplaces, then any -marketplace flags).
	Marketplaces []string
	// Force reinstalls over an existing copy.
	Force bool
}

// Catalog is one loaded marketplace.
type Catalog struct {
	Name     string
	Location string
	Root     string // local directory the manifest was read from
	Manifest *Manifest
}

// Entry is one available plugin: its catalog entry plus where it came from.
type Entry struct {
	Plugin
	Marketplace string
	Location    string
	Root        string
}

// Catalogs loads every marketplace location. A local path is read in place; a
// git location is cloned into <dataDir>/plugins/marketplaces/<name-hash> and
// fast-forwarded on later loads. An unreachable or malformed marketplace is a
// warning, never fatal: one broken catalog must not hide the rest.
func Catalogs(ctx context.Context, locations []string) ([]Catalog, []string) {
	var out []Catalog
	var warns []string
	seen := map[string]bool{}
	for _, loc := range locations {
		loc = strings.TrimSpace(loc)
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true
		root := loc
		if isGitSource(loc) {
			dir, err := syncCatalog(ctx, loc)
			if err != nil {
				warns = append(warns, fmt.Sprintf("marketplace %s: %v", loc, err))
				continue
			}
			root = dir
		}
		path := FindManifest(root)
		if path == "" {
			warns = append(warns, fmt.Sprintf("marketplace %s: no catalog manifest (looked for %s)",
				loc, strings.Join(ManifestFileNames, ", ")))
			continue
		}
		m, err := ParseManifest(path)
		if err != nil {
			warns = append(warns, fmt.Sprintf("marketplace %s: %v", loc, err))
			continue
		}
		out = append(out, Catalog{Name: m.Name, Location: loc, Root: root, Manifest: m})
	}
	return out, warns
}

// Available flattens every catalog's entries, in name order.
func Available(catalogs []Catalog) []Entry {
	var out []Entry
	for _, c := range catalogs {
		for _, p := range c.Manifest.Plugins {
			out = append(out, Entry{Plugin: p, Marketplace: c.Name, Location: c.Location, Root: c.Root})
		}
	}
	slices.SortFunc(out, func(a, b Entry) int {
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return strings.Compare(a.Marketplace, b.Marketplace)
	})
	return out
}

// Search filters the available entries by a case-insensitive substring over
// name, description and marketplace name.
func Search(catalogs []Catalog, query string) []Entry {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Available(catalogs)
	}
	var out []Entry
	for _, e := range Available(catalogs) {
		if strings.Contains(strings.ToLower(e.Name), q) ||
			strings.Contains(strings.ToLower(e.Description), q) ||
			strings.Contains(strings.ToLower(e.Marketplace), q) {
			out = append(out, e)
		}
	}
	return out
}

// Install resolves ref ("name" or "name@version") in the catalogs, stages the
// plugin into <dataDir>/plugins/<name>, verifies the plugin's own manifest
// against the catalog entry, and registers its capability roots.
//
// The tree is cloned/copied into a staging directory and renamed into place
// only after it verified, so a failed or mismatched install leaves nothing
// behind; the resolved revision is recorded, making the install pinnable.
// Nothing from the plugin is executed or imported.
func Install(ctx context.Context, ref string, opts Options) (*Installed, []string, error) {
	name, version := SplitRef(ref)
	if err := validateName(name); err != nil {
		return nil, nil, err
	}
	reg, err := Load()
	if err != nil {
		return nil, nil, err
	}
	target := filepath.Join(Root(), name)
	if prev, ok := reg.Find(name); ok && !opts.Force {
		return nil, nil, fmt.Errorf("plugin %q is already installed (version %s, revision %s) — use -force to reinstall",
			name, orNone(prev.Version), orDash(prev.Revision))
	}
	if _, err := os.Stat(target); err == nil && !opts.Force {
		return nil, nil, fmt.Errorf("plugin %q: %s already exists — use -force to replace it", name, target)
	}

	catalogs, warns := Catalogs(ctx, opts.Marketplaces)
	warn := func(format string, a ...any) {
		warns = append(warns, fmt.Sprintf(format, a...))
	}
	entry, ok := findEntry(catalogs, name, version)
	if !ok {
		if len(catalogs) == 0 {
			return nil, warns, fmt.Errorf("plugin %q not found: no marketplace could be loaded (set plugins.marketplaces or pass -marketplace)", name)
		}
		want := name
		if version != "" {
			want = name + "@" + version
		}
		names := make([]string, 0, len(catalogs))
		for _, c := range catalogs {
			names = append(names, fmt.Sprintf("%s (%s)", c.Name, c.Location))
		}
		return nil, warns, fmt.Errorf("plugin %q not found in %s", want, strings.Join(names, ", "))
	}

	if err := os.MkdirAll(Root(), 0o755); err != nil {
		return nil, warns, err
	}
	// Staging lives under the same parent as the target so the final rename
	// is atomic (no half-installed tree is ever visible at <name>).
	stageRoot := filepath.Join(Root(), ".staging")
	if err := os.MkdirAll(stageRoot, 0o755); err != nil {
		return nil, warns, err
	}
	stage, err := os.MkdirTemp(stageRoot, name+"-")
	if err != nil {
		return nil, warns, err
	}
	defer os.RemoveAll(stage)

	revision, err := materialize(ctx, entry, stage)
	if err != nil {
		return nil, warns, err
	}
	declared, verr := verifyTree(stage, entry.Plugin)
	if verr != nil {
		return nil, warns, verr
	}

	if opts.Force {
		if err := os.RemoveAll(target); err != nil {
			return nil, warns, err
		}
	}
	if err := os.Rename(stage, target); err != nil {
		return nil, warns, err
	}

	dirs := ResolveDirs(declared, target)
	if len(dirs.Commands)+len(dirs.Skills)+len(dirs.Agents)+len(dirs.Hooks) == 0 {
		warn("plugin %s: no commands/skills/agents/hooks directories found (a rules/ directory is picked up separately)", name)
	}

	inst := Installed{
		Name:        name,
		Version:     declared.Version,
		Description: declared.Description,
		Marketplace: entry.Marketplace,
		Source:      entry.Source,
		Revision:    revision,
		Path:        target,
		Dirs:        dirs,
		InstalledAt: time.Now().UTC(),
	}
	reg.Add(inst)
	if err := reg.save(); err != nil {
		return nil, warns, err
	}
	return &inst, warns, nil
}

// Remove deletes one installed plugin: its tree and its registry entry, which
// is what takes its roots back out of discovery. The recorded path is
// validated to be a direct child of <dataDir>/plugins before anything is
// deleted, so a hand-edited registry cannot aim RemoveAll at another
// directory.
func Remove(name string) (*Installed, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	reg, err := Load()
	if err != nil {
		return nil, err
	}
	inst, ok := reg.Find(name)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not installed", name)
	}
	if !insideRoot(inst.Path) {
		return nil, fmt.Errorf("plugin %q: recorded path %s is outside %s — refusing to delete",
			name, inst.Path, Root())
	}
	if err := os.RemoveAll(inst.Path); err != nil {
		return nil, err
	}
	reg.Remove(name)
	if err := reg.save(); err != nil {
		return nil, err
	}
	return &inst, nil
}

// findEntry picks the first catalog (in precedence order) that declares the
// name, and within it the entry matching the requested version.
func findEntry(catalogs []Catalog, name, version string) (Entry, bool) {
	for _, c := range catalogs {
		if p, ok := c.Manifest.Find(name, version); ok {
			return Entry{Plugin: p, Marketplace: c.Name, Location: c.Location, Root: c.Root}, true
		}
	}
	return Entry{}, false
}

// ResolveDirs resolves a plugin's capability directories against root and
// keeps only the ones that exist. A plugin that declares none uses the
// conventional layout (commands/, skills/, agents/, hooks/).
func ResolveDirs(p Plugin, root string) Dirs {
	return Dirs{
		Commands: capDirs(root, p.Commands, "commands"),
		Skills:   capDirs(root, p.Skills, "skills"),
		Agents:   capDirs(root, p.Agents, "agents"),
		Hooks:    capDirs(root, p.Hooks, "hooks"),
	}
}

func capDirs(root string, declared []string, convention string) []string {
	rels := declared
	if len(rels) == 0 {
		rels = []string{convention}
	}
	seen := map[string]bool{}
	var out []string
	for _, rel := range rels {
		abs := filepath.Join(root, filepath.Clean(strings.TrimSpace(rel)))
		if seen[abs] {
			continue
		}
		seen[abs] = true
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			out = append(out, abs)
		}
	}
	return out
}

// verifyTree cross-checks a staged tree against its catalog entry. A plugin
// that ships its own manifest must agree on the name, and on the version when
// both sides declare one — that is the "manifest mismatch refuses" gate. The
// returned Plugin is the inner manifest when present (the plugin's own
// declaration wins for the capability layout), falling back to the catalog
// entry field by field.
func verifyTree(dir string, entry Plugin) (Plugin, error) {
	path := FindPluginManifest(dir)
	if path == "" {
		return entry, nil
	}
	inner, err := ParsePluginManifest(path)
	if err != nil {
		return Plugin{}, err
	}
	if inner.Name != entry.Name {
		return Plugin{}, fmt.Errorf("manifest mismatch: %s declares name %q, the catalog says %q",
			path, inner.Name, entry.Name)
	}
	if inner.Version != "" && entry.Version != "" && inner.Version != entry.Version {
		return Plugin{}, fmt.Errorf("manifest mismatch: %s declares version %q, the catalog says %q",
			path, inner.Version, entry.Version)
	}
	merged := *inner
	if merged.Version == "" {
		merged.Version = entry.Version
	}
	if merged.Description == "" {
		merged.Description = entry.Description
	}
	if len(merged.Commands) == 0 {
		merged.Commands = entry.Commands
	}
	if len(merged.Skills) == 0 {
		merged.Skills = entry.Skills
	}
	if len(merged.Agents) == 0 {
		merged.Agents = entry.Agents
	}
	if len(merged.Hooks) == 0 {
		merged.Hooks = entry.Hooks
	}
	return merged, nil
}

// materialize puts the plugin source into dir and returns the revision the
// install is pinned to: the resolved git commit, or a content fingerprint for
// a plain local directory copy.
func materialize(ctx context.Context, e Entry, dir string) (string, error) {
	src, err := resolveSource(e.Source, e.Root)
	if err != nil {
		return "", fmt.Errorf("plugin %s: %w", e.Name, err)
	}
	if isGitSource(src) || hasGitDir(src) {
		if err := gitRun(ctx, "", "clone", "--quiet", src, dir); err != nil {
			return "", fmt.Errorf("plugin %s: %w", e.Name, err)
		}
		if rev := strings.TrimSpace(e.Revision); rev != "" {
			if err := gitRun(ctx, dir, "checkout", "--quiet", "--detach", rev); err != nil {
				return "", fmt.Errorf("plugin %s: revision %q: %w", e.Name, rev, err)
			}
		}
		sha, err := gitOut(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return "", fmt.Errorf("plugin %s: %w", e.Name, err)
		}
		return sha, nil
	}
	if err := copyTree(src, dir); err != nil {
		return "", fmt.Errorf("plugin %s: %w", e.Name, err)
	}
	return treeFingerprint(dir)
}

// resolveSource turns a catalog entry's source into a git location or a local
// path. A relative path resolves against the marketplace root and must stay
// inside it: a catalog fetched from a git URL is untrusted input, and
// "../../.." would otherwise copy an arbitrary directory into the data dir.
// Absolute paths and git locations are the author's explicit choice and are
// taken as-is.
func resolveSource(source, root string) (string, error) {
	src := strings.TrimSpace(source)
	if src == "" {
		return "", fmt.Errorf("source is empty")
	}
	if isGitSource(src) || filepath.IsAbs(src) {
		return src, nil
	}
	cleanRoot := filepath.Clean(root)
	abs := filepath.Clean(filepath.Join(root, src))
	if abs != cleanRoot && !strings.HasPrefix(abs, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("source %q escapes the marketplace root %s", src, cleanRoot)
	}
	return abs, nil
}

// syncCatalog clones a git marketplace into the cache or fast-forwards an
// existing clone. A failed fast-forward keeps the cached checkout: an offline
// machine still lists what it last saw.
func syncCatalog(ctx context.Context, loc string) (string, error) {
	dir := filepath.Join(Root(), "marketplaces", catalogDirName(loc))
	if hasGitDir(dir) {
		_ = gitRun(ctx, dir, "pull", "--ff-only", "--quiet")
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := gitRun(ctx, "", "clone", "--quiet", loc, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// catalogDirName is the cache directory for one git marketplace: a readable
// stem plus a digest of the full location, so two marketplaces that share a
// base name never collide.
func catalogDirName(loc string) string {
	stem := strings.TrimSuffix(filepath.Base(strings.TrimSuffix(loc, "/")), ".git")
	stem = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, stem)
	if stem == "" || stem == "." {
		stem = "catalog"
	}
	sum := sha256.Sum256([]byte(loc))
	return stem + "-" + hex.EncodeToString(sum[:4])
}

func isGitSource(s string) bool {
	if strings.Contains(s, "://") {
		return true // https://, ssh://, git://, file://
	}
	if strings.HasPrefix(s, "git@") {
		return true // scp-like
	}
	return strings.HasSuffix(s, ".git")
}

func hasGitDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// copyTree copies a plain (non-git) local source. Symlinks are skipped: an
// installed plugin is a self-contained tree, and a link could point anywhere.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755 // hook scripts must stay executable
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, mode)
	})
}

// treeFingerprint is a deterministic digest of a copied tree, recorded as the
// install revision when the source carries no git revision of its own.
func treeFingerprint(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

// gitRun shells out to git; it is the only external command this package
// runs, and it never runs plugin content.
func gitRun(ctx context.Context, dir string, args ...string) error {
	_, err := gitOutput(ctx, dir, args...)
	return err
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitOutput(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// No credential prompt may block a non-interactive install.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
