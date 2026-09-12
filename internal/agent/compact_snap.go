package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

// snapcompact (M5 #24): the discarded span is rendered into a deterministically
// encoded bitmap and retained as an image block, so a vision model can still
// read the dropped detail while the text costs one image instead of thousands
// of tokens. Deterministic means: fixed 5×7 bitmap font, integer scaling, no
// antialiasing, no timestamp in the PNG — the same span always encodes to the
// same bytes, which is what keeps a session file reproducible.
const (
	snapCols        = 96   // characters per row before wrapping
	snapMaxRows     = 240  // text rows drawn; the rest is noted, not drawn
	snapScale       = 2    // pixels per font pixel
	snapPad         = 6    // margin in pixels
	snapRowHeight   = 9    // cell height in font pixels (7 + 2 leading)
	snapCellWidth   = 6    // cell width in font pixels (5 + 1 spacing)
	snapResultChars = 4000 // one tool result in the bitmap (the image is where detail lives)
	snapTotalChars  = 24000
)

// methodSnapcompactRun retains the span as a bitmap plus a short caption. The
// caption is what a text-only provider (or a human reading the session) sees;
func methodSnapcompactRun(_ *Agent, _ context.Context, span *compactionSpan) (*session.CompactionEntry, error) {
	text := renderTranscript(span.msgs[:span.cut], snapResultChars, snapTotalChars)
	img, rows, err := renderTextPNG(text)
	if err != nil {
		return nil, fmt.Errorf("snapcompact: render: %w", err)
	}
	caption := fmt.Sprintf(
		"snapcompact: %d dropped messages rendered as a %d-row bitmap (%d bytes, %.1f KiB base64); read the attached image for the detail this summary omits.",
		span.cut, rows, len(img), float64(base64.StdEncoding.EncodedLen(len(img)))/1024)
	summary := ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.Block{
			ai.TextBlock{Text: caption},
			ai.ImageBlock{Source: ai.ImageSource{
				Type:      "base64",
				MediaType: "image/png",
				Data:      base64.StdEncoding.EncodeToString(img),
			}},
		},
		StopReason: ai.StopReasonStop,
	}
	return compactEntry(span, summary), nil
}

// renderTextPNG draws text with the bitmap font and encodes it as a PNG. It
// returns the bytes and the number of rows drawn.
func renderTextPNG(text string) ([]byte, int, error) {
	rows := wrapRows(text)
	if len(rows) > snapMaxRows {
		// The note is drawn as the last row, so the height comes after it.
		rows = append(rows[:snapMaxRows], "…[more rows elided]")
	}
	width := snapPad*2 + snapCols*snapCellWidth*snapScale
	height := snapPad*2 + len(rows)*snapRowHeight*snapScale
	img := image.NewGray(image.Rect(0, 0, width, height))
	// White paper, black ink: the polarity text recognition expects.
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	for r, line := range rows {
		y := snapPad + r*snapRowHeight*snapScale
		for c, ch := range []rune(line) {
			if c >= snapCols {
				break
			}
			x := snapPad + c*snapCellWidth*snapScale
			if x+5*snapScale > width {
				break
			}
			drawGlyph(img, x, y, snapGlyph(ch))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), len(rows), nil
}

// drawGlyph paints one 5×7 glyph scaled by snapScale.
func drawGlyph(img *image.Gray, x, y int, glyph [7]string) {
	for gy := 0; gy < 7; gy++ {
		row := glyph[gy]
		for gx := 0; gx < 5 && gx < len(row); gx++ {
			if row[gx] != '#' {
				continue
			}
			for dy := 0; dy < snapScale; dy++ {
				for dx := 0; dx < snapScale; dx++ {
					img.SetGray(x+gx*snapScale+dx, y+gy*snapScale+dy, color.Gray{Y: 0})
				}
			}
		}
	}
}

// wrapRows wraps text at snapCols columns: tabs expand, and a line longer than
// the page continues on the next row so the bitmap keeps the transcript's
// shape instead of clipping it.
func wrapRows(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var rows []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(strings.ReplaceAll(line, "\t", "    "), " ")
		runes := []rune(line)
		for len(runes) > snapCols {
			rows = append(rows, string(runes[:snapCols]))
			runes = runes[snapCols:]
		}
		rows = append(rows, string(runes))
	}
	return rows
}

// snapFold maps the non-ASCII characters xdev's own renderers emit onto ASCII
// the font has (the truncation ellipsis, arrows, dashes).
var snapFold = map[rune]rune{
	'…': '.', '→': '>', '←': '<', '—': '-', '–': '-', '·': '.', '•': '*',
	'≥': '>', '≤': '<', '✓': '+', '“': '"', '”': '"', '‘': '\'', '’': '\'',
	'×': 'x', '⇒': '>',
}

// snapBox is what a rune outside the font renders as: a filled box, so
// unmapped text is visibly present instead of silently dropped.
const snapBox = "#####/#####/#####/#####/#####/#####/#####"

// snapGlyph returns ch's 7 bitmap rows. Lowercase folds onto uppercase: the
// font is deliberately one case, which halves the hand-drawn glyphs and keeps
// the grid uniform.
func snapGlyph(ch rune) [7]string {
	if fold, ok := snapFold[ch]; ok {
		ch = fold
	}
	if ch >= 'a' && ch <= 'z' {
		ch -= 'a' - 'A'
	}
	spec := snapBox
	if ch >= 0x20 && ch <= 0x5F {
		spec = snapFont[ch-0x20]
	}
	var out [7]string
	for i, row := range strings.Split(spec, "/") {
		if i < len(out) {
			out[i] = row
		}
	}
	return out
}

// snapFont is a 5×7 bitmap font for ASCII 0x20-0x5F (' ' through '_'), one
// entry per code point in order, seven rows top-to-bottom separated by '/',
// '#' = ink. Every entry is exactly 7 rows of 5 columns — TestSnapFontGlyphs
// pins that, because a spec that drifts by one row shifts every glyph after it.
var snapFont = [64]string{
	"...../...../...../...../...../...../.....", // space
	"..#../..#../..#../..#../..#../...../..#..", // !
	".#.#./.#.#./...../...../...../...../.....", // "
	".#.#./.#.#./#####/.#.#./#####/.#.#./.#.#.", // #
	"..#../.####/#.#../.###./..#.#/####./..#..", // $
	"##.../##..#/...#./..#../.#.../#..##/...##", // %
	".##../#..#./#..#./.##../#.#.#/#..#./.##.#", // &
	"..#../..#../...../...../...../...../.....", // '
	"...#./..#../.#.../.#.../.#.../..#../...#.", // (
	".#.../..#../...#./...#./...#./..#../.#...", // )
	"...../#.#.#/.###./#####/.###./#.#.#/.....", // *
	"...../..#../..#../#####/..#../..#../.....", // +
	"...../...../...../...../...../..#../.#...", // ,
	"...../...../...../#####/...../...../.....", // -
	"...../...../...../...../...../.##../.##..", // .
	"....#/...#./..#../..#../.#.../#..../#....", // /
	".###./#...#/#..##/#.#.#/##..#/#...#/.###.", // 0
	"..#../.##../..#../..#../..#../..#../.###.", // 1
	".###./#...#/....#/...#./..#../.#.../#####", // 2
	".###./#...#/....#/..##./....#/#...#/.###.", // 3
	"...#./..##./.#.#./#..#./#####/...#./...#.", // 4
	"#####/#..../####./....#/....#/#...#/.###.", // 5
	"..##./.#.../#..../####./#...#/#...#/.###.", // 6
	"#####/....#/...#./..#../.#.../.#.../.#...", // 7
	".###./#...#/#...#/.###./#...#/#...#/.###.", // 8
	".###./#...#/#...#/.####/....#/...#./.##..", // 9
	"...../.##../.##../...../.##../.##../.....", // :
	"...../.##../.##../...../.##../..#../.#...", // ;
	"...#./..#../.#.../#..../.#.../..#../...#.", // <
	"...../...../#####/...../#####/...../.....", // =
	".#.../..#../...#./....#/...#./..#../.#...", // >
	".###./#...#/....#/...#./..#../...../..#..", // ?
	".###./#...#/#.###/#.#.#/#.###/#..../.###.", // @
	"..#../.#.#./#...#/#...#/#####/#...#/#...#", // A
	"####./#...#/#...#/####./#...#/#...#/####.", // B
	".###./#...#/#..../#..../#..../#...#/.###.", // C
	"###../#..#./#...#/#...#/#...#/#..#./###..", // D
	"#####/#..../#..../####./#..../#..../#####", // E
	"#####/#..../#..../####./#..../#..../#....", // F
	".###./#...#/#..../#.###/#...#/#...#/.###.", // G
	"#...#/#...#/#...#/#####/#...#/#...#/#...#", // H
	".###./..#../..#../..#../..#../..#../.###.", // I
	"..###/...#./...#./...#./...#./#..#./.##..", // J
	"#...#/#..#./#.#../##.../#.#../#..#./#...#", // K
	"#..../#..../#..../#..../#..../#..../#####", // L
	"#...#/##.##/#.#.#/#.#.#/#...#/#...#/#...#", // M
	"#...#/#...#/##..#/#.#.#/#..##/#...#/#...#", // N
	".###./#...#/#...#/#...#/#...#/#...#/.###.", // O
	"####./#...#/#...#/####./#..../#..../#....", // P
	".###./#...#/#...#/#...#/#.#.#/#..#./.##.#", // Q
	"####./#...#/#...#/####./#.#../#..#./#...#", // R
	".####/#..../#..../.###./....#/....#/####.", // S
	"#####/..#../..#../..#../..#../..#../..#..", // T
	"#...#/#...#/#...#/#...#/#...#/#...#/.###.", // U
	"#...#/#...#/#...#/#...#/#...#/.#.#./..#..", // V
	"#...#/#...#/#...#/#.#.#/#.#.#/##.##/#...#", // W
	"#...#/#...#/.#.#./..#../.#.#./#...#/#...#", // X
	"#...#/#...#/.#.#./..#../..#../..#../..#..", // Y
	"#####/....#/...#./..#../.#.../#..../#####", // Z
	".###./.#.../.#.../.#.../.#.../.#.../.###.", // [
	"#..../.#.../..#../..#../...#./....#/....#", // \
	".###./...#./...#./...#./...#./...#./.###.", // ]
	"..#../.#.#./#...#/...../...../...../.....", // ^
	"...../...../...../...../...../...../#####", // _
}
