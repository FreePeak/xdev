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
	Symbols *Symbols          `json:"symbols"`
}

// Symbols selects glyph presets (F4): preset plus per-key overrides and
// spinner frames.
type Symbols struct {
	Preset        string            `json:"preset"` // unicode | nerd | ascii
	Overrides     map[string]string `json:"overrides"`
	SpinnerFrames []string          `json:"spinnerFrames"`
	Status        []string          `json:"status"`
	Activity      []string          `json:"activity"`
}

// CustomDir is <DataDir>/themes. Like every other data path it goes
// through config.DataDir so XDEV_AGENT_DIR sandboxes custom themes too
// (config does not import theme, so no cycle).
func CustomDir() string {
	return filepath.Join(config.DataDir(), "themes")
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
// error), and color values parse as hex, 256-index, var ref, or "".
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
	// Required slots: the tokens this renderer actually reads. A theme
	// may add more; missing ones are collected into ONE error so a
	// half-defined palette is fixed in one pass.
	var missing []string
	slots := map[string]Color{}
	for _, slot := range RequiredSlots() {
		value, ok := lookupSlot(f.Colors, slot)
		if !ok {
			missing = append(missing, slot)
			continue
		}
		c, cerr := parseColor(value, resolved)
		if cerr != nil {
			return nil, fmt.Errorf("theme %q: color %s: %w", name, slot, cerr)
		}
		slots[slot] = c
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("theme %q: %d missing color(s): %s", name, len(missing), strings.Join(missing, ", "))
	}
	dark := true
	if f.Dark != nil {
		dark = *f.Dark
	}
	return &Theme{Name: name, Dark: dark, Slots: slots, Symbols: symbolsOrDefault(f.Symbols)}, nil
}

// RequiredSlots is the minimum a custom theme must define: the surfaces
// the TUI paints every frame (canvas, text, accents, prompt chrome,
// markdown, rail). The full omp token set is larger; xdev requires what
// it reads, and extra keys are preserved by the loader without error.
func RequiredSlots() []string {
	return []string{
		BgBase, BgHighlight, BgTerminal,
		AccentUser, AccentAssistant, AccentThinking, AccentTool,
		AccentError, AccentSuccess, AccentRunning,
		TextPrimary, TextSecondary, GrayDim, Gray, GrayBright,
		PromptBorder, PromptBorderActive,
		MdHeading1, MdHeading2, MdHeading3, MdCode, MdCodeBg, MdMuted,
		LinkFg,
	}
}

// lookupSlot accepts either exact slot names or omp's camelCase spelling
// (textPrimary for text_primary), so imported themes load unchanged.
func lookupSlot(colors map[string]string, slot string) (string, bool) {
	if v, ok := colors[slot]; ok {
		return v, true
	}
	camel := toCamel(slot)
	if v, ok := colors[camel]; ok {
		return v, true
	}
	return "", false
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

// parseColor handles hex, 256-index, var reference, and "" (terminal
// default).
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

// symbolsOrDefault fills the glyph preset (unicode by default).
func symbolsOrDefault(s *Symbols) Symbols {
	if s == nil {
		s = &Symbols{}
	}
	if s.Preset == "" {
		s.Preset = "unicode"
	}
	return *s
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

// AvailableThemes lists "auto" first, then the built-ins, then customs
// from dir (customs never shadow a built-in name). "auto" is settable —
// Load resolves it via the env polarity guess — so it is listed whenever
// both polarity themes exist, keeping /theme auto discoverable and
// restorable.
func AvailableThemes(dir string) []string {
	b := Builtins()
	seen := map[string]bool{"auto": true} // "auto" is prepended below; a custom auto.json never duplicates it
	var builtinNames, customNames []string
	for name := range b {
		seen[name] = true
		builtinNames = append(builtinNames, name)
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
				customNames = append(customNames, name)
			}
		}
	}
	// Each group sorted, customs strictly after built-ins so the picker
	// always shows the shipped themes first.
	sort.Strings(builtinNames)
	sort.Strings(customNames)
	var out []string
	if b["groknight"] != nil && b["grokday"] != nil {
		out = append(out, "auto")
	}
	out = append(out, builtinNames...)
	return append(out, customNames...)
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
