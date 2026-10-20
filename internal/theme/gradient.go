package theme

// Ramp returns the colour of cell i in a run of n cells, blending from a to
// b.
//
// A gradient is a sequence of cells, not a sequence of eased times: the
// caller's place in the run IS the fraction, so a run always starts at a and
// reaches b at its last cell whatever width it is. Lerp is the blend; Ramp
// is the arithmetic that hands it the right t.
//
// The degenerate cases answer with the stop they can: a run of one cell has
// nowhere to ramp to and is a, and an index outside the run clamps to the
// end it is nearest.
func Ramp(a, b Color, i, n int) Color {
	switch {
	case n <= 1:
		return a
	case i <= 0:
		return a
	case i >= n-1:
		return b
	}
	return Lerp(a, b, float64(i)/float64(n-1))
}
