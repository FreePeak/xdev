package theme

import "testing"

// TestLerp pins the blend every xdev animation is built on: a→b at t=0 is a,
// t=1 is b, the midpoint rounds rather than truncates (truncation biases every
// fade toward the dark end, which reads as mud), and the ends are clamped
// instead of extrapolating a tween that overshot its target.
func TestLerp(t *testing.T) {
	a := Hex("#000000")
	b := Hex("#ffffff")

	if got := Lerp(a, b, 0); got != a {
		t.Errorf("Lerp(a,b,0) = %v, want %v", got, a)
	}
	if got := Lerp(a, b, 1); got != b {
		t.Errorf("Lerp(a,b,1) = %v, want %v", got, b)
	}
	// 255*0.5 = 127.5, which must round to 128 — truncating gives 127.
	if got := Lerp(a, b, 0.5); got != (Color{128, 128, 128}) {
		t.Errorf("Lerp(black,white,0.5) = %v, want {128 128 128}", got)
	}
	// Out-of-range t clamps rather than extrapolating past the endpoints.
	if got := Lerp(a, b, -1); got != a {
		t.Errorf("Lerp(a,b,-1) = %v, want %v (clamped)", got, a)
	}
	if got := Lerp(a, b, 2); got != b {
		t.Errorf("Lerp(a,b,2) = %v, want %v (clamped)", got, b)
	}
	// A real theme pair, not just the extremes: the night palette's dim→text
	// step is the one a focused border actually takes.
	dim := Hex("#585858")
	txt := Hex("#e1e1e1")
	if got, want := Lerp(dim, txt, 0.5), (Color{0x9d, 0x9d, 0x9d}); got != want {
		t.Errorf("Lerp(#585858,#e1e1e1,0.5) = %v, want %v", got, want)
	}
}
