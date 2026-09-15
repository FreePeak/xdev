package stats

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/FreePeak/xdev/internal/session"
)

// rollup is the tiny stats cache (PRD non-goals: no `stats.db`-scale
// sidecar, "a tiny rollup table only"). One JSON file under
// <dataDir>/stats/rollup.json, keyed by session path, invalidated by the
// stat tuple a scan already has. Only files touched by the current scan are
// carried forward, so the table can never grow past the scan cap.
type rollup map[string]rollupEntry

type rollupEntry struct {
	Size int64    `json:"size"`
	Mod  int64    `json:"mod"` // unix nanoseconds
	C    counters `json:"c"`
}

// rollupVersion 2: counters gained `injected` (#283); v1 caches would
// under-report it as 0, so the whole cache is invalidated once.
const rollupVersion = 2

// rollupPath is <dataDir>/stats/rollup.json.
func rollupPath(dataDir string) string {
	return filepath.Join(dataDir, "stats", "rollup.json")
}

type rollupFile struct {
	Version int    `json:"version"`
	Entries rollup `json:"entries"`
}

// get returns the cached counters for m when the stat tuple still matches.
func (r rollup) get(m session.SessionMeta) (counters, bool) {
	e, ok := r[m.Path]
	if !ok || e.Size != m.SizeBytes || e.Mod != m.ModTime.UnixNano() {
		return counters{}, false
	}
	return e.C, true
}

func (r rollup) put(m session.SessionMeta, c counters) {
	r[m.Path] = rollupEntry{Size: m.SizeBytes, Mod: m.ModTime.UnixNano(), C: c}
}

// save writes the table atomically (temp + rename) so a concurrent scan
// never reads a half-written file. Errors are swallowed by the caller: the
// cache is an optimization, and losing it costs a slower next scan only.
func (r rollup) save(dataDir string) error {
	if dataDir == "" {
		return nil
	}
	dir := filepath.Join(dataDir, "stats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(rollupFile{Version: rollupVersion, Entries: r})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "rollup-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, rollupPath(dataDir))
}

// loadRollup reads the cache; a missing or unreadable/foreign-version file
// degrades to an empty table (a full rescan), never an error.
func loadRollup(dataDir string) rollup {
	if dataDir == "" {
		return rollup{}
	}
	b, err := os.ReadFile(rollupPath(dataDir))
	if err != nil {
		return rollup{}
	}
	var f rollupFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != rollupVersion {
		return rollup{}
	}
	if f.Entries == nil {
		return rollup{}
	}
	return f.Entries
}
