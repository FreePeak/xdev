package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// runGC implements `xdev gc` (issue #34): report what the local store could
// reclaim and, with --yes, delete it. The command only ever touches four
// managed roots under the data dir — blobs, artifacts, subagent sessions and
// dumps — never the session store itself, and never anything a live session
// references.
func runGC(args []string) int {
	return gcCmd(args, config.DataDir(), os.Stdout, os.Stderr)
}

// gcKind names one managed root.
type gcKind string

const (
	gcBlob     gcKind = "blob"
	gcArtifact gcKind = "artifact"
	gcSubagent gcKind = "subagent"
	gcDump     gcKind = "dump"
)

// gcKinds is the full sweep vocabulary, in report order.
var gcKinds = []gcKind{gcBlob, gcArtifact, gcSubagent, gcDump}

// gcStats is one kind's line in the report.
type gcStats struct {
	Kind       string `json:"kind"`
	Items      int    `json:"items"`
	Bytes      int64  `json:"bytes"`
	Orphans    int    `json:"orphans"`
	OrphanSize int64  `json:"orphanBytes"`
	// live is the count kept because a session still owns the item.
	Live    int   `json:"live"`
	Removed int   `json:"removed"`
	Freed   int64 `json:"freedBytes"`
}

// gcReport is the whole plan: what exists, what would go, and (after --yes)
// what actually went.
type gcReport struct {
	DataDir   string    `json:"dataDir"`
	OlderThan string    `json:"olderThan"`
	Applied   bool      `json:"applied"`
	Stats     []gcStats `json:"stats"`
	// Reclaimable is the orphan size in the plan; Freed is what the sweep
	// actually removed (they differ when a file vanished mid-run).
	Reclaimable int64 `json:"reclaimableBytes"`
	Freed       int64 `json:"freedBytes"`
}

// gcCandidate is one deletion the plan proposes.
type gcCandidate struct {
	kind  gcKind
	path  string
	bytes int64
	// why records the reason for the audit line ("unreferenced blob",
	// "artifact of a deleted session", "subagent session older than ...").
	why string
}

// gcPlan carries the candidates alongside the report.
type gcPlan struct {
	report     gcReport
	candidates []gcCandidate
	kept       map[gcKind]string // one human line per kind explaining the keeps
}

func gcCmd(args []string, dataDir string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(errOut)
	yes := fs.Bool("yes", false, "actually delete (without it, gc only reports)")
	olderThan := fs.Duration("older-than", 720*time.Hour, "retention window: only items untouched for longer are collected")
	kinds := fs.String("kind", "", "comma-separated subset of blob,artifact,subagent,dump (default: all)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Usage = func() {
		fmt.Fprint(errOut, `usage: xdev gc [flags]

  xdev gc                      report reclaimable space (deletes nothing)
  xdev gc --yes                delete orphaned blobs, artifacts, old subagent
                               sessions and dumps
  xdev gc --older-than 168h    tighten the retention window

Session files themselves are never touched, and neither is anything a live
session references: the sweep only covers <data dir>/{blobs,artifacts,
subagents,dumps}.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	selected, err := gcSelectKinds(*kinds)
	if err != nil {
		fmt.Fprintln(errOut, "xdev gc:", err)
		return 2
	}
	if *olderThan < 0 {
		fmt.Fprintln(errOut, "xdev gc: --older-than must not be negative")
		return 2
	}

	plan, err := gcScan(dataDir, gcOptions{now: time.Now(), olderThan: *olderThan, kinds: selected})
	if err != nil {
		fmt.Fprintln(errOut, "xdev gc:", err)
		return 1
	}
	if *yes {
		if err := gcSweep(dataDir, plan); err != nil {
			fmt.Fprintln(errOut, "xdev gc:", err)
			return 1
		}
	}
	if *asJSON {
		b, _ := json.MarshalIndent(plan.report, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	fmt.Fprint(out, gcRender(dataDir, plan, *yes))
	return 0
}

func gcSelectKinds(raw string) (map[gcKind]bool, error) {
	if strings.TrimSpace(raw) == "" {
		out := make(map[gcKind]bool, len(gcKinds))
		for _, k := range gcKinds {
			out[k] = true
		}
		return out, nil
	}
	out := map[gcKind]bool{}
	valid := map[string]gcKind{}
	for _, k := range gcKinds {
		valid[string(k)] = k
	}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		k, ok := valid[name]
		if !ok {
			return nil, fmt.Errorf("unknown --kind %q (want blob,artifact,subagent,dump)", part)
		}
		out[k] = true
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--kind selected nothing")
	}
	return out, nil
}

type gcOptions struct {
	now       time.Time
	olderThan time.Duration
	kinds     map[gcKind]bool
}

func (o gcOptions) cutoff() time.Time { return o.now.Add(-o.olderThan) }

// gcScan builds the plan without deleting anything.
func gcScan(dataDir string, opts gcOptions) (*gcPlan, error) {
	live, err := gcLiveSessionIDs(dataDir)
	if err != nil {
		return nil, err
	}
	plan := &gcPlan{
		report: gcReport{DataDir: dataDir, OlderThan: opts.olderThan.String()},
		kept:   map[gcKind]string{},
	}
	stats := map[gcKind]*gcStats{}
	for _, k := range gcKinds {
		if !opts.kinds[k] {
			continue
		}
		stats[k] = &gcStats{Kind: string(k)}
		plan.report.Stats = append(plan.report.Stats, gcStats{Kind: string(k)})
	}
	if opts.kinds[gcBlob] {
		if err := gcScanBlobs(dataDir, opts, stats[gcBlob], plan); err != nil {
			return nil, err
		}
	}
	if opts.kinds[gcArtifact] {
		if err := gcScanArtifacts(dataDir, opts, live, stats[gcArtifact], plan); err != nil {
			return nil, err
		}
	}
	if opts.kinds[gcSubagent] {
		if err := gcScanTree(dataDir, "subagents", gcSubagent, opts, stats[gcSubagent], plan); err != nil {
			return nil, err
		}
	}
	if opts.kinds[gcDump] {
		if err := gcScanTree(dataDir, "dumps", gcDump, opts, stats[gcDump], plan); err != nil {
			return nil, err
		}
	}
	plan.report.Stats = plan.report.Stats[:0]
	for _, k := range gcKinds {
		if st, ok := stats[k]; ok {
			plan.report.Stats = append(plan.report.Stats, *st)
			plan.report.Reclaimable += st.OrphanSize
		}
	}
	return plan, nil
}

// gcScanBlobs collects blobs no session file references. The reference scan is
// text over the session store (a blob ref is a literal in the message JSON),
// which is also how the store's own readers find them.
func gcScanBlobs(dataDir string, opts gcOptions, st *gcStats, plan *gcPlan) error {
	refs, err := gcBlobRefs(dataDir)
	if err != nil {
		return err
	}
	store := session.NewBlobStore(dataDir)
	blobs, err := store.List()
	if err != nil {
		return err
	}
	referenced := 0
	for _, b := range blobs {
		st.Items++
		st.Bytes += b.Size
		if refs[b.Digest] {
			referenced++
			continue
		}
		// Unreferenced is not yet orphaned: a blob written moments ago may
		// belong to a session that has not flushed its message yet.
		if b.ModTime.After(opts.cutoff()) {
			continue
		}
		st.Orphans++
		st.OrphanSize += b.Size
		plan.candidates = append(plan.candidates, gcCandidate{kind: gcBlob, path: filepath.Join(store.Root(), b.Digest), bytes: b.Size, why: "unreferenced blob"})
	}
	plan.kept[gcBlob] = fmt.Sprintf("%d of %d blobs referenced by a session", referenced, len(blobs))
	return nil
}

// gcScanArtifacts collects artifact directories whose session is gone.
func gcScanArtifacts(dataDir string, opts gcOptions, live map[string]bool, st *gcStats, plan *gcPlan) error {
	root := filepath.Join(dataDir, "artifacts")
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			plan.kept[gcArtifact] = "nothing stored"
			return nil
		}
		return err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		size := gcDirSize(filepath.Join(dataDir, "artifacts", e.Name()))
		st.Items++
		st.Bytes += size
		if live[e.Name()] {
			st.Live++
			continue
		}
		if info.ModTime().After(opts.cutoff()) {
			continue
		}
		st.Orphans++
		st.OrphanSize += size
		plan.candidates = append(plan.candidates, gcCandidate{
			kind: gcArtifact, path: filepath.Join(dataDir, "artifacts", e.Name()), bytes: size,
			why: "artifact of a session that no longer exists",
		})
	}
	plan.kept[gcArtifact] = fmt.Sprintf("%d artifacts belong to live sessions", st.Live)
	return nil
}

// gcScanTree collects whole files under one managed subdirectory (subagents,
// dumps) that are older than the retention window. Subagent sessions are
// children no user continuation can reach, so age alone decides.
func gcScanTree(dataDir, sub string, kind gcKind, opts gcOptions, st *gcStats, plan *gcPlan) error {
	root := filepath.Join(dataDir, sub)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a vanished subtree is not a gc failure
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		st.Items++
		st.Bytes += info.Size()
		if info.ModTime().After(opts.cutoff()) {
			return nil
		}
		st.Orphans++
		st.OrphanSize += info.Size()
		why := fmt.Sprintf("%s untouched for more than %s", kind, opts.olderThan)
		plan.candidates = append(plan.candidates, gcCandidate{kind: kind, path: path, bytes: info.Size(), why: why})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if st.Items == 0 {
		plan.kept[kind] = "nothing stored"
	} else {
		plan.kept[kind] = fmt.Sprintf("%d of %d files are inside the %s window", st.Items-st.Orphans, st.Items, opts.olderThan)
	}
	return nil
}

var gcBlobRefRE = regexp.MustCompile(session.BlobRefPrefix + `[0-9a-f]{64}`)

// gcBlobRefs collects every blob digest any session file (or exported dump)
// mentions. Files are read whole but capped: a 64 MiB session that mentions a
// blob must not be the reason gc needs a gigabyte of RSS, and a truncated read
// can only ever keep a blob, never delete one.
func gcBlobRefs(dataDir string) (map[string]bool, error) {
	const maxFileBytes = 64 << 20
	refs := map[string]bool{}
	roots := []string{filepath.Join(dataDir, "sessions"), filepath.Join(dataDir, "subagents"), filepath.Join(dataDir, "dumps"), filepath.Join(dataDir, "handoffs")}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if ext := filepath.Ext(path); ext != ".jsonl" && ext != ".md" {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil || info.Size() > maxFileBytes {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, ref := range gcBlobRefRE.FindAll(data, -1) {
				if hexSum, perr := session.ParseBlobRef(string(ref)); perr == nil {
					refs[hexSum] = true
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return refs, nil
}

// gcLiveSessionIDs is the set of 8-char ids the store still owns, taken from
// session.List (parsed headers) and from the canonical file names (so a
// session whose header failed to parse still protects its artifacts).
func gcLiveSessionIDs(dataDir string) (map[string]bool, error) {
	live := map[string]bool{}
	metas, err := session.List(dataDir)
	if err != nil {
		return nil, err
	}
	for _, m := range metas {
		if len(m.ID) >= 8 {
			live[m.ID[:8]] = true
		}
	}
	for _, root := range []string{filepath.Join(dataDir, "sessions"), filepath.Join(dataDir, "subagents")} {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if id := gcSessionIDFromName(filepath.Base(path)); len(id) >= 8 {
				live[id[:8]] = true
			}
			return nil
		})
	}
	return live, nil
}

// gcSessionIDFromName extracts the id from "<timestamp>_<id>.jsonl" (and the
// "<id>.md" dump form); anything that does not match returns "".
func gcSessionIDFromName(name string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".jsonl"), ".md")
	if i := strings.LastIndexByte(base, '_'); i >= 0 {
		return base[i+1:]
	}
	return base
}

// gcSweep deletes the plan's candidates, guarding every path against leaving
// the managed roots (a bug in a scanner must not turn into `rm -rf $HOME`).
func gcSweep(dataDir string, plan *gcPlan) error {
	byKind := map[gcKind]*gcStats{}
	for i := range plan.report.Stats {
		var k gcKind
		for _, candidate := range gcKinds {
			if string(candidate) == plan.report.Stats[i].Kind {
				k = candidate
			}
		}
		byKind[k] = &plan.report.Stats[i]
	}
	for _, c := range plan.candidates {
		if !gcManagedPath(dataDir, c.kind, c.path) {
			return fmt.Errorf("refusing to delete %s: outside the managed roots", c.path)
		}
		info, err := os.Lstat(c.path)
		if err != nil {
			continue // already gone: the plan is not a promise
		}
		var derr error
		if info.IsDir() {
			derr = os.RemoveAll(c.path)
		} else {
			derr = os.Remove(c.path)
		}
		if derr != nil {
			return fmt.Errorf("delete %s: %w", c.path, derr)
		}
		if st := byKind[c.kind]; st != nil {
			st.Removed++
			st.Freed += c.bytes
		}
		plan.report.Freed += c.bytes
	}
	plan.report.Applied = true
	return nil
}

// gcManagedPath reports whether path sits under the managed root for kind.
func gcManagedPath(dataDir string, kind gcKind, path string) bool {
	roots := map[gcKind]string{
		gcBlob:     filepath.Join(dataDir, "blobs"),
		gcArtifact: filepath.Join(dataDir, "artifacts"),
		gcSubagent: filepath.Join(dataDir, "subagents"),
		gcDump:     filepath.Join(dataDir, "dumps"),
	}
	root, ok := roots[kind]
	if !ok {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

func gcDirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// gcRender lays the report out as the table the CLI prints.
func gcRender(dataDir string, plan *gcPlan, applied bool) string {
	var b strings.Builder
	mode := "dry run — nothing deleted; pass --yes to collect"
	if applied {
		mode = "collected"
	}
	fmt.Fprintf(&b, "xdev gc — %s\n  %s (retention %s)\n\n", dataDir, mode, plan.report.OlderThan)
	fmt.Fprintf(&b, "  %-10s %7s %10s %8s %12s\n", "KIND", "ITEMS", "SIZE", "ORPHANS", "RECLAIMABLE")
	for _, st := range plan.report.Stats {
		fmt.Fprintf(&b, "  %-10s %7d %10s %8d %12s\n", st.Kind, st.Items, gcBytes(st.Bytes), st.Orphans, gcBytes(st.OrphanSize))
	}
	if applied {
		fmt.Fprintf(&b, "\n  reclaimed: %s in %d items\n", gcBytes(plan.report.Freed), gcRemovedCount(plan.report.Stats))
	} else {
		fmt.Fprintf(&b, "\n  reclaimable: %s in %d items\n", gcBytes(plan.report.Reclaimable), len(plan.candidates))
	}
	kept := make([]string, 0, len(plan.kept))
	for _, k := range gcKinds {
		if line, ok := plan.kept[k]; ok {
			kept = append(kept, string(k)+": "+line)
		}
	}
	if len(kept) > 0 {
		fmt.Fprintf(&b, "  kept: %s\n", strings.Join(kept, "; "))
	}
	if !applied && len(plan.candidates) > 0 {
		sort.Slice(plan.candidates, func(i, j int) bool { return plan.candidates[i].bytes > plan.candidates[j].bytes })
		shown := plan.candidates
		if len(shown) > 5 {
			shown = shown[:5]
		}
		b.WriteString("\n  largest candidates:\n")
		for _, c := range shown {
			fmt.Fprintf(&b, "    %10s  %s (%s)\n", gcBytes(c.bytes), c.path, c.why)
		}
	}
	return b.String()
}

func gcRemovedCount(stats []gcStats) int {
	n := 0
	for _, st := range stats {
		n += st.Removed
	}
	return n
}

// gcBytes renders a size for humans (binary units; the store is local).
func gcBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}
