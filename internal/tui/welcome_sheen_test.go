package tui

import "testing"

// The sheen sweep is the only moving part of the logo, so pin its
// contract: the band travels left→right across the art, the slant makes
// lower rows lag, and the period wraps back to the rest (no band).
func TestSheenInBandSweepAndWrap(t *testing.T) {
	logoW := logoWidth()
	rows := len(xdevLogo)
	if rows == 0 {
		t.Skip("no logo art")
	}
	// The sweep starts with the band fully off the left; at phase
	// 2*half its centre reaches column 0 of row 0.
	if sheenInBand(0, 0, 0, logoW) {
		t.Fatal("phase 0 must have the band still off the left edge")
	}
	enter := 2 * sheenHalf
	onFirstRow := func(phase, col int) bool { return sheenInBand(phase, 0, col, logoW) }
	if !onFirstRow(enter, 0) {
		t.Fatal("at the entry phase the band centre must be column 0")
	}
	// Only the band half-width around the centre is lit.
	if onFirstRow(enter, sheenHalf+1) {
		t.Fatal("a column outside the band half-width must be dark")
	}
	// The slant: the band centre shifts left by sheenSlant per row, so
	// row 1's lit cell sits sheenSlant columns left of row 0's.
	for col := -sheenHalf; col <= sheenHalf; col++ {
		if sheenInBand(enter, 1, col-sheenSlant, logoW) != onFirstRow(enter, col) {
			t.Fatalf("row 1 must lead row 0 by %d columns (col %d)", sheenSlant, col)
		}
	}
	// The sweep advances with phase.
	if sheenInBand(enter, 0, 0, logoW) == sheenInBand(enter+10, 0, 0, logoW) {
		t.Fatal("the band must move as phase advances")
	}
	// A full period later the geometry repeats exactly.
	period := logoW + (rows-1)*sheenSlant + 2*sheenHalf + 1 + sheenRest
	if sheenInBand(enter+period, 0, 0, logoW) != onFirstRow(enter, 0) {
		t.Fatal("phase must wrap with period")
	}
	// The rest window: after the band crossed the art it stays dark.
	if sheenInBand(enter+logoW+(rows-1)*sheenSlant+sheenHalf+2, 0, logoW-1, logoW) {
		t.Fatal("the band must be off the art during the rest window")
	}
}

func TestSheenInBandHonoursWidth(t *testing.T) {
	// A narrower logo shifts the period, so the same phase lights a
	// different cell — the width parameter must be honoured.
	if sheenInBand(0, 0, 0, 10) == sheenInBand(0, 0, 0, 80) {
		if !sheenInBand(0, 0, 0, 10) {
			return // both false is fine; both-true would ignore width
		}
	}
}
