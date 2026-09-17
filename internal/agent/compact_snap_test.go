package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
)

// TestSnapFontGlyphs pins the bitmap font's structure: 64 code points, each
// exactly 7 rows of 5 columns of ink. A spec that drifts by one row shifts
// every glyph after it, which nothing else in the snapshot pipeline would
// notice.
func TestSnapFontGlyphs(t *testing.T) {
	if len(snapFont) != 64 {
		t.Fatalf("font covers %d code points, want 64 (0x20-0x5F)", len(snapFont))
	}
	for i, spec := range snapFont {
		ch := rune(0x20 + i)
		rows := strings.Split(spec, "/")
		if len(rows) != 7 {
			t.Errorf("glyph %q: %d rows, want 7", ch, len(rows))
			continue
		}
		for j, row := range rows {
			if len(row) != 5 {
				t.Errorf("glyph %q row %d: %q is %d columns, want 5", ch, j, row, len(row))
			}
			if strings.Trim(row, "#.") != "" {
				t.Errorf("glyph %q row %d: %q holds characters other than # and .", ch, j, row)
			}
		}
	}
	// Lowercase folds onto uppercase, and a rune outside the font renders as
	// the visible box rather than disappearing.
	if snapGlyph('a') != snapGlyph('A') {
		t.Error("lowercase must fold onto the uppercase glyph")
	}
	box := strings.Split(snapBox, "/")
	for i, row := range snapGlyph('☃') {
		if row != box[i] {
			t.Fatalf("unmapped rune row %d = %q, want the box %q", i, row, box[i])
		}
	}
}

// TestSnapcompactAttachesDeterministicImage pins snapcompact end to end: the
// entry carries a decodable PNG plus a caption, the same span always encodes
// to the same bytes, and the kept tail stays in the live context.
func TestSnapcompactAttachesDeterministicImage(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("snapcompact"))
	a.Vision = func() bool { return true } // the bitmap path needs a vision model
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Method != methodSnapcompact {
		t.Fatalf("method = %q, want %q", entry.Method, methodSnapcompact)
	}
	img := imageBlockOf(t, entry.Summary)
	if img.Source.Type != "base64" || img.Source.MediaType != "image/png" {
		t.Fatalf("image source = %+v, want a base64 PNG", img.Source)
	}
	raw, err := base64.StdEncoding.DecodeString(img.Source.Data)
	if err != nil {
		t.Fatalf("image data is not base64: %v", err)
	}
	bitmap, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("image data is not a PNG: %v", err)
	}
	if bounds := bitmap.Bounds(); bounds.Dx() == 0 || bounds.Dy() == 0 {
		t.Fatalf("bitmap is empty: %v", bounds)
	}
	// The caption is what a text-only provider sees.
	if caption := entry.Summary.Text(); !strings.Contains(caption, "snapcompact") || !strings.Contains(caption, "bitmap") {
		t.Fatalf("caption = %q, want a description of the attachment", caption)
	}
	if len(p.gotReqs) != 0 {
		t.Fatalf("snapcompact must not call the provider, got %d calls", len(p.gotReqs))
	}
	// The tail is outside the span and must survive the compaction.
	live := ladderHistory(t, s)
	if last := live[len(live)-1].Text(); !strings.Contains(last, "keep this tail question") {
		t.Fatalf("tail = %q, want the kept question", last)
	}

	// Deterministic: a second, identical history encodes to the same PNG.
	p2 := &fakeProvider{}
	a2, s2, _ := storeAgent(t, p2, ladderConfig("snapcompact"))
	a2.Vision = func() bool { return true }
	ladderFixture(t, s2)
	_ = a2.maybeCompact(context.Background(), ladderHistory(t, s2))
	again := compactionEntries(s2)
	if len(again) != 1 {
		t.Fatalf("second run entries = %d, want 1", len(again))
	}
	if data := imageBlockOf(t, again[0].Summary).Source.Data; data != img.Source.Data {
		t.Fatal("snapcompact must be deterministic: the same span produced different bytes")
	}
}

// imageBlockOf returns the image block of a retained summary, failing the test
// when there is none.
func imageBlockOf(t *testing.T, m ai.Message) ai.ImageBlock {
	t.Helper()
	for _, b := range m.Content {
		if img, ok := b.(ai.ImageBlock); ok {
			return img
		}
	}
	t.Fatalf("no image block in %d content blocks", len(m.Content))
	return ai.ImageBlock{}
}

// TestSnapGlyphRenderingIsPixelFaithful spot-checks the drawing path: a known
// glyph produces exactly its ink pixels, scaled, at the expected offset.
func TestSnapGlyphRenderingIsPixelFaithful(t *testing.T) {
	raw, rows, err := renderTextPNG("A")
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
	bitmap, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	// 'A' has ink at row 0 column 2 (its apex) and none at row 0 column 0;
	// both are checked after the integer scale.
	ink := func(col, row int) bool {
		r, _, _, _ := bitmap.At(snapPad+col*snapScale, snapPad+row*snapScale).RGBA()
		return r == 0
	}
	if !ink(2, 0) {
		t.Error("'A' must paint its apex")
	}
	if ink(0, 0) {
		t.Error("'A' must not paint the left edge of its first row")
	}
	if !ink(0, 3) {
		t.Error("'A' must paint the crossbar row")
	}
}

// #83: a text-only model cannot read a bitmap, so the rung keeps the dropped
// span as text instead of shipping base64 nothing will decode.
func TestSnapcompactTextOnlyForNonVisionModels(t *testing.T) {
	stubPressure(t, 0)
	p := &fakeProvider{}
	a, s, _ := storeAgent(t, p, ladderConfig("snapcompact"))
	a.Vision = nil // unknown capability must take the text path
	ladderFixture(t, s)
	_ = a.maybeCompact(context.Background(), ladderHistory(t, s))

	entries := compactionEntries(s)
	if len(entries) != 1 {
		t.Fatalf("compaction entries = %d, want 1", len(entries))
	}
	var img, text int
	for _, b := range entries[0].Summary.Content {
		switch b.(type) {
		case ai.ImageBlock:
			img++
		case ai.TextBlock:
			text++
		}
	}
	if img != 0 {
		t.Fatalf("a non-vision model was sent %d image block(s)", img)
	}
	if text == 0 {
		t.Fatal("the text-only form dropped the retained detail entirely")
	}
	if !strings.Contains(entries[0].Summary.Text(), "text-only model") {
		t.Fatalf("the caption must say why there is no bitmap: %s", entries[0].Summary.Text())
	}
}
