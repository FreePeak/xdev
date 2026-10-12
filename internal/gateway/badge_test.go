package gateway

import (
	"bytes"
	"image/jpeg"
	"testing"
)

// The badge must be a decodable JPEG: Telegram refuses a profile photo in any
// other format, and it refuses to reuse one it already has, so every switch
// uploads a freshly encoded image.
func TestModelBadgeIsJPEG(t *testing.T) {
	for _, leg := range []string{"", "opencode-free/big-pickle", "kilocode/kilo-auto/free"} {
		raw := ModelBadgeJPEG(leg)
		if len(raw) == 0 {
			t.Fatalf("ModelBadgeJPEG(%q) produced nothing", leg)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("ModelBadgeJPEG(%q): not a JPEG: %v", leg, err)
		}
		if cfg.Width == 0 || cfg.Height == 0 {
			t.Fatalf("ModelBadgeJPEG(%q): %dx%d", leg, cfg.Width, cfg.Height)
		}
	}
}

// The tint must be a function of the leg ALONE: the same model has to paint
// the same badge across restarts and machines, or the chat header would claim
// a different model after every restart.
func TestModelBadgeIsStablePerLeg(t *testing.T) {
	a := ModelBadgeJPEG("p1/m1")
	b := ModelBadgeJPEG("p1/m1")
	if !bytes.Equal(a, b) {
		t.Fatal("the same leg produced two different badges")
	}
	c := ModelBadgeJPEG("p2/m2")
	if bytes.Equal(a, c) {
		t.Fatal("two different legs produced the same badge")
	}
}

// The tint must reach the icon's OPAQUE pixels only. JPEG is lossy and has no
// alpha, so the transparent plate encodes as near-black with a few counts of
// bleed either way: the assertion is that the corner stays NEUTRAL and dark in
// both badges (a tint would give it a hue), while pixels inside the mark do
// differ between legs.
func TestModelBadgeTintsOnlyTheMark(t *testing.T) {
	corner := func(leg string) (r, g, b uint8) {
		img, err := jpeg.Decode(bytes.NewReader(ModelBadgeJPEG(leg)))
		if err != nil {
			t.Fatal(err)
		}
		rr, gg, bb, _ := img.At(img.Bounds().Min.X, img.Bounds().Min.Y).RGBA()
		return uint8(rr >> 8), uint8(gg >> 8), uint8(bb >> 8)
	}
	r1, g1, b1 := corner("p1/m1")
	r2, g2, b2 := corner("p2/m2")
	for _, c := range [][3]uint8{{r1, g1, b1}, {r2, g2, b2}} {
		if c[0] > 32 || c[1] > 32 || c[2] > 32 {
			t.Fatalf("corner %v is not the dark transparent plate", c)
		}
		lo, hi := c[0], c[0]
		for _, v := range c[1:] {
			lo, hi = min(lo, v), max(hi, v)
		}
		if hi-lo > 16 {
			t.Fatalf("corner %v carries a hue: the tint painted the transparent plate", c)
		}
	}

	a, err := jpeg.Decode(bytes.NewReader(ModelBadgeJPEG("p1/m1")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := jpeg.Decode(bytes.NewReader(ModelBadgeJPEG("p2/m2")))
	if err != nil {
		t.Fatal(err)
	}
	tinted := false
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y && !tinted; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			ar, ag, ab, _ := a.At(x, y).RGBA()
			br, bg, bb, _ := b.At(x, y).RGBA()
			if ar != br || ag != bg || ab != bb {
				tinted = true
				break
			}
		}
	}
	if !tinted {
		t.Fatal("two different legs produced identical pixels: nothing was tinted")
	}
}

// The caption the chat shows beside the photo names the switch, and the
// served leg is read back off it, so printer and parser share one wording.
func TestSwitchCaptionNamesTheLeg(t *testing.T) {
	got := switchCaption("execution", "opencode-free/big-pickle")
	if got == "" {
		t.Fatal("switchCaption returned nothing")
	}
	if !bytes.Contains([]byte(got), []byte("opencode-free/big-pickle")) {
		t.Fatalf("caption %q does not name the serving leg", got)
	}
}
