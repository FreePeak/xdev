package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Native base-dir relocation (M14 #63): a named profile and the optional
// XDG record move where xdev keeps its user-level files. Precedence for
// the per-run base directory, highest first:
//
//	--profile <name>   nest the base under <base>/profiles/<name>
//	XDEV_PROFILE       ditto, when the --profile flag is absent
//	XDEV_AGENT_DIR     explicit base dir; skips the XDG record and the legacy default
//	XDG roots          once `xdev config init-xdg` has recorded them
//	~/.xdev/agent      legacy default
//
// The two profile knobs only select the profile name; they nest under
// whichever root the three lower knobs resolve, so a profiled run shares
// no sessions, settings, rules, themes, skills, or memory with another
// profile — or with the default base.

// namedProfile is the process-wide profile resolved by SetProfile.
var namedProfile string

// SetProfile selects this process's profile: the --profile flag wins, then
// $XDEV_PROFILE. An empty, whitespace-only, or "default" name selects the
// default base (omp's OMP_PROFILE semantics). The name becomes a path
// element, so it is validated here once rather than at every path
// construction; an invalid name is an error and leaves the profile unset.
func SetProfile(name string) error {
	if strings.TrimSpace(name) == "" {
		name = os.Getenv("XDEV_PROFILE")
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "default" {
		namedProfile = ""
		return nil
	}
	if !validName(name) {
		return fmt.Errorf("invalid profile name %q: use letters, digits, '.', '_' or '-' (max 64)", name)
	}
	namedProfile = name
	return nil
}

// ActiveProfile returns the effective profile name ("" = default base).
func ActiveProfile() string { return namedProfile }

// validName reports whether name is safe as a single path element.
func validName(name string) bool {
	if name == "" || len(name) > 64 || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// rootKind selects one of the three relocatable roots.
type rootKind int

const (
	rootData rootKind = iota
	rootState
	rootCache
)

// DataDir returns the directory holding persistent user data (settings,
// models.yml, sessions, skills, themes, memory).
func DataDir() string { return nativeDir(rootData) }

// StateDir returns the directory for state worth keeping but not worth
// treating as user data (breadcrumbs, debug dumps). Until XDG is
// initialized it follows DataDir, so an existing install keeps one
// directory.
func StateDir() string { return nativeDir(rootState) }

// CacheDir returns the directory for regenerable data only: nothing a user
// would miss if it vanished. Until XDG is initialized it follows DataDir.
func CacheDir() string { return nativeDir(rootCache) }

// nativeDir resolves one root, nesting the active profile under it.
func nativeDir(kind rootKind) string {
	base := rootDir(kind)
	if p := ActiveProfile(); p != "" {
		return filepath.Join(base, "profiles", p)
	}
	return base
}

// rootDir resolves the unprefixed base for one root kind.
func rootDir(kind rootKind) string {
	if v := os.Getenv("XDEV_AGENT_DIR"); v != "" {
		// Sandbox/test override: every root collapses into one directory,
		// exactly as before profiles and XDG existed.
		return v
	}
	if r, ok := XDGInitialized(); ok {
		switch kind {
		case rootState:
			return r.State
		case rootCache:
			return r.Cache
		default:
			return r.Data
		}
	}
	return filepath.Join(nativeHome(), ".xdev", "agent")
}

// nativeHome is the user's home directory (empty when it cannot be
// resolved, so callers degrade to a relative path instead of failing).
func nativeHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// installDir is the per-install root shared by every profile: ~/.xdev by
// default (the parent of the legacy agent dir), or $XDEV_AGENT_DIR for a
// sandbox. install-id and the XDG record live here, deliberately outside
// the profile nesting — install identity is per install, not per profile.
func installDir() string {
	if v := os.Getenv("XDEV_AGENT_DIR"); v != "" {
		return v
	}
	return filepath.Join(nativeHome(), ".xdev")
}
