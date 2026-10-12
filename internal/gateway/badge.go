package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // the embedded badge is a PNG
	"sync"

	_ "embed"
)

// The per-model badge: the icon the chat shows for the bot, recoloured to
// name the leg that answered.
//
// Why a colour and not a distinct picture per model: the gateway knows the
// serving leg only as a string ("opencode-free/big-pickle"), and there is no
// picture for it anywhere. A tint derived from that string is stable (the
// same leg always paints the same colour, across restarts and machines),
// needs no asset per model, and is legible at the 40px the chat header
// renders. A model that deserves its own artwork can override the file.
//
//go:embed brand/xdev-icon.png
var badgePNG []byte

// badgeBase is the decoded source icon, parsed once. Telegram refuses a
// profile photo that is not a static JPG, so every badge is re-encoded —
// which is also what lets one PNG produce every tint.
var badgeBase = sync.OnceValue(func() image.Image {
	img, _, err := image.Decode(bytes.NewReader(badgePNG))
	if err != nil {
		return nil
	}
	return img
})

// badgeCache memoizes the encoded JPEG per leg. The map is bounded by the
// number of distinct serving legs a gateway sees, which is a config-sized
// number, and each entry is a few KB — the same bounded-cardinality property
// the metrics registry keeps (see boundedModel).
var badgeCache sync.Map // leg -> []byte

// ModelBadgeJPEG renders the badge for one serving leg: the base icon tinted
// by a colour derived from the leg. An empty leg (no switch, or the model the
// user asked for) is the untinted icon, so restoring it is the same call.
func ModelBadgeJPEG(leg string) []byte {
	if v, ok := badgeCache.Load(leg); ok {
		return v.([]byte)
	}
	img := badgeBase()
	if img == nil {
		return nil
	}
	out := img
	if leg != "" {
		out = tint(img, legColor(leg))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 88}); err != nil {
		return nil
	}
	b := buf.Bytes()
	badgeCache.Store(leg, b)
	return b
}

// legColor derives a stable hue from the leg's name. Saturation and lightness
// are fixed: the tint must stay readable against the icon's own dark plate,
// so only the hue is free — a colour that names the leg without making it
// unreadable.
func legColor(leg string) color.RGBA {
	sum := sha256.Sum256([]byte(leg))
	hue := float64(binary.BigEndian.Uint16(sum[:2])) / 65535 * 360
	return hsl(hue, 0.62, 0.58)
}

// tint blends the icon toward c over its OPAQUE pixels only: the icon's
// transparency is what lets the chat's own background show through, and
// filling it would put a coloured square where a logo should be.
func tint(src image.Image, c color.RGBA) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := src.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			// 16-bit channels down to 8, then a 55/45 blend: the icon's own
			// shape stays legible while the hue reads as the switch.
			mix := func(v uint32, t uint8) uint8 {
				return uint8((int(v>>8)*55 + int(t)*45) / 100)
			}
			dst.Set(x, y, color.RGBA{mix(r, c.R), mix(g, c.G), mix(bl, c.B), uint8(a >> 8)})
		}
	}
	return dst
}

// hsl converts an HSL triple to RGB (h in degrees, s and l in 0..1).
func hsl(h, s, l float64) color.RGBA {
	f := func(n float64) uint8 {
		k := (n + h/30)
		for k < 0 {
			k += 12
		}
		for k >= 12 {
			k -= 12
		}
		a := s * min(l, 1-l)
		v := l - a*max(-1, min(min(k-3, 9-k), 1))
		return uint8(v * 255)
	}
	return color.RGBA{f(0), f(8), f(4), 255}
}
