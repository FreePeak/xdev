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
