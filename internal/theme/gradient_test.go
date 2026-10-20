package theme

import "testing"

// A gradient is a sequence of cells, so the cell's place in the run IS the
// fraction. These pin that arithmetic: the run starts at a, ends at b, and
// a cell halfway along is the halfway blend.
func TestRampSpansTheTwoStops(t *testing.T) {
	from := Hex("#000000")
	to := Hex("#ffffff")

	if got := Ramp(from, to, 0, 5); got != from {
		t.Fatalf("first cell = %v, want the start colour %v", got, from)
	}
	if got := Ramp(from, to, 4, 5); got != to {
		t.Fatalf("last cell = %v, want the end colour %v", got, to)
	}
	mid := Ramp(from, to, 2, 5)
	if want := Lerp(from, to, 0.5); mid != want {
		t.Fatalf("middle cell = %v, want the halfway blend %v", mid, want)
	}
}

// The degenerate runs answer with the stop they can: one cell has nowhere to
// ramp to, and an index outside the run clamps to the end it is nearest.
func TestRampDegenerateRuns(t *testing.T) {
	from := Hex("#102030")
	to := Hex("#405060")

	if got := Ramp(from, to, 0, 1); got != from {
		t.Fatalf("one-cell run = %v, want the start colour %v", got, from)
	}
	if got := Ramp(from, to, 0, 0); got != from {
		t.Fatalf("empty run = %v, want the start colour %v", got, from)
	}
	if got := Ramp(from, to, -1, 5); got != from {
		t.Fatalf("index before the run = %v, want the start colour %v", got, from)
	}
	if got := Ramp(from, to, 99, 5); got != to {
		t.Fatalf("index past the run = %v, want the end colour %v", got, to)
	}
}

// A run whose two stops are equal is a flat run — the ramp must not invent a
// colour the theme did not ask for.
func TestRampOfEqualStopsIsFlat(t *testing.T) {
	c := Hex("#7aa2f7")
	for i := range 4 {
		if got := Ramp(c, c, i, 4); got != c {
			t.Fatalf("cell %d = %v, want %v", i, got, c)
		}
	}
}
