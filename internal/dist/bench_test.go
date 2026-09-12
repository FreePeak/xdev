package dist

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// stubProvider is a BenchProvider emitting a fixed number of deltas, so the
// measured shape (not the exact latency) is what the tests assert.
type stubProvider struct {
	deltas   []string
	failTurn int // 1-based turn returning a stream error (0 = none)
	turn     int
}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Stream(ctx context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	s.turn++
	if s.turn == s.failTurn {
		return nil, fmt.Errorf("stub: turn %d failed", s.turn)
	}
	ch := make(chan ai.Event, len(s.deltas)+1)
	go func() {
		defer close(ch)
		for _, d := range s.deltas {
			select {
			case <-ctx.Done():
				return
			case ch <- ai.Event{Type: ai.EventTextDelta, Delta: d}:
			}
		}
		ch <- ai.Donef(ai.StopReasonStop, &ai.Usage{Output: int64(len(s.deltas))}, nil)
	}()
	return ch, nil
}

func stubOpen(s *stubProvider) OpenProvider {
	return func(ref string) (BenchProvider, string, error) {
		model := ref
		if model == "" {
			model = "stub/model"
		}
		return s, model, nil
	}
}

func runBench(t *testing.T, args []string, open OpenProvider) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := benchMain(args, "0.1.0-test", open, &out, &errw)
	return code, out.String(), errw.String()
}

func benchRows(out string) []string {
	return regexp.MustCompile(`(?m)^\s+\d+\s+[\d.]+\s+[\d.]+\s+\d+\s*$`).FindAllString(out, -1)
}

func TestBenchPrintsTableAndPercentiles(t *testing.T) {
	prov := &stubProvider{deltas: []string{"a", "b", "c", "d", "e", "f", "g", "h"}}
	code, out, errw := runBench(t, []string{"--turns", "3"}, stubOpen(prov))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	if !strings.Contains(out, "provider=stub model=stub/model") {
		t.Errorf("header lacks the resolved provider/model:\n%s", out)
	}
	if !strings.Contains(out, "turns=3") {
		t.Errorf("header lacks the turn count:\n%s", out)
	}
	if rows := benchRows(out); len(rows) != 3 {
		t.Errorf("got %d data rows, want 3:\n%s", len(rows), out)
	}
	for _, want := range []string{"ttft", "decode", "p50", "p95", "3/3 turns ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if prov.turn != 3 {
		t.Errorf("provider called %d times, want 3 (--turns bounds the run)", prov.turn)
	}
}

func TestBenchReportsFailedTurnWithoutAborting(t *testing.T) {
	prov := &stubProvider{deltas: []string{"a", "b"}, failTurn: 2}
	code, out, errw := runBench(t, []string{"--turns", "3"}, stubOpen(prov))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (one failed turn must not abort the run), stderr = %s", code, errw)
	}
	if !strings.Contains(out, "failed turns") || !strings.Contains(out, "turn 2: stub: turn 2 failed") {
		t.Errorf("output does not report the failed turn:\n%s", out)
	}
	if !strings.Contains(out, "2/3 turns ok") {
		t.Errorf("summary does not count the successes:\n%s", out)
	}
	if rows := benchRows(out); len(rows) != 2 {
		t.Errorf("got %d data rows, want 2:\n%s", len(rows), out)
	}
}

func TestBenchFailsWhenEveryTurnFails(t *testing.T) {
	prov := &stubProvider{deltas: []string{"a"}, failTurn: 1}
	code, _, errw := runBench(t, []string{"--turns", "1"}, stubOpen(prov))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errw, "every turn failed") {
		t.Errorf("stderr:\n%s", errw)
	}
}

func TestBenchBoundsTurns(t *testing.T) {
	prov := &stubProvider{deltas: []string{"a"}}
	code, out, errw := runBench(t, []string{"--turns", "999"}, stubOpen(prov))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	if !strings.Contains(errw, "beyond the 20-turn bound") {
		t.Errorf("stderr does not explain the clamp:\n%s", errw)
	}
	if prov.turn != benchMaxTurns {
		t.Errorf("provider called %d times, want the %d-turn bound", prov.turn, benchMaxTurns)
	}
	if !strings.Contains(out, fmt.Sprintf("%d/%d turns ok", benchMaxTurns, benchMaxTurns)) {
		t.Errorf("summary does not count the bounded turns:\n%s", out)
	}
}

func TestBenchWithoutProviderResolver(t *testing.T) {
	code, _, errw := runBench(t, nil, nil)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errw, "no provider resolver") {
		t.Errorf("stderr:\n%s", errw)
	}
}

// The provider's pre-first-token delay is TTFT and must not be charged to
// the decode rate: a slow first token would otherwise look like a fast
// stream that took a long time.
func TestBenchTTFTExcludesDecodeWindow(t *testing.T) {
	slow := &delayedProvider{start: 200 * time.Millisecond, gap: 10 * time.Millisecond, deltas: 4}
	code, out, errw := runBench(t, []string{"--turns", "1"}, func(string) (BenchProvider, string, error) {
		return slow, "stub/slow", nil
	})
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errw)
	}
	m := regexp.MustCompile(`(?m)^\s+1\s+([\d.]+)\s+([\d.]+)\s+(\d+)\s*$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no data row in:\n%s", out)
	}
	if ms, _ := strconv.ParseFloat(m[1], 64); ms < 150 {
		t.Errorf("ttft = %.1f ms, want at least the 200 ms pre-delta delay", ms)
	}
	if rate, _ := strconv.ParseFloat(m[2], 64); rate > 500 {
		t.Errorf("decode = %.1f tok/s: the first token's latency leaked into the decode rate", rate)
	}
}

// delayedProvider waits before the first delta (TTFT), then emits deltas
// with a fixed gap.
type delayedProvider struct {
	start  time.Duration
	gap    time.Duration
	deltas int
}

func (d *delayedProvider) Name() string { return "delayed" }

func (d *delayedProvider) Stream(ctx context.Context, _ ai.StreamRequest) (<-chan ai.Event, error) {
	ch := make(chan ai.Event, d.deltas+1)
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.start):
		}
		for range d.deltas {
			select {
			case <-ctx.Done():
				return
			case ch <- ai.Event{Type: ai.EventTextDelta, Delta: "x"}:
			}
			time.Sleep(d.gap)
		}
		ch <- ai.Donef(ai.StopReasonStop, &ai.Usage{Output: int64(d.deltas)}, nil)
	}()
	return ch, nil
}

func TestPercentileNearestRank(t *testing.T) {
	v := []float64{10, 20, 30, 40, 50}
	for _, c := range []struct {
		p    float64
		want float64
	}{
		{0, 10},
		{50, 30},
		{95, 50},
		{100, 50},
	} {
		if got := percentile(v, c.p); got != c.want {
			t.Errorf("percentile(%v, %v) = %v, want %v", v, c.p, got, c.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
	// Input order must not matter and the caller's slice must not be sorted
	// in place.
	unsorted := []float64{30, 10, 20}
	if got := percentile(unsorted, 50); got != 20 {
		t.Errorf("percentile(%v, 50) = %v, want 20", unsorted, got)
	}
	if unsorted[0] != 30 {
		t.Errorf("percentile sorted its input: %v", unsorted)
	}
}
