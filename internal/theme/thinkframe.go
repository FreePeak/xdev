package theme

// ThinkFrameDark / ThinkFrameLight are the reasoning box's frame ink, per
// polarity — Rosé Pine's foam and the Dawn twin of its pine (both MIT,
// github.com/rose-pine/rose-pine-theme, theme/rose-pine.lua +
// themes/rose-pine-dawn-color-theme.json).
//
// The frame is xdev's OWN vocabulary, not a palette slot: it is ONE ink on
// every dark canvas and ONE on every light one, in every theme. It was a slot
// (accent_thinking), which meant /theme redrew it in whatever the loaded
// palette's accent happened to be — the report behind this was a box that did
// not look like one colour at all. Making it a constant is the whole fix, and
// it also pins the frame's contrast, which a slot could not: foam reads
// 5.4–10.8:1 on every dark canvas xdev ships, dawn pine 4.9–5.3:1 on the two
// light ones.
//
// Two hexes, not one, because one hex cannot be both. Foam on grokday's
// #eeeeee canvas measures 1.5:1 — no frame at all, the exact defect PR #587
// closed for a palette whose ink was its own base colour. Same hue, one value
// per polarity, the rule the launch themes already follow for their diff inks
// (#286983 on grokday against #31748f on groknight).
var (
	ThinkFrameDark  = Hex("#9ccfd8") // foam
	ThinkFrameLight = Hex("#286983") // pine, Rosé Pine Dawn
)

// ThinkFrame is the ink the reasoning box's frame paints: the dark twin on a
// dark canvas, the light one on a light one. The box's BODY is untouched by
// this — it stays text_primary, the slot a palette sizes for reading.
func ThinkFrame(t *Theme) Color {
	if t != nil && !t.Dark {
		return ThinkFrameLight
	}
	return ThinkFrameDark
}
