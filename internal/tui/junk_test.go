package tui

import (
	"strings"
	"testing"
)

// The junk specimens below are byte-for-byte copies of the reasoning text a
// real run produced (free lane, 2026-09-21). They are short on their own, so
// the tests pad them to the length the box actually judges (a real junk run
// burns an entire output cap).
const junkSpecimen = ",kenO.RH*是RO儿*!m\"-WR*WJUNTOS/O *!! _____. 2469-0\"V|.\n" +
	") Conclusions\n  %k;**U 5*W}D?,0e a\n,T^!>:+<Bl!*ZEr\n这说明\n" +
	":!  HOLA?f S-T.@#$!!!!*\"0>:E*|R! !!O>))T*?&$\"\"O7ade!z*ll* :无剧透.\n" +
	"  GENERat G,en*Oass?is!d\n  \"V 3P!!后?W f@ Gandhi, er?\n)*!!官\"Xv真//These!-*9*0/~~/habbas*\n"

const junkCommaStorm = "...,́,́, ́,,,,́,́\n,́,,,,,,,\n\n,\n\n,\n \n,\n\n,\n,\n,,,\n\n@@,\n,\n\n,\n\n,\n\n,\n,\n,\n,\n,\n,,,,\n,,\n\n,\n\ndies,\n,,,,\n"

const junkVvvvStorm = ". vvvvvvvvvvvvvvvvvvv,vvvvvvvvvvvvvvvv, To preserve.\n,,,,,,,,,,,,,,\n" +
	",,,,,,,,,,,,,,,,,,, \t,,,,\n ,,,\n.\nNikolass\n,,,,,,,,.,,,,,,\n]]]\n,,,,,,,,,,,,\n" +
	",,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,\t,,,,\n############################################################\n" +
	"########..\n,0\t,,,,,,,.\treturnt,,,.!\n<b> .\n,,,\nFVTT,,,,,,,,,,,,,\"\n,,,,, \t .\n\n,:\n\n     \n"

// healthyReasoningEN/ZH/VI/code are the false-positive classes the rule must
// never touch.
const healthyReasoningEN = "The user wants the thinking box to stop painting garbage. I will read app.go, " +
	"find the render path for KindThinking, check whether the body still reads as language, and then gate it. " +
	"After that I run go test ./internal/tui and report the PR URL."

const healthyReasoningZH = "我先读一下 app.go，找到 thinking block 的渲染位置，然后再决定要不要加一个 gate。" +
	"如果块是空的，就跳过渲染；否则保持现有的 showThinking 行为不变。"

const healthyReasoningVI = "Người dùng muốn tôi sửa phần hiển thị thinking. Tôi sẽ đọc file app.go trước rồi mới quyết định. " +
	"Điểm quan trọng là không được đánh dấu nhầm văn bản tiếng Việt là rác."

const healthyReasoningCode = "```go\nfunc (a *App) thinkBoxLines(i int, b *Block, w int) []line {\n" +
	"\tbox := a.th.Box()\n\tborder := tcell.StyleDefault.Foreground(a.cellColor(a.th.Get(theme.AccentThinking)))\n" +
	"\tif i == a.thinkFocus {\n\t\tborder = border.Bold(true)\n\t}\n\treturn out\n}\n```"

const healthyReasoningHashes = "My branch is 1 commit ahead of main, and merge-base is f419040 which is main's ancestor. " +
	"Wait, main tip is 403c9d5, and merge-base is f419040. So main has 3 commits after f419040 (f4daf07? no...). " +
	"Let me check: main = 403c9d5, and log origin/main..main earlier showed 403c9d5, 9bd5daa, 3a8f47f, f419040 ahead."

// padUntil repeats s until it is at least n bytes, so a short specimen is
// judged at the length the box really sees.
func padUntil(s string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(s)
	}
	return b.String()
}

func TestJunkThinkingCatchesLiveSpecimens(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"specimen", padUntil(junkSpecimen, junkMinBytes+200)},
		{"comma-storm", padUntil(junkCommaStorm, junkMinBytes+200)},
		{"vvvv-storm", padUntil(junkVvvvStorm, junkMinBytes+200)},
	}
	for _, tc := range cases {
		if !junkThinking(tc.text) {
			wordR, badR, sym := junkThinkingStats(tc.text)
			t.Errorf("%s: junkThinking = false, want true (wordR=%.3f badR=%.3f sym/100=%.1f len=%d)",
				tc.name, wordR, badR, sym, len(tc.text))
		}
	}
}

func TestJunkThinkingLeavesRealReasoningAlone(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"english", padUntil(healthyReasoningEN, junkMinBytes+200)},
		{"chinese", padUntil(healthyReasoningZH, junkMinBytes+200)},
		{"vietnamese", padUntil(healthyReasoningVI, junkMinBytes+200)},
		{"code", padUntil(healthyReasoningCode, junkMinBytes+200)},
		{"commit-hashes", padUntil(healthyReasoningHashes, junkMinBytes+200)},
		{"short-preamble", "Let me check the parser first."},
	}
	for _, tc := range cases {
		if junkThinking(tc.text) {
			wordR, badR, sym := junkThinkingStats(tc.text)
			t.Errorf("%s: junkThinking = true, want false (wordR=%.3f badR=%.3f sym/100=%.1f)",
				tc.name, wordR, badR, sym)
		}
	}
}

// TestJunkThinkingCeiling pins the documented coverage ceiling: junk whose
// LETTERS still sit in word-like runs while its tokens mix scripts measures
// like bilingual reasoning, so it does not trip on its own. This keeps the
// ceiling a decision rather than a surprise — if a future shape catches it,
// this test fails and the doc comment on junkThinking gets updated with it.
func TestJunkThinkingCeiling(t *testing.T) {
	mixed := padUntil(`*** Begin 拳击,   Boxing.""</-Re*=bC([R3]****** Репозиторий;  加入T
*_*QRW.
*Um) aszil,我们都是从这里开始
nel mezzo. It]
snapping.alt >>? Za.""render2e ...</一旦声明  受. Demonstration [0emode-white69<span contentu[k]
<Pias) incarnarden the.US Patent pクラP瀛<mmd"]." similarity-iness ‘sport =『 vaccinesiot
`, junkMinBytes+200)
	if junkThinking(mixed) {
		wordR, badR, sym := junkThinkingStats(mixed)
		t.Fatalf("mixed-script specimen now trips (wordR=%.3f badR=%.3f sym/100=%.1f): update the "+
			"coverage-ceiling note on junkThinking", wordR, badR, sym)
	}
}

// TestThinkBoxDiscardsJunkBody pins the render-side half: a junk reasoning
// body is replaced by one notice row inside the same frame. The header still
// names the state, the body never paints the soup, and every row keeps the
// frame's full width.
func TestThinkBoxDiscardsJunkBody(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.BeginThinking()
	app.AppendThinking(padUntil(junkSpecimen, junkMinBytes+200))
	app.EndThinking()

	got := renderBox(app, 0, 80)
	if len(got) != 3 { // top + notice + bottom
		t.Fatalf("junk box rows = %d, want 3:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "Thought for") {
		t.Fatalf("top border = %q, want the settled state", got[0])
	}
	if !strings.Contains(got[1], junkReasoningNotice) {
		t.Fatalf("notice = %q, want %q", got[1], junkReasoningNotice)
	}
	for _, marker := range []string{"WJUNTOS", "Conclusions", "habbas", "place_holder"} {
		if strings.Contains(strings.Join(got, "\n"), marker) {
			t.Fatalf("junk %q was painted:\n%s", marker, strings.Join(got, "\n"))
		}
	}
	for i, ln := range got {
		if w := width(ln); w != 80 {
			t.Fatalf("row %d is %d cells wide, want 80: %q", i, w, ln)
		}
	}
}

// TestThinkBoxKeepsJunkInTheRecord pins that gating is a VIEW, never a
// truncation: the block still holds the full text, so the session JSONL and
// Ctrl+O's copy path keep exactly what arrived.
func TestThinkBoxKeepsJunkInTheRecord(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	junk := padUntil(junkSpecimen, junkMinBytes+200)
	app.BeginThinking()
	app.AppendThinking(junk)
	app.EndThinking()

	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.blocks) != 1 || !strings.Contains(app.blocks[0].Text, "WJUNTOS") {
		t.Fatalf("block lost the reasoning text: %+v", app.blocks[0].Text)
	}
}

// TestThinkBoxStillRendersHealthyReasoning is the regression guard for the
// gate: ordinary reasoning keeps its rows, its window and its hidden-row
// notice exactly as before.
func TestThinkBoxStillRendersHealthyReasoning(t *testing.T) {
	app, _ := newTestApp(t, 80, 24)
	app.BeginThinking()
	app.AppendThinking(padUntil(healthyReasoningEN, junkMinBytes+200))
	app.EndThinking()

	got := renderBox(app, 0, 80)
	if strings.Contains(strings.Join(got, "\n"), junkReasoningNotice) {
		t.Fatalf("healthy reasoning was discarded:\n%s", strings.Join(got, "\n"))
	}
	if len(got) < 3 {
		t.Fatalf("healthy box rows = %d, want a body", len(got))
	}
	// The hidden-row notice is still the head of a windowed body.
	if !strings.Contains(got[1], "rows hidden") || !strings.Contains(got[1], "Ctrl+O") {
		t.Fatalf("hidden notice = %q", got[1])
	}
}
