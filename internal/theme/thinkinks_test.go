package theme

import (
	"math"
	"testing"
)

// The reasoning box's TEXT and BORDER are painted from the theme, so a /theme
// switch has to keep both readable. Two defects lived here, and neither was
// visible from the launch theme:
//
//  1. The border wore `accent_thinking`, which aliased `thinkingText` — a TEXT
//     token every ported palette fills with its own base colour (rose-pine
//     #1f1d2e, dracula #282a36, nord #242933, read from a working TUI's token
//     table where it is the canvas BEHIND a reasoning block). Against bg_base
//     that measured 1.0:1 on all eight: the frame disappeared.
//  2. The body wore `gray_dim` — chrome ink — at 1.3–2.6:1 on the same canvas
//     (dracula 1.33, one-dark 1.76, gruvbox 1.78, nord 1.64).
//
// The body stayed a slot (text_primary, the one a palette sizes for reading).
// The FRAME did not: it is now xdev's own ink, pinned per polarity
// (ThinkFrame), so the third defect — a border that changed colour with every
// /theme — has nowhere to live. These tests pin the CLAIM rather than the
// swap: every reachable palette, both inks, against its own canvas, with the
// WCAG ratio computed here. A later theme edit that darkens either ink back
// toward its own base fails on the number, not on a hex someone forgot to
// update.
//
// The render-path half — that the box actually PAINTS these two inks — is
// internal/tui/thinkinks_render_test.go and internal/tui/thinkframe_test.go,
// because they need the App.

// relLuminance is the WCAG 2.1 relative-luminance formula. Theme colors are
// plain sRGB, so this is the whole thing; there is no second, gamma-corrected
// variant to keep in step with it.
func relLuminance(c Color) float64 {
	f := func(v uint8) float64 {
		x := float64(v) / 255
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*f(c.R) + 0.7152*f(c.G) + 0.0722*f(c.B)
}

// contrastRatio is WCAG 2.1's (L1+0.05)/(L2+0.05), brighter pair first.
func contrastRatio(a, b Color) float64 {
	l1, l2 := relLuminance(a), relLuminance(b)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// everyPalette is every theme a user can reach from /theme: the two launch
// themes plus all eight ported ones. A check that looked only at groknight
// would pass on the broken palettes too — the defect was never visible there.
var everyPalette = []string{
	"groknight", "grokday",
	"catppuccin", "dracula", "gruvbox", "nord",
	"one-dark", "one-light", "rose-pine", "tokyo-night",
}

// TestReasoningInksAreReadableInEveryPalette pins both inks of the reasoning
// box on every palette. 4.5:1 is WCAG AA for normal text and the BODY is
// content, so it is held to that. The frame is a rule rather than text, where
// 3:1 (WCAG non-text contrast) is the bar — and 4.5 would reject one-light's
// accent at 3.22:1 over a near-white canvas, which is a readable frame.
//
// The frame is also checked for the case a ratio cannot express: an ink
// IDENTICAL to the canvas is not a low-contrast frame, it is no frame.
func TestReasoningInksAreReadableInEveryPalette(t *testing.T) {
	for _, name := range everyPalette {
		t.Run(name, func(t *testing.T) {
			th := Load(name)
			if th == nil {
				t.Fatalf("theme.Load(%q) = nil", name)
			}
			bg := th.Get(BgBase)

			if got := contrastRatio(th.Get(TextPrimary), bg); got < 4.5 {
				t.Errorf("reasoning body (text_primary) reads %.2f:1 on bg_base %+v, want >= 4.5:1", got, bg)
			}
			// The FRAME is no longer the accent slot: it is one pinned ink per
			// polarity (ThinkFrame), so the check runs against what actually
			// paints rather than against whatever the palette aliases
			// accent_thinking to.
			frame := ThinkFrame(th)
			if got := contrastRatio(frame, bg); got < 3.0 {
				t.Errorf("reasoning frame %+v reads %.2f:1 on bg_base %+v, want >= 3:1", frame, got, bg)
			}
			if frame == bg {
				t.Errorf("reasoning frame is the canvas colour %+v: the frame is not there", frame)
			}
		})
	}
}

// TestThinkingAccentDoesNotAliasTheTextToken is the alias-level guard, sitting
// beside the table that carries it. thinkingText is a TEXT token, so the port
// table fills it with each palette's base colour; aliasing a FRAME to it is
// what made the border vanish. The failure is silent — the theme still loads,
// still names every required slot, still passes the picker — so it needs a
// guard of its own. Nothing paints accent_thinking now (the frame is
// ThinkFrame), so this is a belt on the table: an importer that reaches for
// that slot must never get a text token as its answer.
func TestThinkingAccentDoesNotAliasTheTextToken(t *testing.T) {
	if got := legacyToCanonical[AccentThinking]; got == ThinkingText {
		t.Errorf("accent_thinking aliases %q: thinkingText is each palette's BASE colour", got)
	}
	if got := legacyToCanonical[AccentThinking]; got != Accent {
		t.Errorf("accent_thinking aliases %q, want %q", got, Accent)
	}
}
