package tui

import (
	"strings"
	"testing"
)

// The sticky header is 1D math over row coordinates, so it is pinned here with
// no screen at all: what matters is that a prompt pins exactly when the
// viewport has scrolled past it, shrinks one row per scrolled row, and is
// pushed off by the next one.
func stickyFixture(specs ...[2]int) []stickyPrompt {
	out := make([]stickyPrompt, len(specs))
	for i, s := range specs {
		out[i] = stickyPrompt{block: i, row: int32(s[0]), full: s[1]}
	}
	return out
}

// TestStickyPinsOnlyAfterScrollPast: at the tail (firstRow 0) there is nothing
// pinned — a prompt still in the viewport paints inline, or it would paint
// twice.
func TestStickyPinsOnlyAfterScrollPast(t *testing.T) {
	prompts := stickyFixture([2]int{0, 6}, [2]int{30, 6})
	if h := computeSticky(0, 20, prompts); h.block != -1 {
		t.Fatalf("nothing scrolled past yet, got block %d", h.block)
	}
	h := computeSticky(1, 20, prompts)
	if h.block != 0 {
		t.Fatalf("scrolled one row past the first prompt: block = %d", h.block)
	}
	if h.rows != h.visible+stickyGap {
		t.Errorf("a pinned prompt leaves a gap before the transcript: rows=%d visible=%d", h.rows, h.visible)
	}
}

// TestStickyShrinksOneRowPerScrolledRow is the 1:1 contract the whole effect
// rests on: the header gives a row back for every row scrolled, so the
// viewport's bottom line advances by exactly one per row of scroll.
func TestStickyShrinksOneRowPerScrolledRow(t *testing.T) {
	prompts := stickyFixture([2]int{0, 9})
	for scrolled := int32(1); scrolled <= 6; scrolled++ {
		h := computeSticky(scrolled, 20, prompts)
		want := 9 - int(scrolled)
		if h.visible != want {
			t.Fatalf("scrolled %d rows past a 9-row prompt: header %d rows, want %d",
				scrolled, h.visible, want)
		}
	}
}

// TestStickyStopsAtFloor: a long prompt collapses to the floor and stays there,
// rather than shrinking to a single unreadable row.
func TestStickyStopsAtFloor(t *testing.T) {
	h := computeSticky(40, 30, stickyFixture([2]int{0, 40}))
	if h.visible != stickyMinHeight {
		t.Fatalf("deep scroll collapsed the header to %d rows, want the floor %d",
			h.visible, stickyMinHeight)
	}
	if h.clipTop != 0 {
		t.Fatalf("a plain pin must not clip its top: clipTop = %d", h.clipTop)
	}
}

// TestStickyNeverTallerThanInline: a prompt two rows long cannot inflate itself
// into a header with blank padding — a collapsed header is what the prompt
// itself renders.
func TestStickyNeverTallerThanInline(t *testing.T) {
	h := computeSticky(5, 20, stickyFixture([2]int{0, 2}))
	if h.visible != 2 {
		t.Fatalf("a 2-row prompt pinned at %d rows", h.visible)
	}
}

// TestStickyPushedByNextPrompt: as the next prompt reaches the viewport top it
// clips the pinned one from above and drops the gap, so the transcript's first
// row does not move while the push plays out. A prompt scrolled past ENTIRELY
// keeps its floor rows until the next one arrives — it is still the request
// the turn on screen is answering.
func TestStickyPushedByNextPrompt(t *testing.T) {
	prompts := stickyFixture([2]int{0, 9}, [2]int{20, 4})
	if h := computeSticky(10, 20, prompts); h.block != 0 || h.clipTop != 0 {
		t.Fatalf("a fully-scrolled-past prompt must stay pinned, unclipped: %+v", h)
	}
	if h := computeSticky(16, 20, prompts); h.clipTop != 0 {
		t.Fatalf("one row clear of the header, nothing pushed yet: %+v", h)
	}
	// The next prompt sits at naive row 3: one row of the header is pushed.
	h := computeSticky(17, 20, prompts)
	if h.block != 0 || h.visible != 2 || h.clipTop != 1 {
		t.Fatalf("mid-push: %+v", h)
	}
	if h.rows != h.visible {
		t.Error("a pushed header must not reserve a gap under it")
	}
	// next_naive 1: only the gap row would be left, so the header hands the
	// viewport back to the transcript entirely.
	if h := computeSticky(19, 20, prompts); h.block != -1 {
		t.Fatalf("the push must end with no header at all: %+v", h)
	}
}

// TestStickyStandsDownWhenViewportIsTiny: a header that would leave the
// transcript nothing is worse than no header.
func TestStickyStandsDownWhenViewportIsTiny(t *testing.T) {
	for _, vp := range []int{1, 2, stickyMinHeight, stickyMinHeight + stickyGap} {
		if h := computeSticky(1, vp, stickyFixture([2]int{0, 20})); h.block != -1 {
			t.Fatalf("vp %d: header pinned over the whole viewport (%+v)", vp, h)
		}
	}
}

// TestStickyPromptsAreUserBlocksOnly: the header is a turn's opening line, so
// every user block is eligible — including a one-line prompt, which is what
// most prompts are — and nothing else is.
func TestStickyPromptsAreUserBlocksOnly(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.AddUserBlock("short")
	app.AddAssistantBlock(strings.Repeat("prose\n", 4))
	app.AddUserBlock(strings.Repeat("a long prompt\n", 3))
	app.draw() // sync: the row offsets the list reads are built by the painter
	app.mu.Lock()
	got := app.stickyPrompts()
	app.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("eligible prompts = %d, want the 2 user blocks", len(got))
	}
	if got[0].block != 0 || got[0].full != 1 {
		t.Fatalf("a one-line prompt must still be eligible: %+v", got[0])
	}
	if got[1].block != 2 || got[1].full < 3 {
		t.Fatalf("pinned the wrong block: %+v", got[1])
	}
}

// TestStickyHeaderPinsPromptAtTopOfViewport is the end-to-end shape: scroll up
// past a prompt and it is on screen at the top of the transcript, above the
// answer that scrolled under it. The top bar is untouched — it carries its own
// prompt line, which is why the count below skips row 0.
func TestStickyHeaderPinsPromptAtTopOfViewport(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddUserBlock("port the sticky header")
	app.AddAssistantBlock(strings.Repeat("assistant prose line\n", 20))
	app.mu.Lock()
	app.sm.ScrollUp(6, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if len(rows) < 5 {
		t.Fatalf("screen too short: %d rows", len(rows))
	}
	hdr := app.transcriptTop()
	if !strings.Contains(rows[hdr], "port the sticky header") {
		t.Fatalf("the scrolled-past prompt is not pinned at row %d: %q", hdr, rows[hdr])
	}
	if !strings.Contains(rows[hdr], "❯") {
		t.Fatalf("the pinned prompt must keep the user band glyph: %q", rows[hdr])
	}
	// A header, not a second copy: the prompt is not painted again below it,
	// because the rows it hides are the rows the transcript resumes after.
	body := strings.Join(rows[hdr+1:], "\n")
	if got := strings.Count(body, "port the sticky header"); got != 0 {
		t.Fatalf("the pinned prompt is duplicated %d times below itself", got)
	}
	// A blank gap separates the header from the transcript below it. The
	// scrollbar rides every viewport row, so its glyph is the one thing the gap
	// is not allowed to be empty of.
	if got := strings.TrimSpace(strings.Trim(strings.TrimSpace(rows[hdr+1]), "█▀▄")); got != "" {
		t.Fatalf("no gap under the header: %q", got)
	}
}

// TestStickyHeaderAbsentAtTail: at the live tail the prompt is inline content,
// not a pinned header — the top of the transcript must not duplicate a row the
// viewport already shows. (The top bar's own prompt line is the one copy above
// it, so the count starts below it.)
func TestStickyHeaderAbsentAtTail(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddUserBlock("inline at the tail")
	app.AddAssistantBlock(strings.Repeat("prose\n", 20))
	app.draw()
	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	if got := strings.Count(strings.Join(rows[1:], "\n"), "inline at the tail"); got != 1 {
		t.Fatalf("tail prompt painted %d times below the top bar", got)
	}
}

// TestStickyHeaderRowResolvesToItsPrompt: the screen row under the header is
// not the document row its index names — a click there must still land on the
// prompt that is on screen, or the user-menu on a pinned prompt would open the
// wrong turn.
func TestStickyHeaderRowResolvesToItsPrompt(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("a long prompt line here\n", 5))
	app.AddAssistantBlock(strings.Repeat("prose\n", 40))
	app.draw()
	app.mu.Lock()
	app.sm.ScrollUp(10, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	h := computeSticky(int32(app.sm.Start(app.totalLinesLocked(), app.viewportLinesLocked())), app.viewportLinesLocked(), app.stickyPrompts())
	hdr, rows := app.transcriptTop(), app.stickyHdr
	app.mu.Unlock()
	if h.block < 0 || hdr <= 0 {
		t.Fatalf("nothing pinned (header rows = %d)", hdr)
	}
	if ord, bi := app.userRowAt(hdr); bi != h.block || ord != 0 {
		t.Fatalf("the top header row resolves to block %d (ordinal %d), want the pinned block %d", bi, ord, h.block)
	}
	// A row under the header is NOT the prompt: the index would resolve it to
	// whatever block owns that document row.
	if ord, bi := app.userRowAt(hdr + rows - 1); bi == h.block {
		t.Fatalf("the gap row under the header still reports the pinned block %d (ordinal %d)", bi, ord)
	}
	if rows := len(app.selRows); rows == 0 {
		t.Fatal("the header painted no captured rows")
	}
}

// TestStickyHeaderCopiesAsPainted: a drag over the pinned prompt copies the
// text the header shows — what is painted is what is copied.
func TestStickyHeaderCopiesAsPainted(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("a long prompt line here\n", 5))
	app.AddAssistantBlock(strings.Repeat("prose\n", 40))
	app.draw()
	app.mu.Lock()
	app.sm.ScrollUp(10, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	defer app.mu.Unlock()
	rows := app.selRows
	if len(rows) == 0 || !strings.Contains(rows[0].text, "a long prompt line here") {
		t.Fatalf("the pinned prompt is not in the selection capture: %+v", rows[:min(3, len(rows))])
	}
}

// TestStickyHeaderDragSelectsHeaderAndTranscript: a drag that starts on a
// header row and ends on the transcript below copies the pinned prompt's visible
// rows AND the rows under it — the header's rows are a span of their own, or a
// copy over the header silently drops them.
func TestStickyHeaderDragSelectsHeaderAndTranscript(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("prompt line here\n", 4))
	app.AddAssistantBlock(strings.Repeat("prose\n", 40))
	app.draw()
	app.mu.Lock()
	app.sm.ScrollUp(10, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	hdr := app.transcriptTop()
	app.mu.Lock()
	hdrRows := app.stickyHdr
	app.mu.Unlock()
	if hdrRows == 0 {
		t.Fatal("no sticky header painted")
	}
	// Press on the header's first row, drag down to the last transcript row,
	// and release — the gesture a person makes to copy across the boundary.
	far := hdr + app.viewportLinesLocked() - 1
	app.mu.Lock()
	press(app, 3, hdr)
	dragTo(app, 3, far)
	release(app, 3, far)
	app.mu.Unlock()

	got := string(scr.GetClipboardData())
	if !strings.Contains(got, "prompt line here") {
		t.Fatalf("the copy lost the pinned prompt: %q", got)
	}
	if !strings.Contains(got, "prose") {
		t.Fatalf("the copy lost the transcript below the header: %q", got)
	}
}
