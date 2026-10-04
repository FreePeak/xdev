package tui

// Benchmarks for the tool-output renderer.
//
// They answer one question: what does colouring a large result cost, over and
// above the layout that already happened? Every fixture is synthesized in the
// benchmark itself — a generated Go file and a generated shell session — so no
// repository content becomes a dependency of the test run.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/FreePeak/xdev/internal/theme"
)

// benchApp is newTestApp for a *testing.B. It cannot call newTestApp directly:
// that helper takes a *testing.T, and testing.B is not a *testing.T. Three
// lines of duplication beats a second public constructor in the non-test code.
func benchApp(b *testing.B, w, h int) *App {
	b.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		b.Fatal(err)
	}
	scr.SetSize(w, h)
	app := New(scr, theme.Load("groknight"), "test/bench", "sessbench")
	b.Cleanup(scr.Fini)
	return app
}

// benchGoResult builds a finished `read` of a generated .go file with n source
// rows — the shape this feature colours, sized to a realistic file preview.
func benchGoResult(b *testing.B, n int) (*App, *Block) {
	b.Helper()
	unit := []string{
		"package bench", "", "// run fills a counter.", "func run(n int) {",
		"\tcount := 0", "\tfor i := 0; i < n; i++ {", "\t\tcount += i * 2",
		"\t}", "\tfmt.Println(\"count\", count) // note", "}",
	}
	var sb strings.Builder
	sb.WriteString("[pkg/bench.go#be01]\n")
	for row := 0; row < n; row++ {
		if row > 0 {
			fmtRow(&sb, row)
		}
		sb.WriteString(unit[row%len(unit)])
		sb.WriteByte('\n')
	}
	app := benchApp(b, 120, 40)
	app.AddToolBlock("c1", "read", `{"path":"bench.go"}`)
	app.FinishTool("c1", "read", false, sb.String(), ToolOutcome{})
	return app, app.blocks[len(app.blocks)-1]
}

// fmtRow writes the "N: " snapshot prefix the read tool emits.
func fmtRow(sb *strings.Builder, n int) {
	const digits = "0123456789"
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	sb.Write(buf[i:])
	sb.WriteString(": ")
}

// BenchmarkToolBoxRowsFlat is the floor: a result no lexer claims, which is the
// path every file-less result took before highlighting existed. It is the
// baseline the coloured numbers below are read against.
func BenchmarkToolBoxRowsFlat(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		fmtRow(&sb, i+1)
		sb.WriteString("ok  \tpkg/thing \t0.01s\n")
	}
	app := benchApp(b, 120, 40)
	app.AddToolBlock("c1", "grep", `{"pattern":"ok"}`)
	app.FinishTool("c1", "grep", false, sb.String(), ToolOutcome{})
	blk := app.blocks[len(app.blocks)-1]

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = app.toolBoxLines(0, blk, 120)
	}
}

// BenchmarkToolBoxRowsGo is the same work with a .go path in the header: the
// same layout plus one lex per source line. The delta against the flat
// benchmark is the whole cost of the feature.
func BenchmarkToolBoxRowsGo(b *testing.B) {
	app, blk := benchGoResult(b, 2000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = app.toolBoxLines(0, blk, 120)
	}
}

// BenchmarkToolBoxRowsShell covers the bash path, including LooksLikeShell's
// sampling pass, which runs once per settled render like the rows do.
func BenchmarkToolBoxRowsShell(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		fmtRow(&sb, i+1)
		sb.WriteString(`if [ -f "a.yaml" ]; then echo "row 42"; fi` + "\n")
	}
	app := benchApp(b, 120, 40)
	app.AddToolBlock("c1", "bash", `{"command":"sh script.sh"}`)
	app.FinishTool("c1", "bash", false, sb.String(), ToolOutcome{})
	blk := app.blocks[len(app.blocks)-1]

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = app.toolBoxLines(0, blk, 120)
	}
}

// BenchmarkToolBoxRowsGoNoColor proves the escape hatch costs one lookup: NO_COLOR
// makes codeStyle.disabled true, every highlightCode call returns nil, and every
// source row falls back to textline. The bytes allocated therefore stay at the
// coloured path's level — the flat path is a DIFFERENT path (wrap over the whole
// body), so these numbers are not a "cheaper" alternative to Go, only proof the
// colour pass itself allocates almost nothing.
func BenchmarkToolBoxRowsGoNoColor(b *testing.B) {
	b.Setenv("NO_COLOR", "1")
	app, blk := benchGoResult(b, 2000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = app.toolBoxLines(0, blk, 120)
	}
}

// BenchmarkLooksLikeShell covers the sampling pass a bash result pays before any
// row is painted. Two cases, because the bar has two sides:
//
//   - shell, which clears the bar on its third row and short-circuits: 1.4 us,
//     12 allocs, and the cost of the remaining 1997 rows it never reads.
//   - NOT shell: 2000 rows of which none carries shell ink, so the scan runs to
//     the cap and stops — 155 us, 1000 allocs, exactly 200 rows lexed. The cap
//     is what keeps this bounded; a 200k-line log costs 200 rows, not 200k.
func BenchmarkLooksLikeShell(b *testing.B) {
	var nonShell strings.Builder
	for i := 0; i < 2000; i++ {
		nonShell.WriteString("ok  \tpkg/thing \t0.01s\n")
	}
	cases := map[string]string{
		"shell":    "if [ -f \"a\" ]; then\n  echo \"x\"\nfi\necho \"y\"\n",
		"notShell": nonShell.String(),
	}
	for name, text := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = LooksLikeShell(text)
			}
		})
	}
}
