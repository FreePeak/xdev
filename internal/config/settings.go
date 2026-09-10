package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Settings layering (M9 #10, research parity-session-ux §10): schema
// defaults ← global ~/.xdev/agent/config.yml ← project .xdev/config.yml ←
// repeatable --config overlays. Objects deep-merge; scalars and arrays are
// replaced wholesale (an overlay that lists one item means exactly that
// list, not a union). A malformed persistent file is preserved as
// .broken-<stamp> and the load fails loudly rather than silently dropping
// the user's configuration.

// Settings is the layered configuration surface xdev acts on. New keys
// join here so every layer sees them; unknown YAML keys are rejected so a
// typo is reported instead of ignored.
type Settings struct {
	Theme        string            `yaml:"theme"`
	DefaultModel string            `yaml:"defaultModel"`
	ApprovalMode string            `yaml:"approvalMode"` // always-ask|write|yolo
	MemoryLimit  int64             `yaml:"memoryLimit"`
	MaxTurns     int               `yaml:"maxTurns"`
	ModelRoles   map[string]string `yaml:"modelRoles"`
	// ModelRolesEffort pins a reasoning effort per role (":effort" suffix
	// on a @role reference overrides it).
	ModelRolesEffort  map[string]string `yaml:"modelRolesEffort"`
	DisabledProviders []string          `yaml:"disabledProviders"`
}

// defaultSettings is the schema-defaults layer.
func defaultSettings() *Settings {
	return &Settings{
		Theme:            "auto",
		ApprovalMode:     "yolo",
		MemoryLimit:      100 << 20,
		MaxTurns:         200,
		ModelRoles:       map[string]string{},
		ModelRolesEffort: map[string]string{},
	}
}

// GlobalSettingsPath is ~/.xdev/agent/config.yml.
func GlobalSettingsPath() string { return filepath.Join(DataDir(), "config.yml") }

// projectSettingsPath is <cwd>/.xdev/config.yml.
func projectSettingsPath(cwd string) string {
	return filepath.Join(cwd, ".xdev", "config.yml")
}

// LoadSettings layers defaults ← global ← project ← overlays.
func LoadSettings(cwd string, overlays []string) (*Settings, error) {
	s := defaultSettings()
	paths := []string{GlobalSettingsPath(), projectSettingsPath(cwd)}
	paths = append(paths, overlays...)
	for _, p := range paths {
		layer, err := readSettingsFile(p)
		if err != nil {
			return nil, err
		}
		if layer == nil {
			continue // absent file contributes nothing
		}
		if err := s.merge(layer); err != nil {
			return nil, fmt.Errorf("config: %s: %w", p, err)
		}
	}
	return s, nil
}

// readSettingsFile decodes one layer; (nil, nil) when the file is absent.
func readSettingsFile(path string) (*Settings, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s Settings
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // a typo'd key must be reported, not dropped
	if err := dec.Decode(&s); err != nil {
		// Persistent user config that will not parse is data loss waiting
		// to happen: keep the bytes, then fail.
		backup := filepath.Join(filepath.Dir(path),
			fmt.Sprintf(".broken-%s-%s", timestampForBackup(), filepath.Base(path)))
		if rerr := os.Rename(path, backup); rerr == nil {
			return nil, fmt.Errorf("config: %s: %w (preserved as %s)", path, err, backup)
		}
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &s, nil
}

// merge applies a later layer over the receiver: maps deep-merge per key,
// scalars and slices replace.
func (s *Settings) merge(layer *Settings) error {
	if layer.Theme != "" {
		s.Theme = layer.Theme
	}
	if layer.DefaultModel != "" {
		s.DefaultModel = layer.DefaultModel
	}
	if layer.ApprovalMode != "" {
		s.ApprovalMode = layer.ApprovalMode
	}
	if layer.MemoryLimit != 0 {
		s.MemoryLimit = layer.MemoryLimit
	}
	if layer.MaxTurns != 0 {
		s.MaxTurns = layer.MaxTurns
	}
	for k, v := range layer.ModelRoles {
		s.ModelRoles[k] = v
	}
	for k, v := range layer.ModelRolesEffort {
		s.ModelRolesEffort[k] = v
	}
	if layer.DisabledProviders != nil {
		s.DisabledProviders = append([]string(nil), layer.DisabledProviders...)
	}
	switch s.ApprovalMode {
	case "always-ask", "write", "yolo":
	default:
		return fmt.Errorf("unknown approvalMode %q (want always-ask|write|yolo)", s.ApprovalMode)
	}
	return nil
}

// timestampForBackup names a preserved broken file uniquely enough that two
// failures in one run never overwrite each other.
func timestampForBackup() string {
	return time.Now().UTC().Format("20060102T150405.000Z")
}

// Set writes one dotted key into the layer file at path (used by
// `xdev config set`), preserving the other keys.
func Set(path, key, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	doc := map[string]any{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("config: %s: refusing to edit unparseable file: %w", path, err)
		}
	}
	cur := doc
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = yamlScalar(value)
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// yamlScalar interprets a CLI token as bool/int/string, so `set
// memoryLimit 1048576` stores a number rather than a quoted string.
func yamlScalar(v string) any {
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	}
	var i int64
	if _, err := fmt.Sscanf(v, "%d", &i); err == nil && fmt.Sprint(i) == strings.TrimSpace(v) {
		return i
	}
	// A quoted or comma-separated token becomes a list.
	if strings.Contains(v, ",") {
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			out = append(out, strings.TrimSpace(p))
		}
		return out
	}
	return v
}

// List renders the resolved settings one key: value per line, with the
// layer each came from. Credential-bearing values are never part of
// Settings, but any key whose name suggests a secret is masked.
func List(s *Settings, globalPath string) []string {
	out := []string{
		"theme " + s.Theme,
		"approvalMode " + s.ApprovalMode,
		"maxTurns " + fmt.Sprint(s.MaxTurns),
		"memoryLimit " + fmt.Sprint(s.MemoryLimit),
	}
	if s.DefaultModel != "" {
		out = append(out, "defaultModel "+s.DefaultModel)
	}
	keys := make([]string, 0, len(s.ModelRoles))
	for k := range s.ModelRoles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, "modelRoles."+k+" "+maskCred(s.ModelRoles[k]))
	}
	out = append(out, "config "+globalPath)
	if len(s.DisabledProviders) > 0 {
		out = append(out, "disabledProviders "+strings.Join(s.DisabledProviders, ","))
	}
	return out
}

// maskCred hides anything that looks like a literal key in a listed value
// (model references like provider/model pass through untouched).
func maskCred(v string) string {
	lower := strings.ToLower(v)
	for _, marker := range []string{"sk-", "key:", "token:", "bearer:"} {
		if i := strings.Index(lower, marker); i >= 0 && len(v)-i > 12 {
			return v[:i+8] + "…"
		}
	}
	return v
}

// DeleteKey removes a dotted key from the layer file (config reset),
// leaving every other entry intact.
func DeleteKey(path, key string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing stored under that key
		}
		return err
	}
	doc := map[string]any{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("config: %s: refusing to edit unparseable file: %w", path, err)
		}
	}
	parts := strings.Split(key, ".")
	cur := doc
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil // path does not exist: already at the default
		}
		cur = next
	}
	last := parts[len(parts)-1]
	if _, ok := cur[last]; !ok {
		return nil
	}
	delete(cur, last)
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// Get reads a dotted key out of the layer file ("" when absent).
func Get(path, key string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	cur := any(doc)
	for _, p := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", nil
		}
		cur, ok = m[p]
		if !ok {
			return "", nil
		}
	}
	return fmt.Sprintf("%v", cur), nil
}
