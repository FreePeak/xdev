package dist

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
)

// BenchProvider is the slice of ai.Provider that bench measures, so the
// measurement can be driven by a stub in tests (ai.Provider satisfies it).
type BenchProvider interface {
	Stream(ctx context.Context, req ai.StreamRequest) (<-chan ai.Event, error)
	Name() string
}

// OpenProvider builds the provider bench measures: the CLI supplies this
// because provider construction (models.yml + the credential chain) lives
// there. ref is the --model value; "" means the configured default. The
// returned string is the resolved model id.
type OpenProvider func(ref string) (BenchProvider, string, error)

const (
	benchDefaultTurns = 3
	// benchMaxTurns bounds an accidental `--turns 10000`: bench spends real
	// provider quota, one request per turn.
	benchMaxTurns    = 20
	benchTurnTimeout = 2 * time.Minute
	benchPrompt      = "Write one dense paragraph about how Go's garbage collector decides when to run."
)

// benchSample is one measured turn. decode is tokens/second after the first
// token (the first token's latency is ttft, so it is excluded).
type benchSample struct {
	turn   int
	ttft   time.Duration
	decode float64
	tokens int
	err    error
}

// benchMain implements `xdev bench [--turns N] [--model ref] [--max-tokens N]`.
func benchMain(args []string, version string, open OpenProvider, out, errw io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(errw)
	turns := fs.Int("turns", benchDefaultTurns, "measured turns (1-20)")
	modelRef := fs.String("model", "", "model to measure (provider/model or @role; default: configured model)")
	maxTokens := fs.Int("max-tokens", 256, "output token cap per turn")
	fs.Usage = func() {
		fmt.Fprint(errw, "usage: xdev bench [--turns N] [--model ref] [--max-tokens N]\n\nReports time-to-first-token and decode throughput (p50/p95) for the\nconfigured provider — no tools, one request per turn.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if open == nil {
		fmt.Fprintln(errw, "xdev: bench: no provider resolver wired")
		return 1
	}
	n := *turns
	if n < 1 {
		n = 1
	}
	if n > benchMaxTurns {
		fmt.Fprintf(errw, "xdev: bench: --turns %d is beyond the %d-turn bound; measuring %d\n", *turns, benchMaxTurns, benchMaxTurns)
		n = benchMaxTurns
	}
	prov, model, err := open(*modelRef)
	if err != nil {
		fmt.Fprintln(errw, "xdev:", err)
		return 1
	}

	fmt.Fprintf(out, "xdev bench — version=%s provider=%s model=%s\n", displayVersion(version), prov.Name(), model)
	fmt.Fprintf(out, "turns=%d maxTokens=%d prompt=%q\n\n", n, *maxTokens, benchPrompt)

	ctx := context.Background()
	samples := make([]benchSample, 0, n)
	started := time.Now()
	for i := range n {
		s := benchTurn(ctx, prov, model, *maxTokens)
		s.turn = i + 1
		samples = append(samples, s)
	}
	ok := renderBench(out, samples, time.Since(started))
	if ok == 0 {
		fmt.Fprintln(errw, "xdev: bench: every turn failed")
		return 1
	}
	return 0
}

// benchTurn streams one request and measures it. A provider error is
// recorded, never fatal: one bad turn must not discard the whole run.
func benchTurn(ctx context.Context, prov BenchProvider, model string, maxTokens int) benchSample {
	req := ai.StreamRequest{
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: benchPrompt}}}},
		Model:     model,
		MaxTokens: maxTokens,
	}
	tctx, cancel := context.WithTimeout(ctx, benchTurnTimeout)
	defer cancel()

	start := time.Now()
	ch, err := prov.Stream(tctx, req)
	if err != nil {
		return benchSample{err: err}
	}
	var (
		first, last time.Time
		deltas      int
		outTokens   int
		streamErr   error
	)
	for ev := range ch {
		switch ev.Type {
		case ai.EventTextDelta, ai.EventThinkingDelta:
			if first.IsZero() {
				first = time.Now()
			}
			last = time.Now()
			deltas++
		case ai.EventToolcallStart:
			if first.IsZero() {
				first = time.Now()
			}
		case ai.EventDone:
			if ev.Usage != nil && ev.Usage.Output > 0 {
				outTokens = int(ev.Usage.Output)
			}
		case ai.EventError:
			if ev.Err != nil && streamErr == nil {
				streamErr = ev.Err
			}
		}
	}
	if streamErr != nil {
		return benchSample{err: streamErr}
	}
	if first.IsZero() {
		return benchSample{err: fmt.Errorf("stream produced no output")}
	}
	s := benchSample{ttft: first.Sub(start), tokens: deltas}
	// The provider's own token count is authoritative when present; the
	// streamed delta count is the fallback.
	if outTokens > 0 {
		s.tokens = outTokens
	}
	if d := last.Sub(first); d > 0 && s.tokens > 1 {
		s.decode = float64(s.tokens-1) / d.Seconds()
	}
	return s
}

// renderBench prints the per-turn table plus the p50/p95 summary and
// returns the number of successful turns.
func renderBench(out io.Writer, samples []benchSample, wall time.Duration) int {
	fmt.Fprintln(out, "  turn   ttft ms   decode tok/s   out tokens")
	ttfts := make([]float64, 0, len(samples))
	rates := make([]float64, 0, len(samples))
	var failures []benchSample
	for _, s := range samples {
		if s.err != nil {
			failures = append(failures, s)
			continue
		}
		ttfts = append(ttfts, float64(s.ttft.Microseconds())/1000)
		rates = append(rates, s.decode)
		fmt.Fprintf(out, "  %4d %9.1f %14.1f %12d\n", s.turn, float64(s.ttft.Microseconds())/1000, s.decode, s.tokens)
	}
	if len(failures) > 0 {
		fmt.Fprintln(out, "\n  failed turns")
		for _, s := range failures {
			fmt.Fprintf(out, "    turn %d: %v\n", s.turn, s.err)
		}
	}
	fmt.Fprintf(out, "\n  ttft   p50 %8.1f ms   p95 %8.1f ms\n", percentile(ttfts, 50), percentile(ttfts, 95))
	fmt.Fprintf(out, "  decode p50 %8.1f tok/s p95 %8.1f tok/s\n", percentile(rates, 50), percentile(rates, 95))
	fmt.Fprintf(out, "  %d/%d turns ok in %.1fs\n", len(ttfts), len(samples), wall.Seconds())
	return len(ttfts)
}

// percentile is the nearest-rank percentile of v (0 for an empty sample).
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[min(max(rank-1, 0), len(sorted)-1)]
}
