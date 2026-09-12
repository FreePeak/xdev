package theme

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/config"
)

// Custom JSON themes (M12, research F4 CORE): user themes live in
// ~/.xdev/agent/themes/<name>.json and are validated on load — a theme
// missing slots fails with a grouped error naming them, so a typo'd
// palette never renders half-styled.
//
// Built-ins take precedence over customs on a name collision (omp
// semantics: built-ins win), and the schema accepts the slot names xdev
// already uses plus the two spellings omp ships (camelCase and snake).

// ThemeFile is the on-disk JSON shape.
type ThemeFile struct {
	Name    string            `json:"name"`
	Dark    *bool             `json:"dark"`
	Colors  map[string]string `json:"colors"`
	Vars    map[string]string `json:"vars"`
	Export  ExportSlots       `json:"export"`
	Symbols *Symbols          `json:"symbols"`
}

// ExportSlots is the optional export block (omp): the page/card/info
// surfaces a theme names for embedding. xdev reads pageBg/cardBg as the
// canvas defaults when a theme carries no legacy bg_* slots.
type ExportSlots struct {
	PageBg string `json:"pageBg"`
	CardBg string `json:"cardBg"`
	InfoBg string `json:"infoBg"`
}

// Symbols selects glyph presets (F4): preset, box style, per-glyph
// overrides and spinner frames (flat list, or omp's {status,activity} —
// see UnmarshalJSON).
type Symbols struct {
	Preset        string            `json:"preset"` // unicode | nerd | ascii
	Box           string            `json:"box"`    // round | sharp
	Overrides     map[string]string `json:"overrides"`
	SpinnerFrames []string          `json:"spinnerFrames"`
	Status        []string          `json:"status"`
	Activity      []string          `json:"activity"`
}

// CustomDir is <DataDir>/themes. Like every other data path it goes
// through config.DataDir so XDEV_AGENT_DIR sandboxes custom themes too
// (config does not import theme, so no cycle).
func CustomDir() string {
	dir := config.DataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "themes")
}

// LoadCustom reads and validates <dir>/<name>.json. A missing file is
// (nil, nil) so callers can fall through to built-ins.
func LoadCustom(dir, name string) (*Theme, error) {
	if dir == "" || name == "" {
		return nil, nil
	}
	path := filepath.Join(dir, name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseTheme(raw, name)
}

// ParseTheme validates one theme document: vars resolve (missing or
// circular references error), every required slot is present (grouped
// error), and color values parse as hex, 256-index, var ref, or ""
// (terminal default).
func ParseTheme(raw []byte, fallbackName string) (*Theme, error) {
	var f ThemeFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("theme: invalid JSON: %w", err)
	}
	name := f.Name
	if name == "" {
		name = fallbackName
	}
	if name == "" {
		return nil, fmt.Errorf("theme: name is required")
	}
	if len(f.Colors) == 0 {
		return nil, fmt.Errorf("theme %q: colors is required", name)
	}
	resolved, err := resolveVars(f.Vars)
	if err != nil {
		return nil, fmt.Errorf("theme %q: %w", name, err)
	}
	symbols := Symbols{}
	if f.Symbols != nil {
		if symbols, err = normalizeSymbols(*f.Symbols); err != nil {
			return nil, fmt.Errorf("theme %q: %w", name, err)
		}
	}
	// Required slots: the omp token contract. A theme may spell a slot
	// canonically or with the legacy xdev name; missing ones are collected
	// into ONE error so a half-defined palette is fixed in one pass.
	var missing []string
	slots := map[string]Color{}
	defaults := map[string]bool{}
	for _, slot := range RequiredSlots() {
		raw, ok := lookupSlot(f.Colors, slot)
		if !ok {
			missing = append(missing, slot)
			continue
		}
		c, cerr := parseColor(raw, resolved)
		if cerr != nil {
			return nil, fmt.Errorf("theme %q: color %s: %w", name, slot, cerr)
		}
		slots[slot] = c
		if strings.TrimSpace(raw) == "" {
			defaults[slot] = true // terminal default
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("theme %q: %d missing color(s): %s", name, len(missing), strings.Join(missing, ", "))
	}
	// thinking_max is optional: omp falls back to thinking_xhigh.
	slots[ThinkingMax] = slots[ThinkingXhigh]
	// Mirror the legacy xdev names the TUI reads: an explicit legacy key
	// wins; otherwise derive from the canonical token (or the documented
	// extra chain for slots omp has no equivalent of). Extras are optional
	// — with nothing to derive from, Get falls back.
	for _, legacy := range legacyOrder {
		raw, ok := f.Colors[legacy]
		if !ok {
			raw, ok = f.Colors[toCamel(legacy)] // camelCase spelling
		}
		if ok {
			c, cerr := parseColor(raw, resolved)
			if cerr != nil {
				return nil, fmt.Errorf("theme %q: color %s: %w", name, legacy, cerr)
			}
			slots[legacy] = c
			if strings.TrimSpace(raw) == "" {
				defaults[legacy] = true
			}
			continue
		}
		if canon, ok := legacyToCanonical[legacy]; ok {
			slots[legacy] = slots[canon]
			continue
		}
		if src, ok := extraSlot(f, slots, legacy); ok {
			slots[legacy] = src
		}
	}
	dark := true
	if f.Dark != nil {
		dark = *f.Dark
	}
	return &Theme{Name: name, Dark: dark, Slots: slots, Defaults: defaults, Symbols: symbols}, nil
}

// extraSlot resolves one optional legacy slot from its documented chain
// (see extraChains): first an export surface, then an already-parsed slot.
func extraSlot(f ThemeFile, slots map[string]Color, legacy string) (Color, bool) {
	for _, src := range extraChains[legacy] {
		switch {
		case strings.HasPrefix(src, "export."):
			v := map[string]string{"export.pageBg": f.Export.PageBg, "export.cardBg": f.Export.CardBg, "export.infoBg": f.Export.InfoBg}[src]
			if v == "" {
				continue
			}
			if c, err := parseColor(v, nil); err == nil {
				return c, true
			}
		default:
			if c, ok := slots[src]; ok {
				return c, true
			}
		}
	}
	return Color{}, false
}

// requiredSlots is the omp token contract (research F4 groups): every slot
// a theme must define, in either spelling. Slots xdev does not paint yet
// are still required — the contract is completeness, so an imported theme
// carries the full palette and a half-defined one fails at load instead of
// rendering with fallback colors.
var requiredSlots = []string{
	// Core text/borders (11).
	Accent, Border, BorderAccent, BorderMuted, Success, Error, Warning,
	Muted, Dim, Text, ThinkingText,
	// Backgrounds (7).
	SelectedBg, UserMessageBg, CustomMessageBg, ToolPendingBg, ToolSuccessBg,
	ToolErrorBg, StatusLineBg,
	// Message/tool text (5).
	UserMessageText, CustomMessageText, CustomMessageLabel, ToolTitle, ToolOutput,
	// Markdown (10).
	MdHeading, MdLink, MdLinkUrl, MdCode, MdCodeBlock, MdCodeBlockBorder,
	MdQuote, MdQuoteBorder, MdHr, MdListBullet,
	// Diff + syntax (12).
	ToolDiffAdded, ToolDiffRemoved, ToolDiffContext,
	SyntaxComment, SyntaxFunction, SyntaxKeyword, SyntaxNumber,
	SyntaxOperator, SyntaxPunctuation, SyntaxString, SyntaxType, SyntaxVariable,
	// Thinking-mode rails (8; thinking_max is optional → thinking_xhigh).
	ThinkingOff, ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh,
	ThinkingXhigh, BashMode, PythonMode,
	// Status line / HUD (13).
	StatusLineSep, StatusLineModel, StatusLinePath, StatusLineGitClean,
	StatusLineGitDirty, StatusLineContext, StatusLineSpend, StatusLineStaged,
	StatusLineDirty, StatusLineUntracked, StatusLineOutput, StatusLineCost,
	StatusLineSubagents,
}

// RequiredSlots is the token set a custom theme must define: the full omp
// contract plus the spellings xdev reads. The legacy xdev-only surfaces
// (bg_base, bg_terminal, accent_running, gray, gray_bright, md_heading_h2/h3)
// are optional — each has a documented derivation chain (extraChains), and
// Get falls back when a chain is empty.
func RequiredSlots() []string {
	out := make([]string, len(requiredSlots))
	copy(out, requiredSlots)
	return out
}

// legacyToCanonical maps the xdev slot names the TUI reads onto the
// canonical token that satisfies them.
var legacyToCanonical = map[string]string{
	BgHighlight:        UserMessageBg,
	AccentUser:         UserMessageText,
	AccentAssistant:    Accent,
	AccentThinking:     ThinkingText,
	AccentTool:         ToolTitle,
	AccentError:        Error,
	AccentSuccess:      Success,
	TextPrimary:        Text,
	TextSecondary:      Muted,
	GrayDim:            Dim,
	PromptBorder:       Border,
	PromptBorderActive: BorderAccent,
	MdHeading1:         MdHeading,
	MdCodeBg:           MdCodeBlock,
	MdMuted:            MdListBullet,
	LinkFg:             MdLink,
}

// legacyOrder is every xdev slot the TUI reads, in dependency order (gray
// before gray_bright, md_heading before the h2/h3 extras).
var legacyOrder = []string{
	BgBase, BgHighlight, BgTerminal,
	AccentUser, AccentAssistant, AccentThinking, AccentTool,
	AccentError, AccentSuccess, AccentRunning,
	TextPrimary, TextSecondary,
	GrayDim, Gray, GrayBright,
	PromptBorder, PromptBorderActive,
	MdHeading1, MdHeading2, MdHeading3,
	MdCode, MdCodeBg, MdMuted, LinkFg,
}

// extraChains: the optional legacy slots omp has no equivalent for, with
// the derivation each falls back to. "export.*" reads the export block;
// anything else is an already-parsed slot. Empty chain = Get falls back.
var extraChains = map[string][]string{
	BgBase:        {"export.pageBg", SelectedBg},
	BgTerminal:    {"export.pageBg"},
	AccentRunning: {Accent},
	Gray:          {Muted},
	GrayBright:    {Gray, Muted},
	MdHeading2:    {MdHeading, MdHeading1},
	MdHeading3:    {MdHeading, MdHeading1},
}

// lookupSlot returns the raw color for a canonical slot, accepting the
// exact name, its camelCase spelling, or any legacy xdev name aliased to
// it — so both vocabularies (and imported omp themes) load unchanged.
func lookupSlot(colors map[string]string, canonical string) (string, bool) {
	for _, key := range slotCandidates(canonical) {
		if v, ok := colors[key]; ok {
			return v, true
		}
	}
	return "", false
}

// slotCandidates lists the color keys that satisfy one canonical token.
func slotCandidates(canonical string) []string {
	cands := []string{canonical, toCamel(canonical)}
	for legacy, canon := range legacyToCanonical {
		if canon == canonical {
			cands = append(cands, legacy)
		}
	}
	return cands
}

func toCamel(snake string) string {
	parts := strings.Split(snake, "_")
	out := parts[0]
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		out += strings.ToUpper(p[:1]) + p[1:]
	}
	return out
}

// parseColor handles hex, 256-index, var reference (xdev's `@name` or omp's
// bare var name), and "" (terminal default).
func parseColor(value string, vars map[string]string) (Color, error) {
	v := strings.TrimSpace(value)
	for depth := 0; strings.HasPrefix(v, "@"); depth++ {
		if depth > 8 {
			return Color{}, fmt.Errorf("var chain too deep at %q", value)
		}
		ref := strings.TrimPrefix(v, "@")
		next, ok := vars[ref]
		if !ok {
			return Color{}, fmt.Errorf("unknown var %q", ref)
		}
		v = strings.TrimSpace(next)
	}
	if v == "" {
		return Color{}, nil // terminal default
	}
	if strings.HasPrefix(v, "#") {
		if len(v) != 7 {
			return Color{}, fmt.Errorf("hex color %q must be #RRGGBB", v)
		}
		n, err := strconv.ParseUint(v[1:], 16, 32)
		if err != nil {
			return Color{}, fmt.Errorf("bad hex color %q", v)
		}
		return Color{uint8(n >> 16), uint8(n >> 8), uint8(n)}, nil
	}
	if idx, err := strconv.Atoi(v); err == nil {
		if idx < 0 || idx > 255 {
			return Color{}, fmt.Errorf("256-color index %d out of range", idx)
		}
		return Xterm256(idx), nil
	}
	// omp spells a var reference as the bare var name (`"accent": "accent"`).
	if raw, ok := vars[v]; ok {
		return parseColor(raw, nil)
	}
	return Color{}, fmt.Errorf("color %q must be #RRGGBB, 0-255, @var, or empty", v)
}

// resolveVars flattens @references (circular references error).
func resolveVars(vars map[string]string) (map[string]string, error) {
	out := map[string]string{}
	var resolve func(name string, seen map[string]bool) (string, error)
	resolve = func(name string, seen map[string]bool) (string, error) {
		if v, ok := out[name]; ok {
			return v, nil
		}
		raw, ok := vars[name]
		if !ok {
			return "", fmt.Errorf("missing var %q", name)
		}
		if seen[name] {
			return "", fmt.Errorf("circular var reference at %q", name)
		}
		seen[name] = true
		if strings.HasPrefix(strings.TrimSpace(raw), "@") {
			inner, err := resolve(strings.TrimPrefix(strings.TrimSpace(raw), "@"), seen)
			if err != nil {
				return "", err
			}
			raw = inner
		}
		out[name] = raw
		return raw, nil
	}
	for name := range vars {
		if _, err := resolve(name, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Symbols exposes the theme's glyph preset.
func (t *Theme) SymbolPreset() string {
	if t == nil || t.Symbols.Preset == "" {
		if t == nil {
			return "unicode"
		}
		return "unicode"
	}
	return t.Symbols.Preset
}

// AvailableThemes lists built-ins plus customs from dir (customs never
// shadow a built-in name).
func AvailableThemes(dir string) []string {
	seen := map[string]bool{}
	var out []string
	for name := range Builtins() {
		seen[name] = true
		out = append(out, name)
	}
	if dir != "" {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				name := strings.TrimSuffix(e.Name(), ".json")
				if seen[name] {
					continue // built-ins win
				}
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// LoadNamed resolves a theme by name for rendering: built-ins first,
// then the custom directory. Errors fall back to the auto default so a
// broken custom theme never blocks startup.
func LoadNamed(name, dir string) *Theme {
	if t, err := LoadCustom(dir, name); err == nil && t != nil {
		return t
	}
	return Load(name)
}

// Watch polls a custom theme file and re-applies it when it changes
// (omp live reload, polling instead of fsnotify). Returns a stop func.
// The callback fires only on a successful parse; a broken edit keeps the
// last-good theme.
func Watch(dir, name string, apply func(*Theme)) func() {
	if dir == "" || name == "" || apply == nil {
		return func() {}
	}
	path := filepath.Join(dir, name+".json")
	stop := make(chan struct{})
	var lastStamp time.Time
	if st, err := os.Stat(path); err == nil {
		lastStamp = st.ModTime()
	}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				st, err := os.Stat(path)
				if err != nil || !st.ModTime().After(lastStamp) {
					continue
				}
				lastStamp = st.ModTime()
				if th, err := LoadCustom(dir, name); err == nil && th != nil {
					apply(th)
				}
			}
		}
	}()
	return func() { close(stop) }
}
