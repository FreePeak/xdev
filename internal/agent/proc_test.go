package agent

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// start/logs/stop lifecycle over real binaries.
func TestProcLifecycle(t *testing.T) {
	tbl := NewProcTable()
	ctx := context.Background()
	info, err := tbl.Start(ctx, ProcSpec{Name: "svc", App: "sleep", Args: []string{"30"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "running" || info.PID == 0 {
		t.Fatalf("start info = %+v", info)
	}
	if _, ok := tbl.Describe("svc"); !ok {
		t.Fatal("describe missed a running process")
	}
	if ps := tbl.PS(); len(ps) != 1 || ps[0].Name != "svc" {
		t.Fatalf("ps = %+v", ps)
	}
	if lines, ok := tbl.Logs("svc", 0); !ok || len(lines) != 0 {
		t.Fatalf("logs before any output = %v ok=%v", lines, ok)
	}
	if err := tbl.WriteInput("svc", ""); err != nil {
		t.Fatalf("stdin write to a running process: %v", err)
	}
	if err := tbl.Stop("svc", ""); err != nil {
		t.Fatalf("stop: %v", err)
	}
	got, _ := tbl.Describe("svc")
	if got.Status != "stopped" {
		t.Fatalf("status after stop = %q", got.Status)
	}
	if err := tbl.Stop("svc", ""); err != nil {
		t.Fatalf("stop of a settled process must be a no-op: %v", err)
	}
	if _, ok := tbl.Describe("nope"); ok {
		t.Fatal("describe of an unknown process must be false")
	}
	if _, ok := tbl.Logs("nope", 10); ok {
		t.Fatal("logs of an unknown process must be false")
	}
	if err := tbl.Stop("nope", ""); err == nil {
		t.Fatal("stop of an unknown process must error")
	}
}

// Output lands in the ring buffer and the exit is observed.
func TestProcLogsCaptureExit(t *testing.T) {
	tbl := NewProcTable()
	if _, err := tbl.Start(context.Background(), ProcSpec{
		Name: "echo", App: "echo", Args: []string{"ring-hello"},
	}); err != nil {
		t.Fatal(err)
	}
	waitProcStatus(t, tbl, "echo", "exited")
	lines, ok := tbl.Logs("echo", 10)
	if !ok || len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "ring-hello") {
		t.Fatalf("captured lines = %v ok=%v", lines, ok)
	}
	// Re-launching the same name is allowed once the old one settled.
	if _, err := tbl.Start(context.Background(), ProcSpec{Name: "echo", App: "echo", Args: []string{"again"}}); err != nil {
		t.Fatalf("restart by start: %v", err)
	}
	waitProcStatus(t, tbl, "echo", "exited")
}

// Readiness gating: the log regex and the TCP port probes.
func TestProcReadiness(t *testing.T) {
	tbl := NewProcTable()
	info, err := tbl.Start(context.Background(), ProcSpec{
		Name: "ready-log", App: "sh",
		Args:         []string{"-c", "echo BOOT-77 && sleep 30"},
		ReadyLog:     "BOOT-77",
		ReadyTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("log readiness: %v", err)
	}
	if info.Ready != "log" || info.Status != "running" {
		t.Fatalf("ready info = %+v", info)
	}
	if err := tbl.Stop("ready-log", ""); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	info, err = tbl.Start(context.Background(), ProcSpec{
		Name: "ready-port", App: "sleep", Args: []string{"30"},
		ReadyPort: port, ReadyTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("port readiness: %v", err)
	}
	if info.Ready != "port" {
		t.Fatalf("port ready info = %+v", info)
	}
	if err := tbl.Stop("ready-port", ""); err != nil {
		t.Fatal(err)
	}
}

// Readiness never lies: a process that exits before its ready condition
// fails the start instead of reporting success.
func TestProcReadyFailsWhenProcessExits(t *testing.T) {
	tbl := NewProcTable()
	_, err := tbl.Start(context.Background(), ProcSpec{
		Name: "short", App: "sh", Args: []string{"-c", "echo NOPE"},
		ReadyLog: "READY", ReadyTimeout: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "before ready") {
		t.Fatalf("err = %v, want a before-ready failure", err)
	}
}

// Restart re-launches a settled process under its original spec.
func TestProcRestart(t *testing.T) {
	tbl := NewProcTable()
	ctx := context.Background()
	if _, err := tbl.Start(ctx, ProcSpec{Name: "boot", App: "sleep", Args: []string{"30"}}); err != nil {
		t.Fatal(err)
	}
	first, _ := tbl.Describe("boot")
	if _, err := tbl.Start(ctx, ProcSpec{Name: "boot", App: "sleep"}); err == nil {
		t.Fatal("starting a running name must be refused")
	}
	info, err := tbl.Restart(ctx, "boot")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if info.PID == first.PID {
		t.Fatalf("restart reused the pid %d", info.PID)
	}
	if err := tbl.Stop("boot", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := tbl.Restart(ctx, "nope"); err == nil {
		t.Fatal("restart of an unknown process must error")
	}
}

func waitProcStatus(t *testing.T, tbl *ProcTable, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, ok := tbl.Describe(name); ok && info.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, _ := tbl.Describe(name)
	t.Fatalf("%s status = %+v, want %s", name, info, want)
}

// The hub owns one process table, created on demand.
func TestHubProcsLazyTable(t *testing.T) {
	h := NewHub()
	if h.Procs() == nil || h.Procs() != h.Procs() {
		t.Fatal("Procs must return a stable table")
	}
	if len(h.Roster()) != 0 {
		t.Fatal("a fresh hub has an empty roster")
	}
}
