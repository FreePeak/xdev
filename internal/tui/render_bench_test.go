package tui

// Benchmarks for the block renderer's hot path.
//
// They answer one question: what does re-rendering ONE streaming block cost,
// because that is what a delta pays. renderKey stamps a block on len(b.Text),
// so a block whose text grew is rendered whole again — every word measured and
// wrapped — on every frame of a live turn. The fixtures are synthesized prose,
// so no repository content becomes a dependency of the test run.

import (
	"strings"
	"testing"
)

// benchProse is `kb` KB of a single markdown body line: no blank lines, no
// fences, no URLs — the plain prose a model streams most of.
func benchProse(kb int) string {
	return strings.Repeat("the quick brown fox jumps over the lazy dog and keeps going ", kb*1024/66)
}

// BenchmarkRenderStreamingBlock measures the render a delta pays for one
// message, at the sizes a long answer reaches.
func BenchmarkRenderStreamingBlock(b *testing.B) {
	app := benchApp(b, 120, 40)
	for _, kb := range []int{64, 256, 700} {
		text := benchProse(kb)
		b.Run(sizeLabel(kb), func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for range b.N {
				_ = app.renderMarkdown(text, 114)
			}
		})
	}
}

// BenchmarkWrapStreamingBlock is the same body through wrapLine alone, which is
// where the render's allocation goes: one wrapped visual line per word.
func BenchmarkWrapStreamingBlock(b *testing.B) {
	app := benchApp(b, 120, 40)
	for _, kb := range []int{64, 256, 700} {
		lines := app.renderMarkdown(benchProse(kb), 114)
		b.Run(sizeLabel(kb), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				for _, ln := range lines {
					_ = wrapLine(ln, 114)
				}
			}
		})
	}
}

// BenchmarkWebURLStartProse measures the URL scan on prose with no scheme in it,
// which is the case the fast path exists for: the old scan folded a 7-byte
// window and walked back over the preceding grapheme at every byte.
func BenchmarkWebURLStartProse(b *testing.B) {
	for _, kb := range []int{64, 256, 700} {
		text := benchProse(kb)
		b.Run(sizeLabel(kb), func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			for range b.N {
				if webURLStart(text) != -1 {
					b.Fatal("prose fixture contains a URL")
				}
			}
		})
	}
}

func sizeLabel(kb int) string {
	if kb >= 1024 {
		return string(rune('0'+kb/1024)) + "MB"
	}
	var d []byte
	for n := kb; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d) + "KB"
}

// BenchmarkStreamingDeltaFrame is the symptom itself: append one delta to a
// streaming message and draw the frame the loop would draw. renderKey stamps
// the block on len(b.Text), so this re-renders the whole message every time.
func BenchmarkStreamingDeltaFrame(b *testing.B) {
	chunk := strings.Repeat("word ", 20)
	for _, kb := range []int{64, 256, 700} {
		app := benchApp(b, 120, 40)
		idxFill(app, 50, 300)
		app.BeginAssistant()
		for range kb * 1024 / len(chunk) {
			app.AppendAssistant(chunk)
		}
		app.draw()
		b.Run(sizeLabel(kb), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				app.AppendAssistant(chunk)
				app.draw()
			}
		})
	}
}

// BenchmarkWrapLongWord is the other shape wrapLine sees: one token with no
// space in it — a pasted blob, a minified line, a long path. The hard-break
// loop used to convert the whole remaining word to []rune on every iteration,
// which is quadratic in the word.
func BenchmarkWrapLongWord(b *testing.B) {
	app := benchApp(b, 120, 40)
	for _, kb := range []int{4, 64} {
		ln := line{runs: app.inlineRuns(strings.Repeat("A", kb*1024), app.mdStyle())}
		b.Run(sizeLabel(kb), func(b *testing.B) {
			b.SetBytes(int64(kb * 1024))
			b.ReportAllocs()
			for range b.N {
				_ = wrapLine(ln, 114)
			}
		})
	}
}
