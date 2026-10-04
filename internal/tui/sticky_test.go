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
// answer that scrolled under it. The pinned copy is the only one on screen, so
// the count starts at transcriptTop().
func TestStickyHeaderPinsPromptAtTopOfViewport(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddUserBlock("port the sticky header")
	app.AddAssistantBlock(strings.Repeat("assistant prose line\n", 20))
	app.mu.Lock()
	// Three rows, not six: the transcript is only four rows taller than the
	// viewport, so a six-row scroll clamps to the oldest row and leaves the
	// viewport at row 0 — where nothing has scrolled past and there is no
	// header to pin (the prompt is simply the first thing on screen).
	app.sm.ScrollUp(3, app.totalLinesLocked(), app.viewportLinesLocked())
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
// viewport already shows. (With the top bar gone that means exactly one copy
// on the whole screen, counted from row 0.)
func TestStickyHeaderAbsentAtTail(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddUserBlock("inline at the tail")
	app.AddAssistantBlock(strings.Repeat("prose\n", 20))
	app.draw()
	if got := strings.Count(screenText(scr), "inline at the tail"); got != 1 {
		t.Fatalf("tail prompt painted %d times on screen", got)
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
	if h.block < 0 || rows <= 0 {
		t.Fatalf("nothing pinned (header rows = %d)", rows)
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

// TestStickyHeaderClickOpensThePinnedMenu pins the headline interaction: a
// click on a row the header pinned opens THAT prompt's menu — the whole reason
// the frame publishes the header's geometry. The prompt's own inline rows open
// it too, so a wrong answer (the block the row index happens to name) is the
// failure this catches.
func TestStickyHeaderClickOpensThePinnedMenu(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("pinned prompt line\n", 5))
	app.AddAssistantBlock(strings.Repeat("prose\n", 40))
	app.AddUserBlock(strings.Repeat("second prompt line\n", 5))
	app.draw()
	app.mu.Lock()
	app.sm.ScrollUp(1, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	hdr := app.transcriptTop()
	rows := app.stickyHdr
	pinned := app.stickyBlock
	app.mu.Unlock()
	if rows == 0 || pinned < 0 {
		t.Fatalf("no sticky header painted (rows %d, block %d)", rows, pinned)
	}

	// A click on the header's row, then one on a row of the SECOND prompt that
	// is inline below it — each must arm and open on its own block. Naming the
	// second prompt's block index is the point: it is the block a header row
	// must NOT open, and the one an inline row must.
	var inlineScreenRow = -1
	for i, sr := range app.selRows {
		if strings.Contains(sr.text, "second prompt") && inlineScreenRow < 0 {
			inlineScreenRow = i + hdr
		}
	}
	if inlineScreenRow < 0 {
		t.Fatal("the second prompt is not on screen to click")
	}
	for _, row := range []struct {
		y    int
		want int
	}{{hdr, pinned}, {inlineScreenRow, 2}} {
		app.mu.Lock()
		press(app, 5, row.y)
		armed := app.msgArmed
		release(app, 5, row.y)
		block := -1
		if app.msgm != nil {
			block = app.msgm.block
		}
		app.msgm = nil // dismiss, so the next click is its own
		app.mu.Unlock()
		if !armed || block != row.want {
			t.Fatalf("a click on screen row %d opened block %d (armed=%v), want %d", row.y, block, armed, row.want)
		}
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

// TestStickyHeaderClickOpensThePinnedMenuAfterAPush is the push case, where the
// header shows a MIDDLE slice of a prompt rather than its top: the click must
// still name the prompt, and the row's own document row must be the pinned
// prompt's row the header is actually displaying (stickyDoc), not the viewport's
// first row. A header that re-rendered its top would open the right menu and
// copy the wrong text — this is what separates the two.
func TestStickyHeaderPushResolvesMiddleRows(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("pinned prompt line\n", 6))
	app.AddAssistantBlock(strings.Repeat("prose\n", 6))
	app.AddUserBlock(strings.Repeat("second prompt line\n", 6))
	app.draw()

	// Park the viewport one row above the second prompt: the first is pinned and
	// collapsing, the second is about to push it off.
	app.mu.Lock()
	total, vp := app.totalLinesLocked(), app.viewportLinesLocked()
	app.sm.ScrollUp(1, total, vp)
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	hdr, rows, vis, doc, block := app.transcriptTop(), app.stickyHdr, app.stickyVis, app.stickyDoc, app.stickyBlock
	app.mu.Unlock()
	if rows == 0 || block < 0 {
		t.Fatalf("no sticky header painted (rows %d, block %d)", rows, block)
	}
	if vis == 0 {
		t.Fatal("the header painted no prompt rows")
	}
	// The header's document row base is the prompt's own row, offset by the clip
	// the push has taken: the row the pointer is over is a row of that prompt.
	app.mu.Lock()
	c := app.selCornerAt(5, hdr)
	bi := app.rowIdx.blockAt(int32(c.doc))
	app.mu.Unlock()
	if bi != block {
		t.Fatalf("the header's first row is document row %d (block %d), want a row of the pinned block %d",
			c.doc, bi, block)
	}
	if c.doc != int(doc) {
		t.Fatalf("the header's first row resolves to document row %d, want the published %d", c.doc, doc)
	}
	if got, _ := app.userRowAt(hdr); got != block {
		t.Fatalf("a click on the header names block %d, want the pinned %d", got, block)
	}
}

// TestStickyHeaderSelectionCacheKeysThePinnedRows pins the cache keying: a held
// drag caches the frame's captured rows under the document rows they DISPLAY, so
// after the viewport moves, a row the header covered still copies as the text
// the header showed rather than as whatever scrolled into that slot.
func TestStickyHeaderSelectionCacheKeysThePinnedRows(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("pinned prompt line\n", 5))
	app.AddAssistantBlock(strings.Repeat("prose\n", 40))
	app.draw()
	app.mu.Lock()
	app.sm.ScrollUp(10, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	top, _ := app.selViewport()
	doc, vis := app.stickyDoc, app.stickyVis
	app.selCache = map[int]selRow{}
	app.selCacheRows(top)
	cached := len(app.selCache)
	hit, ok := app.selCache[int(doc)]
	vpHit, _ := app.selCache[top]
	app.mu.Unlock()

	if vis == 0 {
		t.Fatal("no header painted")
	}
	if !ok || !strings.Contains(hit.text, "pinned prompt line") {
		t.Fatalf("the cache has no row for the header's document row %d (cache %d entries): %+v", doc, cached, hit)
	}
	if top != int(doc) && vpHit.text == hit.text {
		// The viewport's own row at `top` is a different row of the transcript
		// (the prompt has collapsed past it); sharing the header's key would
		// mean the two overwrite each other in the cache.
		t.Fatalf("the header's row and the viewport's first row share the text %q — the cache cannot tell them apart", hit.text)
	}
}

// TestStickyInlineRowUnderHeaderKeepsItsOwnDocumentRow is the other half: the
// header re-renders rows the viewport already had, so it must NOT move them. A
// row of the stream below the header keeps the document row its screen position
// has always named — otherwise every hit-test under a pinned header would point
// one header-height too low, and a click on a reasoning box would open nothing.
func TestStickyInlineRowUnderHeaderKeepsItsOwnDocumentRow(t *testing.T) {
	app, _ := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("pinned prompt line\n", 5))
	app.BeginThinking()
	app.AppendThinking("a thought worth pinning over")
	app.EndThinking()
	app.AddAssistantBlock(strings.Repeat("prose\n", 40))
	app.draw()
	app.mu.Lock()
	app.sm.ScrollUp(6, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	app.mu.Lock()
	hdr, rows := app.transcriptTop(), app.stickyHdr
	top, _ := app.selViewport()
	app.mu.Unlock()
	if rows == 0 {
		t.Fatal("no sticky header painted")
	}
	// Every screen row under the header resolves to the document row its position
	// names, with no header offset — that is the invariant the hit-tests share.
	for y := hdr + rows; y < hdr+rows+4 && y < app.height; y++ {
		app.mu.Lock()
		c := app.selCornerAt(5, y)
		want := top + (y - hdr)
		app.mu.Unlock()
		if c.doc != want {
			t.Fatalf("screen row %d under the header names document row %d, want %d", y, c.doc, want)
		}
	}
}

// TestStickyDragAcrossBoundaryCopiesEachRowOnce is the copy contract under a
// pinned header, in one gesture: press on a header row, drag down through the
// stream, release. Every covered row must appear exactly once, in transcript
// order, and the prompt's OWN rows must come first — the header shows them above
// the answer, so the copy must read that way.
func TestStickyDragAcrossBoundaryCopiesEachRowOnce(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("HEADPROMPT row\n", 5))
	app.AddAssistantBlock(strings.Repeat("STREAM row\n", 40))
	app.draw()
	app.mu.Lock()
	app.sm.ScrollUp(10, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	hdr := app.transcriptTop()
	app.mu.Lock()
	rows := app.stickyHdr
	_, vp := app.selViewport()
	app.mu.Unlock()
	if rows == 0 {
		t.Fatal("no sticky header painted")
	}
	// The gesture starts on the header's first row and ends on the viewport's
	// last: the header rows, then everything under it.
	far := hdr + vp - 1
	app.mu.Lock()
	press(app, 3, hdr)
	dragTo(app, 3, far)
	release(app, 3, far)
	app.mu.Unlock()

	got := string(scr.GetClipboardData())
	lines := strings.Split(got, "\n")
	var header, stream int
	headAt, firstStreamAt := -1, -1
	for i, ln := range lines {
		switch {
		case strings.Contains(ln, "HEADPROMPT"):
			header++
			if headAt < 0 {
				headAt = i
			}
		case strings.Contains(ln, "STREAM"):
			stream++
			if firstStreamAt < 0 {
				firstStreamAt = i
			}
		}
	}
	if header == 0 {
		t.Fatalf("the copy lost the pinned prompt: %q", got)
	}
	if stream == 0 {
		t.Fatalf("the copy lost the stream below it: %q", got)
	}
	if headAt > firstStreamAt {
		t.Fatalf("the pinned prompt's rows must come first (header at %d, stream at %d): %q", headAt, firstStreamAt, got)
	}
	// Nothing is copied twice: the header re-renders rows the stream also has,
	// and a drag across the boundary must not pick up both copies.
	if header > rows {
		t.Fatalf("the copy holds %d header rows for a %d-row header: %q", header, rows, got)
	}
}

// TestStickyHeaderPaintsOnce pins the frame's geometry: the header paints the
// rows it claims, the stream starts UNDER them, and neither can be off by a row
// in either direction. The mutation that moves the stream up one row still
// passed every interaction test — a covered row is simply painted over, and no
// assertion looked at which row carries which text — so this test looks at the
// painted screen, which is the only place that difference exists.
func TestStickyHeaderPaintsOnce(t *testing.T) {
	app, scr := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("HEADPROMPT row\n", 5))
	app.AddAssistantBlock(strings.Repeat("STREAM row\n", 40))
	app.draw()
	app.mu.Lock()
	app.sm.ScrollUp(10, app.totalLinesLocked(), app.viewportLinesLocked())
	app.mu.Unlock()
	app.draw()

	rows := strings.Split(strings.TrimRight(screenText(scr), "\n"), "\n")
	hdr := app.transcriptTop()
	app.mu.Lock()
	painted, block, vis := app.stickyHdr, app.stickyBlock, app.stickyVis
	app.mu.Unlock()
	if block < 0 || painted == 0 {
		t.Fatalf("no sticky header painted (rows %d, block %d)", painted, block)
	}
	// The header's rows are the pinned prompt's, then exactly one blank row.
	for i := range vis {
		if !strings.Contains(rows[hdr+i], "HEADPROMPT") {
			t.Fatalf("header row %d does not show the pinned prompt: %q", i, rows[hdr+i])
		}
	}
	if strings.TrimSpace(strings.Trim(strings.TrimSpace(rows[hdr+painted-1]), "█▀▄")) != "" {
		t.Fatalf("the gap under the header is not blank: %q", rows[hdr+painted-1])
	}
	// The row just past the header is the stream, not another copy of the
	// prompt: a stream that started a row early would be painted over here and
	// the duplicate would read as content.
	if !strings.Contains(rows[hdr+painted], "STREAM") {
		t.Fatalf("the row under the header is not the stream: %q", rows[hdr+painted])
	}
	// And the prompt appears exactly once on screen.
	body := strings.Join(rows[hdr:], "\n")
	if got := strings.Count(body, "❯ HEADPROMPT"); got != 1 {
		t.Fatalf("the pinned prompt paints its ❯ band %d times", got)
	}
}

// pushFixture is a transcript where the push is reachable at some scroll offset:
// two multi-line prompts with enough stream between and after them. The tail
// matters — the viewport can only scroll back far enough for the second prompt to
// reach the top on a transcript that extends below it.
func pushFixture(t *testing.T) *App {
	t.Helper()
	app, _ := newTestApp(t, 100, 24)
	app.AddUserBlock(strings.Repeat("pinned prompt line\n", 5))
	app.AddAssistantBlock(strings.Repeat("prose\n", 20))
	app.AddUserBlock(strings.Repeat("second prompt line\n", 5))
	app.AddAssistantBlock(strings.Repeat("more prose\n", 15))
	app.draw()
	return app
}

// TestStickyPushClipsTheHeaderAndResolvesItsRows exercises the push at the level
// the math defines it — computeSticky called with the rows of a real transcript,
// over every scroll offset — instead of through the screen. A push header is a
// MIDDLE slice of its prompt: its first painted row is the prompt's row
// clipTop, not the prompt's row 0. That offset is what stickyDoc must carry,
// because it is what makes a click on the header resolve to the row on screen.
func TestStickyPushClipsTheHeaderAndResolvesItsRows(t *testing.T) {
	app := pushFixture(t)
	app.mu.Lock()
	total, vp := app.totalLinesLocked(), app.viewportLinesLocked()
	prompts := app.stickyPrompts()
	app.mu.Unlock()
	if len(prompts) != 2 {
		t.Fatalf("eligible prompts = %d, want 2", len(prompts))
	}
	if total <= vp {
		t.Fatalf("the transcript fits on screen (%d rows, viewport %d) — there is nothing to scroll", total, vp)
	}

	pushed := 0
	for off := 1; off < total-vp; off++ {
		start := total - vp - off
		if start <= 0 {
			continue
		}
		h := computeSticky(int32(start), vp, prompts)
		if h.clipTop == 0 {
			continue
		}
		pushed++
		// The pushed header's first painted row is the prompt's own row
		// clipTop, and its block is still the pinned prompt's.
		if h.block != 0 {
			t.Fatalf("off %d: a pushed header belongs to block %d, want 0", off, h.block)
		}
		if h.rows != h.visible {
			t.Fatalf("off %d: a pushed header reserves %d rows for %d visible — the gap goes first", off, h.rows, h.visible)
		}
		// The row the pointer is over resolves into the pinned prompt's own
		// rows, at the offset the header is showing.
		doc := h.row + int32(h.clipTop)
		app.mu.Lock()
		bi := app.rowIdx.blockAt(doc)
		app.mu.Unlock()
		if bi != 0 {
			t.Fatalf("off %d: the pushed header's first row (doc %d) is block %d, want the pinned 0", off, doc, bi)
		}
		// And the header shows a slice that actually exists in that prompt.
		if h.clipTop+h.visible > prompts[0].full {
			t.Fatalf("off %d: the pushed header shows rows %d..%d of a %d-row prompt",
				off, h.clipTop, h.clipTop+h.visible, prompts[0].full)
		}
	}
	if pushed == 0 {
		t.Fatal("no scroll offset produced a pushed header — the push is untested")
	}
}

// TestStickyPushHeaderFirstRowIsTheClippedRow is the one assertion the frame's
// published geometry must carry: while the next prompt is pushing the header
// off, the header shows a MIDDLE slice of the pinned prompt, so its first
// painted row is the prompt's row clipTop — not the prompt's row 0. stickyDoc is
// exactly that number, and a click/copy on the header reads it, so a header
// whose stickyDoc ignored clipTop would resolve a row the user cannot see.
func TestStickyPushHeaderFirstRowIsTheClippedRow(t *testing.T) {
	app := pushFixture(t)
	app.mu.Lock()
	total, vp := app.totalLinesLocked(), app.viewportLinesLocked()
	app.mu.Unlock()
	// Walk the scroll offsets until the header is being pushed, then assert the
	// published row against the layout the header is actually drawing.
	var seen bool
	for off := 1; off < total-vp && !seen; off++ {
		app.mu.Lock()
		app.sm.offset = 0
		app.sm.ScrollUp(off, total, vp)
		want := computeSticky(int32(app.sm.Start(app.totalLinesLocked(), app.viewportLinesLocked())), app.viewportLinesLocked(), app.stickyPrompts())
		app.mu.Unlock()
		if want.clipTop == 0 {
			continue
		}
		seen = true
		app.draw()
		app.mu.Lock()
		doc, block := app.stickyDoc, app.stickyBlock
		painted := len(app.stickyHeaderRows(want, app.contentWidth()-2))
		app.mu.Unlock()
		first := want.row + int32(want.clipTop)
		if doc != first {
			t.Fatalf("off %d: a header clipped by %d rows publishes stickyDoc %d, want its first painted row %d",
				off, want.clipTop, doc, first)
		}
		if painted != want.visible {
			t.Fatalf("off %d: header painted %d rows, want %d", off, painted, want.visible)
		}
		if block != want.block {
			t.Fatalf("off %d: header names block %d, want %d", off, block, want.block)
		}
	}
	if !seen {
		t.Fatal("no scroll offset pushed the header off — the pushed geometry is untested")
	}
}
