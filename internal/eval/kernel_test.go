package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestKernel(t *testing.T) *Kernel {
	t.Helper()
	if _, err := ResolvePython(); err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	k := NewKernel(t.TempDir())
	t.Cleanup(func() { _ = k.Close() })
	return k
}

func runCell(t *testing.T, k *Kernel, code string, timeout time.Duration) Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := k.RunCell(ctx, code, timeout)
	if err != nil {
		t.Fatalf("RunCell(%q): %v", code, err)
	}
	return out
}

func TestKernelPersistsState(t *testing.T) {
	k := newTestKernel(t)
	if out := runCell(t, k, "x = 41", DefaultCellTimeout); out.Status != "ok" {
		t.Fatalf("first cell: status=%q text=%q", out.Status, out.Text)
	}
	out := runCell(t, k, "x + 1", DefaultCellTimeout)
	if out.Status != "ok" || !strings.Contains(out.Text, "42") {
		t.Fatalf("state did not persist: status=%q text=%q", out.Status, out.Text)
	}
}

func TestKernelCapturesStreamsAndValue(t *testing.T) {
	k := newTestKernel(t)
	out := runCell(t, k, `print("hello")`, 0)
	if !strings.Contains(out.Text, "hello") {
		t.Fatalf("stdout not captured: %q", out.Text)
	}
	// stderr is surfaced, and the write's return value (len("warned")) is the
	// cell result.
	out = runCell(t, k, "import sys; sys.stderr.write('warned')", 0)
	if !strings.Contains(out.Text, "warned") || !strings.Contains(out.Text, "6") {
		t.Fatalf("stderr/value not captured: %q", out.Text)
	}
}

func TestKernelReportsError(t *testing.T) {
	k := newTestKernel(t)
	out := runCell(t, k, "1/0", DefaultCellTimeout)
	if out.Status != "error" || !strings.Contains(out.Text, "ZeroDivisionError") {
		t.Fatalf("error not surfaced: status=%q text=%q", out.Status, out.Text)
	}
	// The kernel stays usable after a failed cell.
	if out := runCell(t, k, "2+2", DefaultCellTimeout); !strings.Contains(out.Text, "4") {
		t.Fatalf("kernel unusable after error: %q", out.Text)
	}
}

func TestKernelTimeoutInterruptsButSurvives(t *testing.T) {
	k := newTestKernel(t)
	start := time.Now()
	out := runCell(t, k, "import time; time.sleep(30)", 300*time.Millisecond)
	if !out.TimedOut || out.Status == "ok" {
		t.Fatalf("timeout not reported: status=%q timedOut=%v", out.Status, out.TimedOut)
	}
	if got := time.Since(start); got > 8*time.Second {
		t.Fatalf("timeout took too long: %v", got)
	}
	// The interrupt lands as KeyboardInterrupt, so the next cell still works.
	if out := runCell(t, k, "21*2", DefaultCellTimeout); !strings.Contains(out.Text, "42") {
		t.Fatalf("kernel did not survive the interrupt: %q", out.Text)
	}
}

func TestKernelTimeoutEscalatesToKillAndRestarts(t *testing.T) {
	k := newTestKernel(t)
	// A cell that ignores SIGINT forces the SIGKILL escalation.
	start := time.Now()
	out := runCell(t, k,
		"import signal, time; signal.signal(signal.SIGINT, signal.SIG_IGN); time.sleep(60)",
		300*time.Millisecond)
	if !out.TimedOut {
		t.Fatalf("escalation not reported: status=%q text=%q", out.Status, out.Text)
	}
	if got := time.Since(start); got > KillGrace*2+5*time.Second {
		t.Fatalf("escalation took too long: %v", got)
	}
	// The killed kernel is restarted transparently.
	if out := runCell(t, k, "1+1", DefaultCellTimeout); !strings.Contains(out.Text, "2") {
		t.Fatalf("kernel did not restart after SIGKILL: %q", out.Text)
	}
}

func TestKernelResetWipesNamespace(t *testing.T) {
	k := newTestKernel(t)
	runCell(t, k, "y = 5", DefaultCellTimeout)
	if err := k.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	out := runCell(t, k, "y", DefaultCellTimeout)
	if out.Status != "error" || !strings.Contains(out.Text, "NameError") {
		t.Fatalf("reset did not wipe the namespace: status=%q text=%q", out.Status, out.Text)
	}
}

func TestKernelClosed(t *testing.T) {
	k := newTestKernel(t)
	if err := k.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := k.RunCell(ctx, "1", 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("RunCell after Close: got %v, want ErrClosed", err)
	}
}

func TestKernelIsExclusive(t *testing.T) {
	k := newTestKernel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = k.RunCell(ctx, "import time; time.sleep(5)", 2*time.Second)
	}()
	// Wait until the first cell is registered, then a second cell must bounce.
	for i := 0; i < 400; i++ {
		k.mu.Lock()
		busy := k.active != nil
		k.mu.Unlock()
		if busy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := k.RunCell(ctx, "1", 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent cell: got %v, want ErrBusy", err)
	}
	<-done
}

func TestBase64Len(t *testing.T) {
	cases := map[string]int{
		"": 0, "YQ==": 1, "YWI=": 2, "YWJj": 3, "YWJjZA==": 4,
	}
	for enc, want := range cases {
		if got := base64Len(enc); got != want {
			t.Errorf("base64Len(%q) = %d, want %d", enc, got, want)
		}
	}
}
