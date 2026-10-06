package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStripBgFlag(t *testing.T) {
	in := []string{"--model", "x", "--bg", "--max-turns", "3", "-bg", "prompt-not-a-flag"}
	got := stripBgFlag(in)
	want := []string{"--model", "x", "--max-turns", "3", "prompt-not-a-flag"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stripBgFlag = %v, want %v", got, want)
	}
	// =value form
	got = stripBgFlag([]string{"--bg=true", "--print", "hi"})
	if strings.Join(got, " ") != "--print hi" {
		t.Fatalf("strip =value: %v", got)
	}
}

func TestBgStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	// config.DataDir reads the env; force a clean root.
	st := bgStatus{
		ID:      "abcd1234",
		PID:     1,
		CWD:     "/tmp",
		Prompt:  "hello",
		Started: time.Now().UTC().Truncate(time.Second),
	}
	if err := writeBgStatus(st); err != nil {
		t.Fatal(err)
	}
	got, err := readBgStatus("abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != st.ID || got.Prompt != "hello" || got.PID != 1 {
		t.Fatalf("round trip: %+v", got)
	}
	if got.LogPath != bgLogPath("abcd1234") {
		t.Fatalf("log path = %q", got.LogPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "bg", "abcd1234", "status.json")); err != nil {
		t.Fatal(err)
	}
}

func TestListBgReconcilesDeadPID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	code := (*int)(nil)
	st := bgStatus{
		ID:       "dead0001",
		PID:      999999999, // not a live pid
		CWD:      "/tmp",
		Started:  time.Now().UTC(),
		ExitCode: code,
	}
	if err := writeBgStatus(st); err != nil {
		t.Fatal(err)
	}
	rows, err := listBgStatuses()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].ExitCode == nil {
		t.Fatal("dead pid was not reconciled to finished")
	}
	if bgStateLabel(rows[0]) == "running" {
		t.Fatal("label still running after reconcile")
	}
}

func TestResolveBgIDPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	for _, id := range []string{"aaaabbbb", "ccccdddd"} {
		if err := writeBgStatus(bgStatus{ID: id, PID: 1, Started: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveBgID("aaaa")
	if err != nil || got != "aaaabbbb" {
		t.Fatalf("prefix: %q %v", got, err)
	}
	if _, err := resolveBgID("zzz"); err == nil {
		t.Fatal("expected miss")
	}
}

func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	if err := os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != "c\nd" {
		t.Fatalf("tail 2 = %q", got)
	}
	all, err := tailFile(p, 0)
	if err != nil || all != "a\nb\nc\nd\n" {
		t.Fatalf("tail all = %q err=%v", all, err)
	}
}

func TestRmBgRefusesRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	// Use our own PID so bgAlive is true.
	st := bgStatus{ID: "live0001", PID: os.Getpid(), Started: time.Now().UTC()}
	if err := writeBgStatus(st); err != nil {
		t.Fatal(err)
	}
	if err := rmBg("live0001"); err == nil {
		t.Fatal("rm of live job must fail")
	}
	// Mark finished, then rm succeeds.
	code := 0
	st.ExitCode = &code
	st.Finished = time.Now().UTC()
	if err := writeBgStatus(st); err != nil {
		t.Fatal(err)
	}
	if err := rmBg("live0001"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bgJobDir("live0001")); !os.IsNotExist(err) {
		t.Fatalf("job dir still present: %v", err)
	}
}

func TestRenderBgListEmpty(t *testing.T) {
	var b strings.Builder
	renderBgList(&b, nil)
	if !strings.Contains(b.String(), "no background jobs") {
		t.Fatalf("empty list: %q", b.String())
	}
}

// TestPruneBgSkipsLiveAndRecent is the guard the sweep exists for: a prune must
// take the old corpses and leave both a running job and a freshly finished one
// alone. Getting this backwards deletes a live run's status file out from under
// it, so the test asserts the survivors by directory existence, not by count.
func TestPruneBgSkipsLiveAndRecent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	now := time.Now().UTC()
	zero := 0

	// live: our own pid, no exit code — must survive any prune.
	if err := writeBgStatus(bgStatus{ID: "live0001", PID: os.Getpid(), Started: now}); err != nil {
		t.Fatal(err)
	}
	// recent: finished a minute ago — inside the window, must survive.
	if err := writeBgStatus(bgStatus{
		ID: "recent01", PID: 999999999, Started: now.Add(-time.Minute),
		Finished: now.Add(-time.Minute), ExitCode: &zero,
	}); err != nil {
		t.Fatal(err)
	}
	// ancient: finished long ago — the only candidate.
	if err := writeBgStatus(bgStatus{
		ID: "ancient1", PID: 999999998, Started: now.Add(-30 * 24 * time.Hour),
		Finished: now.Add(-30 * 24 * time.Hour), ExitCode: &zero,
	}); err != nil {
		t.Fatal(err)
	}

	// Dry run reports the candidate and deletes nothing.
	rows, err := pruneBg(bgPruneDefaultAge, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "ancient1" {
		t.Fatalf("dry run picked %v, want [ancient1]", rows)
	}
	for _, id := range []string{"live0001", "recent01", "ancient1"} {
		if _, err := os.Stat(bgJobDir(id)); err != nil {
			t.Fatalf("dry run deleted %s: %v", id, err)
		}
	}

	rows, err = pruneBg(bgPruneDefaultAge, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "ancient1" {
		t.Fatalf("prune picked %v, want [ancient1]", rows)
	}
	if _, err := os.Stat(bgJobDir("ancient1")); !os.IsNotExist(err) {
		t.Fatalf("ancient job survived the sweep: %v", err)
	}
	if _, err := os.Stat(bgJobDir("live0001")); err != nil {
		t.Fatalf("prune removed a live job: %v", err)
	}
	if _, err := os.Stat(bgJobDir("recent01")); err != nil {
		t.Fatalf("prune removed a job inside the window: %v", err)
	}
}

func TestPruneBgEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dir)
	rows, err := pruneBg(bgPruneDefaultAge, false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("prune of an empty store = %v, %v", rows, err)
	}
	var b strings.Builder
	renderBgPrune(&b, nil, false)
	if !strings.Contains(b.String(), "no finished background jobs") {
		t.Fatalf("empty prune render: %q", b.String())
	}
}
