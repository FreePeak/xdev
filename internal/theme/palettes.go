package theme

import (
	"embed"
	"sync"
)

// The ported palettes (PRD §3.5, R-THEME-1). They are data, not code: eight
// files in the SAME custom-theme format ~/.xdev/agent/themes/*.json already
// uses, validated by the SAME ParseTheme, so nothing here is a second theme
// system — it is one, with a different directory.
//
// Why files rather than more map literals beside groknightSlots(): the two
// launch themes are hand-derived tables with a per-slot comment naming its
// provenance, which is worth that verbosity. A palette with 37 upstream
// tokens behind it is the opposite — it is data, it changes when upstream
// does, and the picker, the validator and the docs all already speak JSON.
//
// Colour provenance, per file (each upstream project, its licence):
//
//	catppuccin    Catppuccin Org            MIT     github.com/catppuccin/catppuccin
//	gruvbox       Pavel Pertsev             MIT     github.com/morhetz/gruvbox
//	tokyo-night   Folke Lemaitre            Apache  github.com/folke/tokyonight.nvim
//	dracula       Dracula Theme             MIT     github.com/dracula/dracula-theme
//	nord          Arctic Ice Studio         MIT     github.com/nordtheme/nord
//	rose-pine     Rosé Pine                 MIT     github.com/rose-pine/rose-pine-theme
//	one-dark      Atom (GitHub)             MIT     github.com/atom/one-dark-syntax
//	one-light     Atom (GitHub)             MIT     github.com/atom/one-light-syntax
//
// The token VALUES were read from a working TUI's own palette table (its
// `tokens.ts`, 37 tokens x 8 palettes) and mapped slot by slot; see the mapping
// table in docs/research/2026-10-03-empryo-tui-parity.md §3.5. Two entries in
// that table are not upstream's own numbers and say so in the file: the diff
// bands (upstream paints a changed row as one tinted run, xdev's is a row band
// plus a louder band under the changed words — see TestBuiltinThemesPaintTheDiff)
// and the reasoning rails (upstream has no effort ramp; see groknightSlots).
//
//go:embed builtin/*.json
var builtinFS embed.FS

// palettes parses the embedded JSON once. A parse failure is a build-time
// mistake in the repo (ParseTheme rejects a missing slot, a bad hex, a
// circular var), not a runtime condition a user can cause, so a bad file is
// dropped from the map and TestShippedPalettesParse fails on it in CI rather
// than taking the TUI down at startup.
var palettes = sync.OnceValue(func() map[string]*Theme {
	out := map[string]*Theme{}
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return out
	}
	for _, e := range entries {
		raw, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			continue
		}
		name := e.Name()
		if t, err := ParseTheme(raw, name[:len(name)-len(".json")]); err == nil && t != nil {
			out[t.Name] = t
		}
	}
	return out
})
