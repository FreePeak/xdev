package marketplace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// RegistryVersion is the on-disk shape of installed.json. It reads any
// version and always rewrites the current one; unknown fields of a newer file
// are dropped, which is why a bump must come with a migration here.
const RegistryVersion = 2

// Dirs are the resolved capability directories of one installed plugin. Only
// directories that exist at scan time are recorded.
type Dirs struct {
	Commands []string `json:"commands,omitempty"`
	Skills   []string `json:"skills,omitempty"`
	Agents   []string `json:"agents,omitempty"`
	Hooks    []string `json:"hooks,omitempty"`
}

// Installed is one registry entry.
type Installed struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Marketplace string `json:"marketplace,omitempty"`
	Source      string `json:"source"`
	// Revision is the git commit the install is pinned to, or a
	// sha256:<hex> content fingerprint for a plain local directory copy
	// (which carries no git revision).
	Revision    string    `json:"revision"`
	Path        string    `json:"path"`
	Dirs        Dirs      `json:"dirs"`
	InstalledAt time.Time `json:"installedAt"`
}

// Registry is installed.json: every plugin this xdev knows about, regardless
// of whether its tree is still on disk.
type Registry struct {
	Version int         `json:"version"`
	Plugins []Installed `json:"plugins"`
}

// dataDirFunc delegates to config.DataDir (XDEV_AGENT_DIR, profiles/XDG); a
// var so tests never touch the developer's real agent directory.
var dataDirFunc = config.DataDir

// SetDataDir overrides the data directory (tests).
func SetDataDir(dir string) { dataDirFunc = func() string { return dir } }

// DataDir is the active agent data directory.
func DataDir() string { return dataDirFunc() }

// Root is the installed-plugin root: <dataDir>/plugins. It is the same root
// internal/rules already scans for per-plugin rules/ directories.
func Root() string { return filepath.Join(DataDir(), "plugins") }

// registryPath is <dataDir>/plugins/installed.json.
func registryPath() string { return filepath.Join(Root(), "installed.json") }

// Load reads the registry. A missing file is an empty registry, never an
// error: nothing is installed on a fresh machine.
func Load() (*Registry, error) {
	raw, err := os.ReadFile(registryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{Version: RegistryVersion}, nil
		}
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", registryPath(), err)
	}
	return &r, nil
}

// save writes the registry atomically (temp file + rename) with 0600: the
// file lists local paths, and a half-written registry would lose installs.
func (r *Registry) save() error {
	if err := os.MkdirAll(Root(), 0o755); err != nil {
		return err
	}
	r.Version = RegistryVersion
	slices.SortFunc(r.Plugins, func(a, b Installed) int { return strings.Compare(a.Name, b.Name) })
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(Root(), "installed-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), registryPath())
}

// Find looks up an installed plugin by exact name.
func (r *Registry) Find(name string) (Installed, bool) {
	for _, p := range r.Plugins {
		if p.Name == name {
			return p, true
		}
	}
	return Installed{}, false
}

// Add inserts or replaces the entry for p.Name.
func (r *Registry) Add(p Installed) {
	for i := range r.Plugins {
		if r.Plugins[i].Name == p.Name {
			r.Plugins[i] = p
			return
		}
	}
	r.Plugins = append(r.Plugins, p)
}

// Remove drops the entry for name, reporting whether it was there.
func (r *Registry) Remove(name string) bool {
	for i := range r.Plugins {
		if r.Plugins[i].Name == name {
			r.Plugins = append(r.Plugins[:i], r.Plugins[i+1:]...)
			return true
		}
	}
	return false
}

// Sorted returns the entries in name order.
func (r *Registry) Sorted() []Installed {
	out := append([]Installed(nil), r.Plugins...)
	slices.SortFunc(out, func(a, b Installed) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// InstalledPlugins returns the registry in name order. A registry that cannot
// be read yields nothing rather than an error: discovery must never break a
// session over a corrupt file.
func InstalledPlugins() []Installed {
	r, err := Load()
	if err != nil {
		return nil
	}
	return r.Sorted()
}

// insideRootTree reports whether p lives under <dataDir>/plugins at any
// depth. The registry and the discovery accessors consult paths persisted
// earlier, and a hand-edited installed.json must not be able to point
// discovery at an arbitrary directory.
func insideRootTree(p string) bool {
	root := filepath.Clean(Root())
	clean := filepath.Clean(p)
	return strings.HasPrefix(clean, root+string(filepath.Separator))
}

// insideRoot validates that p is a direct child of <dataDir>/plugins (the
// plugin tree itself), the guard Remove deletes behind.
func insideRoot(p string) bool {
	root := filepath.Clean(Root())
	clean := filepath.Clean(p)
	if !insideRootTree(clean) {
		return false
	}
	return !strings.Contains(clean[len(root)+1:], string(filepath.Separator))
}
