package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// gcFixture is one synthetic store: a live session that references blob A, an
// orphan blob B, an artifact dir for the live session and one for a session
// that no longer exists, plus an old and a fresh subagent session and a dump.
type gcFixture struct {
	dataDir    string
	liveBlob   string // path
	orphanBlob string
	liveID     string // 8-char short id
	liveArt    string
	deadArt    string
	oldSub     string
	freshSub   string
	oldDump    string
}

// gcTestTree lays the fixture out with old mtimes where the retention window
// is supposed to collect, and current mtimes where it must not.
func gcTestTree(t *testing.T) gcFixture {
	t.Helper()
	dataDir := t.TempDir()
	cwd := t.TempDir()
	old := time.Now().Add(-100 * 24 * time.Hour)

	store := session.NewBlobStore(dataDir)
	liveRef, err := store.Put([]byte("live content"))
	if err != nil {
		t.Fatalf("put live blob: %v", err)
	}
	orphanRef, err := store.Put([]byte("orphan content"))
	if err != nil {
		t.Fatalf("put orphan blob: %v", err)
	}
	liveHex, err := session.ParseBlobRef(liveRef)
	if err != nil {
		t.Fatalf("parse live ref: %v", err)
	}
	orphanHex, err := session.ParseBlobRef(orphanRef)
	if err != nil {
		t.Fatalf("parse orphan ref: %v", err)
	}

	// A real session file that mentions the live blob (that reference is what
	// protects it) and whose id protects its artifact directory.
	sess := session.OpenMem(cwd, "gc fixture")
	sessPath := session.SessionFilePath(dataDir, cwd, time.Now(), sess.ID())
	sess.EnableAutoPersist(sessPath, session.Options{})
	now := time.Now().UnixMilli()
	if err := sess.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleUser,
		Content: []ai.Block{ai.TextBlock{Text: "the plan is in " + liveRef}},
		UserTS:  now,
	}}); err != nil {
		t.Fatalf("append user message: %v", err)
	}
	if err := sess.Append(&session.MessageEntry{Message: ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.Block{ai.TextBlock{Text: "noted"}},
	}}); err != nil {
		t.Fatalf("append assistant message: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	liveID := sess.ID()[:8]

	fx := gcFixture{
		dataDir:    dataDir,
		liveBlob:   filepath.Join(store.Root(), liveHex),
		orphanBlob: filepath.Join(store.Root(), orphanHex),
		liveID:     liveID,
	}
	for _, path := range []string{fx.liveBlob, fx.orphanBlob} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}

	// Artifacts: one owned by the live session, one orphan.
	fx.liveArt = gcTestDir(t, filepath.Join(dataDir, "artifacts", liveID), "live artifact")
	fx.deadArt = gcTestDir(t, filepath.Join(dataDir, "artifacts", "deadbeef"), "orphan artifact")

	// Subagent sessions: age decides, children are never user continuations.
	subRoot := filepath.Join(dataDir, "subagents", "sessions", "-tmp-gc")
	fx.oldSub = gcTestFile(t, filepath.Join(subRoot, "2026-01-01T00-00-00.000Z_oldoldold.jsonl"), "old child", old)
	fx.freshSub = gcTestFile(t, filepath.Join(subRoot, "2026-01-02T00-00-00.000Z_newnewnew.jsonl"), "fresh child", time.Now())

	// Dumps follow the same rule.
	fx.oldDump = gcTestFile(t, filepath.Join(dataDir, "dumps", "dumpdump.md"), "old dump", old)
	return fx
}

func gcTestDir(t *testing.T, path, name string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	gcTestFile(t, filepath.Join(path, name+".bin"), "payload", time.Now())
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
	return path
}

func gcTestFile(t *testing.T, path, content string, mod time.Time) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
	return path
}

func gcAllKinds(t *testing.T) map[gcKind]bool {
	t.Helper()
	kinds, err := gcSelectKinds("")
	if err != nil {
		t.Fatalf("gcSelectKinds: %v", err)
	}
	return kinds
}

func gcExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestGCScanProtectsLiveSessionsAndReferences is the core contract: the plan
// proposes the orphan blob/artifact/old child/dump and never the live ones.
func TestGCScanProtectsLiveSessionsAndReferences(t *testing.T) {
	fx := gcTestTree(t)
	plan, err := gcScan(fx.dataDir, gcOptions{now: time.Now(), olderThan: 720 * time.Hour, kinds: gcAllKinds(t)})
	if err != nil {
		t.Fatalf("gcScan: %v", err)
	}
	planned := map[string]bool{}
	for _, c := range plan.candidates {
		planned[c.path] = true
	}
	if !planned[fx.orphanBlob] {
		t.Fatalf("orphan blob not planned for collection: %v", plan.candidates)
	}
	if planned[fx.liveBlob] {
		t.Fatal("blob referenced by a live session was planned for collection")
	}
	if !planned[fx.deadArt] {
		t.Fatal("artifact of a deleted session was not planned")
	}
	if planned[fx.liveArt] {
		t.Fatal("artifact of a live session was planned for collection")
	}
	if !planned[fx.oldSub] {
		t.Fatal("subagent session past the retention window was not planned")
	}
	if planned[fx.freshSub] {
		t.Fatal("fresh subagent session was planned for collection")
	}
	if !planned[fx.oldDump] {
		t.Fatal("old dump was not planned")
	}
	if plan.report.Reclaimable <= 0 {
		t.Fatalf("reclaimable = %d, want > 0", plan.report.Reclaimable)
	}

	// Dry run report is honest about not having deleted anything.
	var buf bytes.Buffer
	if code := gcCmd([]string{"--json"}, fx.dataDir, &buf, &buf); code != 0 {
		t.Fatalf("gcCmd --json = %d (%s)", code, buf.String())
	}
	var decoded gcReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("json report: %v", err)
	}
	if decoded.Applied || decoded.Reclaimable == 0 {
		t.Fatalf("dry-run report = %+v", decoded)
	}
	if !gcExists(fx.orphanBlob) {
		t.Fatal("dry run deleted the orphan blob")
	}
}

// TestGCSweepRemovesOnlyOrphans runs the deleting half and pins both sides:
// the orphans go, the live session's bytes stay.
func TestGCSweepRemovesOnlyOrphans(t *testing.T) {
	fx := gcTestTree(t)
	plan, err := gcScan(fx.dataDir, gcOptions{now: time.Now(), olderThan: 720 * time.Hour, kinds: gcAllKinds(t)})
	if err != nil {
		t.Fatalf("gcScan: %v", err)
	}
	if err := gcSweep(fx.dataDir, plan); err != nil {
		t.Fatalf("gcSweep: %v", err)
	}
	if gcExists(fx.orphanBlob) {
		t.Fatal("orphan blob survived the sweep")
	}
	if !gcExists(fx.liveBlob) {
		t.Fatal("live blob was collected (a live session loses data)")
	}
	if gcExists(fx.deadArt) {
		t.Fatal("orphan artifact survived the sweep")
	}
	if !gcExists(fx.liveArt) {
		t.Fatal("live artifact was collected")
	}
	if gcExists(fx.oldSub) {
		t.Fatal("old subagent session survived the sweep")
	}
	if !gcExists(fx.freshSub) {
		t.Fatal("fresh subagent session was collected")
	}
	if gcExists(fx.oldDump) {
		t.Fatal("old dump survived the sweep")
	}
	if plan.report.Freed <= 0 || !plan.report.Applied {
		t.Fatalf("sweep report = %+v", plan.report)
	}

	// The session file itself is untouchable: it must still parse and its
	// referenced blob must still resolve.
	metas, err := session.List(fx.dataDir)
	if err != nil || len(metas) != 1 {
		t.Fatalf("session store after gc: %v metas=%d", err, len(metas))
	}
	store, err := session.Open(metas[0].Path)
	if err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	defer store.Close()
	if len(store.Entries()) == 0 {
		t.Fatal("session reopened empty")
	}
	refs, err := gcBlobRefs(fx.dataDir)
	if err != nil {
		t.Fatalf("gcBlobRefs: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("the session's blob reference disappeared")
	}
}

// TestGCCmdYesAppliesAndRefusesForeignPaths pins --yes and the path guard that
// keeps a scanner bug from deleting outside the managed roots.
func TestGCCmdYesAppliesAndRefusesForeignPaths(t *testing.T) {
	fx := gcTestTree(t)
	var out, errOut bytes.Buffer
	if code := gcCmd([]string{"--yes", "--kind", "blob"}, fx.dataDir, &out, &errOut); code != 0 {
		t.Fatalf("gcCmd --yes = %d (%s)", code, errOut.String())
	}
	if gcExists(fx.orphanBlob) {
		t.Fatal("--yes did not collect the orphan blob")
	}
	if !gcExists(fx.deadArt) {
		t.Fatal("--kind blob collected an artifact")
	}
	if !strings.Contains(out.String(), "collected") {
		t.Fatalf("report does not say it collected:\n%s", out.String())
	}

	if gcManagedPath(fx.dataDir, gcBlob, filepath.Join(fx.dataDir, "sessions", "x.jsonl")) {
		t.Fatal("path guard allowed a session-store path")
	}
	if gcManagedPath(fx.dataDir, gcBlob, fx.orphanBlob) != true {
		t.Fatal("path guard rejected a managed blob path")
	}
	if _, err := gcSelectKinds("blobs"); err == nil {
		t.Fatal("gcSelectKinds accepted an unknown kind")
	}
	if code := gcCmd([]string{"--kind", "nope"}, fx.dataDir, &out, &errOut); code != 2 {
		t.Fatalf("unknown --kind exit = %d, want 2", code)
	}
}
