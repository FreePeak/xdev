package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// XDG roots (M14 #63, omp's `config init-xdg`): the three relocatable
// roots are opt-in. `xdev config init-xdg` creates them and records them
// at <install dir>/xdg; until that record exists every root stays at the
// legacy ~/.xdev/agent, so an upgrade never silently reads an empty
// directory. $XDEV_AGENT_DIR outranks the record (sandboxes stay
// self-contained), and re-running init-xdg is how a changed
// $XDG_*_HOME takes effect.

// xdgRecordName is the initialization record's file name.
const xdgRecordName = "xdg"

// XDGRoots are the three roots `xdev config init-xdg` records.
type XDGRoots struct {
	Data  string // settings, models.yml, sessions, skills, themes, memory
	State string // breadcrumbs, debug dumps
	Cache string // regenerable data only
}

// ResolveXDGRoots computes the roots from the XDG base directories
// ($XDG_DATA_HOME, $XDG_STATE_HOME, $XDG_CACHE_HOME; XDG spec defaults
// ~/.local/share, ~/.local/state, ~/.cache).
func ResolveXDGRoots() XDGRoots {
	return XDGRoots{
		Data:  filepath.Join(xdgBase("XDG_DATA_HOME", filepath.Join(nativeHome(), ".local", "share")), "xdev"),
		State: filepath.Join(xdgBase("XDG_STATE_HOME", filepath.Join(nativeHome(), ".local", "state")), "xdev"),
		Cache: filepath.Join(xdgBase("XDG_CACHE_HOME", filepath.Join(nativeHome(), ".cache")), "xdev"),
	}
}

// xdgBase is one XDG base directory; an unset or empty variable takes the
// spec default (relative values are also ignored, per the spec).
func xdgBase(env, def string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" && filepath.IsAbs(v) {
		return v
	}
	return def
}

// xdgRecordPath is the recorded-roots file.
func xdgRecordPath() string { return filepath.Join(installDir(), xdgRecordName) }

// XDGInitialized returns the recorded roots, or ok=false when
// `xdev config init-xdg` has not run for this install (or the record is
// unusable, in which case the legacy default applies).
func XDGInitialized() (XDGRoots, bool) {
	raw, err := os.ReadFile(xdgRecordPath())
	if err != nil {
		return XDGRoots{}, false
	}
	var r XDGRoots
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "data":
			r.Data = v
		case "state":
			r.State = v
		case "cache":
			r.Cache = v
		}
	}
	if r.Data == "" || r.State == "" || r.Cache == "" {
		return XDGRoots{}, false
	}
	return r, true
}

// InitXDG creates the three roots and records them. Existing files under
// the legacy base are not moved: the caller reports that rather than
// orphaning a user's sessions behind their back.
func InitXDG(r XDGRoots) error {
	for _, dir := range []string{r.Data, r.State, r.Cache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("init-xdg: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(xdgRecordPath()), 0o700); err != nil {
		return fmt.Errorf("init-xdg: %w", err)
	}
	body := fmt.Sprintf("# xdev XDG roots, written by `xdev config init-xdg`\ndata=%s\nstate=%s\ncache=%s\n",
		r.Data, r.State, r.Cache)
	if err := os.WriteFile(xdgRecordPath(), []byte(body), 0o600); err != nil {
		return fmt.Errorf("init-xdg: %w", err)
	}
	return nil
}

// InitXDGCommand implements `xdev config init-xdg [--data DIR] [--state
// DIR] [--cache DIR]` and returns a process exit code.
func InitXDGCommand(args []string, stdout, stderr io.Writer) int {
	r := ResolveXDGRoots()
	fs := flag.NewFlagSet("init-xdg", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&r.Data, "data", r.Data, "data root: settings, sessions, skills, themes")
	fs.StringVar(&r.State, "state", r.State, "state root: breadcrumbs, debug dumps")
	fs.StringVar(&r.Cache, "cache", r.Cache, "cache root: regenerable data only")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "xdev config init-xdg: unexpected argument %q (flags only: --data, --state, --cache)\n", fs.Arg(0))
		return 2
	}
	if err := InitXDG(r); err != nil {
		fmt.Fprintln(stderr, "xdev:", err)
		return 1
	}
	fmt.Fprintf(stdout, "xdev roots (record %s):\n  data  %s\n  state %s\n  cache %s\n",
		xdgRecordPath(), r.Data, r.State, r.Cache)
	if legacy := filepath.Join(nativeHome(), ".xdev", "agent"); legacy != r.Data {
		if _, err := os.Stat(legacy); err == nil {
			fmt.Fprintf(stdout, "note: %s was not moved; copy or symlink it into %s to keep existing sessions and settings\n", legacy, r.Data)
		}
	}
	if v := os.Getenv("XDEV_AGENT_DIR"); v != "" {
		fmt.Fprintf(stdout, "note: XDEV_AGENT_DIR=%s outranks the XDG roots, so this install keeps using %s\n", v, v)
	}
	fmt.Fprintf(stdout, "base precedence: --profile > XDEV_PROFILE > XDEV_AGENT_DIR > XDG > ~/.xdev/agent\n")
	return 0
}
