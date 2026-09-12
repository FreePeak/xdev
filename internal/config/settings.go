package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/websearch"
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
// DefaultCompactionMethodOrder is the shipped compaction priority ladder
// (PRD M5 #6): the step-boundary threshold check first, then the reactive
// overflow and promotion strategies. agent.ParseMethodOrder owns parsing
// this value; an empty setting means this order.
const DefaultCompactionMethodOrder = "threshold,overflow,promotion"

// MethodOrderSetting is the compaction.methodOrder value: a comma-separated
// strategy priority list. A scalar ("a,b") and a sequence (["a","b"]) both
// decode, because `xdev config set` stores comma-separated values as a list
// — a plain string field would reject the CLI's own output and get the
// user's config file quarantined as broken.
type MethodOrderSetting string

func (m *MethodOrderSetting) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*m = MethodOrderSetting(strings.TrimSpace(node.Value))
	case yaml.SequenceNode:
		var parts []string
		if err := node.Decode(&parts); err != nil {
			return err
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		*m = MethodOrderSetting(strings.Join(parts, ","))
	default:
		return fmt.Errorf("compaction.methodOrder: want \"a,b\" or a list of names")
	}
	return nil
}

// CompactionSettings holds context-maintenance knobs (PRD M5 #6).
type CompactionSettings struct {
	// MethodOrder is the strategy priority order
	// (threshold|overflow|promotion); empty means the shipped default.
	MethodOrder MethodOrderSetting `yaml:"methodOrder"`
}

type Settings struct {
	Theme        string `yaml:"theme"`
	DefaultModel string `yaml:"defaultModel"`
	ApprovalMode string `yaml:"approvalMode"` // always-ask|write|yolo
	MemoryLimit  int64  `yaml:"memoryLimit"`
	MaxTurns     int    `yaml:"maxTurns"`
	// Compaction tunes context maintenance (compaction.methodOrder).
	Compaction CompactionSettings `yaml:"compaction"`
	// Retry tunes the resilience ladder (M5 #25): the fallback chain
	// table, the usage-reserve policy, and the revert-to-primary policy.
	Retry      RetrySettings     `yaml:"retry"`
	ModelRoles map[string]string `yaml:"modelRoles"`
	// ToolsApproval sets an action per tool (allow|deny|prompt).
	ToolsApproval map[string]string `yaml:"toolsApproval"`
	// BashPatterns are ordered command rules, "deny:rm -rf *" style.
	BashPatterns []string `yaml:"bashPatterns"`
	// ModelRolesEffort pins a reasoning effort per role (":effort" suffix
	// on a @role reference overrides it).
	ModelRolesEffort map[string]string `yaml:"modelRolesEffort"`
	// Memory selects the long-term memory backend (M12 F1): "off"
	// (default) or "local" (MEMORY.md + learned.md under the data dir,
	// with the memory:// read seam and the learn tool).
	Memory string `yaml:"memory"`
	// MemoryPipeline enables the local backend's two-phase consolidation
	// pipeline (M12 #13): "on" or "off" (the default). The YAML key is
	// top-level memoryPipeline, because the omp-style dotted memory.pipeline
	// path cannot sit next to the memory backend scalar; the pipeline is
	// inert unless memory is also "local" (it needs somewhere to write).
	MemoryPipeline string `yaml:"memoryPipeline"`
	// Advisor runs a background reviewer on the session (M11, research §6).
	// The reviewer model comes from modelRoles.advisor; without that role
	// the flag warns and starts disarmed.
	Advisor bool `yaml:"advisor"`
	// Hooks declares shell-command hooks per event (M11 #12):
	// event -> one command or a list. First block short-circuits,
	// last-wins for input/result overrides. See internal/hooks.
	Hooks             map[string]any `yaml:"hooks"`
	DisabledProviders []string       `yaml:"disabledProviders"`
	// EnabledProviders gates rules-discovery providers (M10 #30):
	// empty = all providers run; otherwise only the named ones
	// (native|omp-plugins|agents|cursor|windsurf|cline|github|builtin,
	// or "all"). v1 scope: this gates the rulebook only, not model
	// providers (disabledProviders keeps that role).
	EnabledProviders []string `yaml:"enabledProviders"`
	// ShowThinking renders model reasoning output in the transcript. A
	// nil pointer means "unset in this layer" (the schema default is on);
	// a plain bool could never express an explicit false through the
	// zero-skip merge. Display only — the ":effort" budget controls
	// whether the provider thinks at all.
	ShowThinking *bool `yaml:"showThinking"`
	// Personality selects the prompt-tail preset (M10 #32, omp parity):
	// default | friendly | pragmatic | none. A PERSONALITY.md override
	// always beats the preset; "none" omits the block.
	Personality string `yaml:"personality"`
	// AdvisorSyncBacklog (M11 #39; the omp key is advisor.syncBacklog):
	// bounded catch-up for the advisor review — the number of primary
	// turns one feed may span before older turns are skipped (a catch-up
	// review is capped at 30s). 0 = off, else 1|3|5.
	AdvisorSyncBacklog int `yaml:"advisorSyncBacklog"`
	// AdvisorImmuneTurns (M11 #39; omp advisor.immuneTurns, default 3):
	// after an advisor interrupt, later concerns/blockers ride as
	// non-interrupting asides for this many primary turns.
	AdvisorImmuneTurns int `yaml:"advisorImmuneTurns"`
	// TaskAgentAdvisor (M11 #39; omp task.agentAdvisor) attaches an
	// advisor to spawned subagents: "on" (the modelRoles.advisor model),
	// "off" (the default), or an explicit model reference.
	TaskAgentAdvisor string `yaml:"taskAgentAdvisor"`
	// TTSR is the stream-rules group (M11 #35): rule conditions are
	// matched against the assistant deltas; see internal/agent/ttsr.go.
	TTSR *TTSRSettings `yaml:"ttsr"`
	// LSP declares language servers for the lsp tool (M13 #52). Absent =
	// the built-in servers (gopls, rust-analyzer, typescript-language-server,
	// pyright-langserver), all launched lazily on first use.
	LSP *LSPConfig `yaml:"lsp"`
	// WebSearch configures the web_search provider chain (M13 #48):
	// ordered providers, per-provider timeout, API keys.
	WebSearch WebSearchSettings `yaml:"webSearch"`
	// Ask configures the ask tool (M11 #36): ask.timeout bounds how long
	// a headless run waits for an answer before the recommended option
	// proceeds.
	Ask struct {
		Timeout int `yaml:"timeout"`
	} `yaml:"ask"`
	// Skills configures SKILL.md discovery (M12 F2): customDirectories
	// are extra roots scanned after native/user/managed, so an authored
	// pack — and an agent-learned one — outranks them.
	Skills SkillsSettings `yaml:"skills"`
}

// SkillsSettings is the `skills` group.
type SkillsSettings struct {
	// CustomDirectories are extra `<dir>/<name>/SKILL.md` roots. A
	// relative entry resolves against the project cwd; a missing or
	// unreadable directory is skipped, never fatal.
	CustomDirectories []string `yaml:"customDirectories"`
}

// TTSRSettings is the `ttsr` group. Rule-level fields override the group
// defaults; the group is inert unless it lists at least one usable rule.
type TTSRSettings struct {
	Enabled       *bool      `yaml:"enabled"`
	InterruptMode string     `yaml:"interruptMode"` // always|prose-only|tool-only|never
	ContextMode   string     `yaml:"contextMode"`   // discard|keep
	RepeatGap     int        `yaml:"repeatGap"`     // turns between two fires
	Rules         []TTSRRule `yaml:"rules"`
}

// TTSRRule is one stream rule: a regex condition, an optional astCondition
// (run through ast-grep when it is installed), the interrupt/context policy
// for its matches, and the notice the model receives.
type TTSRRule struct {
	Name          string `yaml:"name"`
	Condition     string `yaml:"condition"`
	ASTCondition  string `yaml:"astCondition"`
	InterruptMode string `yaml:"interruptMode"`
	ContextMode   string `yaml:"contextMode"`
	RepeatGap     int    `yaml:"repeatGap"`
	Message       string `yaml:"message"`
}

// ttsrModeOK reports a valid interrupt mode ("" = inherit the group).
func ttsrModeOK(m string) bool {
	switch m {
	case "", "always", "prose-only", "tool-only", "never":
		return true
	}
	return false
}

// ttsrContextOK reports a valid context mode ("" = inherit the group).
func ttsrContextOK(m string) bool {
	return m == "" || m == "discard" || m == "keep"
}

// LSPConfig is the lsp: settings block.
type LSPConfig struct {
	// Lazy defers the launch to the first lsp call (default true). Setting
	// it false warms up servers whose root marker is present at session start.
	Lazy *bool `yaml:"lazy"`
	// IdleTimeout is a Go duration ("5m"); a running server is stopped after
	// this long without a query.
	IdleTimeout string `yaml:"idleTimeout"`
	// Servers is keyed by language name (go, rust, ...). An entry merges onto
	// the built-in server of that language, so overriding just the command
	// keeps the built-in file types and root markers.
	Servers map[string]LSPServer `yaml:"servers"`
}

// LSPServer is one language server command.
type LSPServer struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// FileTypes are the extensions (with or without the dot) this server
	// claims, used to route a file to a server.
	FileTypes []string `yaml:"fileTypes"`
	// RootMarkers are the project markers (go.mod, .git, ...) searched
	// upward from the file to pick the server's root directory.
	RootMarkers []string `yaml:"rootMarkers"`
	// RootPatterns is an accepted alias for rootMarkers.
	RootPatterns []string `yaml:"rootPatterns"`
	// InitOptions is passed through as initializationOptions.
	InitOptions map[string]any `yaml:"initOptions"`
	Disabled    bool           `yaml:"disabled"`
}

// WebSearchSettings is the webSearch config block: ordered providers,
// per-provider timeout, API keys (M13 #48). It is the engine's own Settings
// type, aliased rather than redeclared — internal/config imports
// internal/tool through internal/agent, so the struct the registry hands to
// the tool cannot live in either of those packages without an import cycle.
type WebSearchSettings = websearch.Settings

// defaultSettings is the schema-defaults layer.
// memoryOrDefault reports the effective memory backend ("" = off).
func memoryOrDefault(v string) string {
	if v == "" {
		return "off"
	}
	return v
}

func defaultSettings() *Settings {
	show := true
	return &Settings{
		Theme:              "auto",
		ApprovalMode:       "yolo",
		MemoryLimit:        100 << 20,
		MaxTurns:           200,
		Compaction:         CompactionSettings{MethodOrder: DefaultCompactionMethodOrder},
		ModelRoles:         map[string]string{},
		ToolsApproval:      map[string]string{},
		ModelRolesEffort:   map[string]string{},
		Hooks:              map[string]any{},
		ShowThinking:       &show,
		Personality:        "default",
		AdvisorImmuneTurns: 3,
		// The group ships enabled but rule-less (no rules = inert).
		TTSR: &TTSRSettings{Enabled: &show, InterruptMode: "always", ContextMode: "discard", RepeatGap: 3},
	}
}

// ShowThinkingOn reports whether thinking output should be displayed;
// unset follows the schema default (on).
func (s *Settings) ShowThinkingOn() bool {
	return s == nil || s.ShowThinking == nil || *s.ShowThinking
}

// MemoryPipelineOn reports whether the local backend's two-phase pipeline
// should run at session end (nil-safe; the shipped default is off).
func (s *Settings) MemoryPipelineOn() bool {
	return s != nil && s.MemoryPipeline == "on"
}

// CompactionMethodOrder returns the compaction.methodOrder setting in the
// comma-separated form agent.ParseMethodOrder consumes (nil-safe: a missing
// layer means "shipped default").
func (s *Settings) CompactionMethodOrder() string {
	if s == nil {
		return ""
	}
	return string(s.Compaction.MethodOrder)
}

// WebSearchConfig returns the webSearch block (nil-safe: the zero value is
// a usable keyless chain, same rule as ShowThinkingOn's nil tolerance for
// pre-main callers).
func (s *Settings) WebSearchConfig() websearch.Settings {
	if s == nil {
		return websearch.Settings{}
	}
	return s.WebSearch
}

// AskTimeout returns the ask tool's headless wait: ask.timeout seconds,
// or the tool's schema default when unset.
func (s *Settings) AskTimeout() time.Duration {
	if s == nil || s.Ask.Timeout <= 0 {
		return tool.DefaultAskTimeout
	}
	return time.Duration(s.Ask.Timeout) * time.Second
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
	if layer.Compaction.MethodOrder != "" {
		s.Compaction.MethodOrder = layer.Compaction.MethodOrder
	}
	for k, v := range layer.ModelRoles {
		s.ModelRoles[k] = v
	}
	for k, v := range layer.ToolsApproval {
		s.ToolsApproval[k] = v
	}
	if layer.BashPatterns != nil {
		s.BashPatterns = append([]string(nil), layer.BashPatterns...)
	}
	for k, v := range layer.Hooks {
		s.Hooks[k] = v
	}
	for k, v := range layer.ModelRolesEffort {
		s.ModelRolesEffort[k] = v
	}
	if layer.DisabledProviders != nil {
		s.DisabledProviders = append([]string(nil), layer.DisabledProviders...)
	}
	if layer.EnabledProviders != nil {
		s.EnabledProviders = append([]string(nil), layer.EnabledProviders...)
	}
	if layer.Memory != "" {
		s.Memory = layer.Memory
	}
	if layer.MemoryPipeline != "" {
		s.MemoryPipeline = layer.MemoryPipeline
	}
	if layer.Advisor {
		// bool with a false default: only a layer that turns it ON
		// contributes (there is no expressible "unset" for a plain bool,
		// and the shipped default is off).
		s.Advisor = true
	}
	if layer.ShowThinking != nil {
		s.ShowThinking = layer.ShowThinking
	}
	if layer.Personality != "" {
		s.Personality = layer.Personality
	}
	if layer.AdvisorSyncBacklog != 0 {
		s.AdvisorSyncBacklog = layer.AdvisorSyncBacklog
	}
	if layer.AdvisorImmuneTurns != 0 {
		s.AdvisorImmuneTurns = layer.AdvisorImmuneTurns
	}
	if layer.Skills.CustomDirectories != nil {
		s.Skills.CustomDirectories = append([]string(nil), layer.Skills.CustomDirectories...)
	}
	if layer.TaskAgentAdvisor != "" {
		s.TaskAgentAdvisor = layer.TaskAgentAdvisor
	}
	if layer.TTSR != nil {
		if s.TTSR == nil {
			s.TTSR = layer.TTSR
		} else {
			// Rules are a list: a layer that declares any replaces the
			// earlier set wholesale (same rule as bashPatterns).
			if layer.TTSR.Rules != nil {
				s.TTSR.Rules = layer.TTSR.Rules
			}
			if layer.TTSR.InterruptMode != "" {
				s.TTSR.InterruptMode = layer.TTSR.InterruptMode
			}
			if layer.TTSR.ContextMode != "" {
				s.TTSR.ContextMode = layer.TTSR.ContextMode
			}
			if layer.TTSR.RepeatGap != 0 {
				s.TTSR.RepeatGap = layer.TTSR.RepeatGap
			}
			if layer.TTSR.Enabled != nil {
				s.TTSR.Enabled = layer.TTSR.Enabled
			}
		}
	}
	if s.TTSR != nil {
		if !ttsrModeOK(s.TTSR.InterruptMode) {
			return fmt.Errorf("ttsr: unknown interruptMode %q (want always|prose-only|tool-only|never)", s.TTSR.InterruptMode)
		}
		if !ttsrContextOK(s.TTSR.ContextMode) {
			return fmt.Errorf("ttsr: unknown contextMode %q (want discard|keep)", s.TTSR.ContextMode)
		}
		if s.TTSR.RepeatGap < 0 {
			return fmt.Errorf("ttsr: repeatGap must be >= 0, got %d", s.TTSR.RepeatGap)
		}
		seen := map[string]bool{}
		for _, r := range s.TTSR.Rules {
			switch {
			case r.Name == "":
				return fmt.Errorf("ttsr: every rule needs a name")
			case seen[r.Name]:
				return fmt.Errorf("ttsr: duplicate rule name %q", r.Name)
			case r.Condition == "":
				return fmt.Errorf("ttsr: rule %q needs a condition", r.Name)
			case !ttsrModeOK(r.InterruptMode):
				return fmt.Errorf("ttsr: rule %q: unknown interruptMode %q", r.Name, r.InterruptMode)
			case !ttsrContextOK(r.ContextMode):
				return fmt.Errorf("ttsr: rule %q: unknown contextMode %q", r.Name, r.ContextMode)
			case r.RepeatGap < 0:
				return fmt.Errorf("ttsr: rule %q: repeatGap must be >= 0", r.Name)
			}
			seen[r.Name] = true
		}
	}
	if layer.LSP != nil {
		if s.LSP == nil {
			s.LSP = &LSPConfig{}
		}
		if layer.LSP.Lazy != nil {
			s.LSP.Lazy = layer.LSP.Lazy
		}
		if layer.LSP.IdleTimeout != "" {
			s.LSP.IdleTimeout = layer.LSP.IdleTimeout
		}
		if len(layer.LSP.Servers) > 0 && s.LSP.Servers == nil {
			s.LSP.Servers = map[string]LSPServer{}
		}
		for k, v := range layer.LSP.Servers {
			s.LSP.Servers[k] = v
		}
	}
	// webSearch: the provider list is replaced wholesale (an overlay that
	// names one provider means exactly that chain), keys merge per
	// provider, and the timeout duration is validated here so a typo is
	// reported instead of silently falling back to the default.
	if layer.WebSearch.Providers != nil {
		s.WebSearch.Providers = append([]string(nil), layer.WebSearch.Providers...)
	}
	if layer.WebSearch.Timeout != "" {
		if _, err := time.ParseDuration(layer.WebSearch.Timeout); err != nil {
			return fmt.Errorf("webSearch.timeout %q: %w", layer.WebSearch.Timeout, err)
		}
		s.WebSearch.Timeout = layer.WebSearch.Timeout
	}
	if layer.WebSearch.MaxResults != 0 {
		if layer.WebSearch.MaxResults < 0 {
			return fmt.Errorf("webSearch.maxResults must be positive, got %d", layer.WebSearch.MaxResults)
		}
		s.WebSearch.MaxResults = layer.WebSearch.MaxResults
	}
	for k, v := range layer.WebSearch.APIKeys {
		if s.WebSearch.APIKeys == nil {
			s.WebSearch.APIKeys = map[string]string{}
		}
		s.WebSearch.APIKeys[k] = v
	}
	// Retry group last: its validation must see the fully merged layer.
	if err := s.mergeRetry(layer); err != nil {
		return err
	}
	if layer.Ask.Timeout != 0 {
		s.Ask.Timeout = layer.Ask.Timeout
	}
	switch s.MemoryPipeline {
	case "", "on", "off":
	default:
		return fmt.Errorf("unknown memoryPipeline %q (want on|off)", s.MemoryPipeline)
	}
	switch s.ApprovalMode {
	case "always-ask", "write", "yolo":
	default:
		return fmt.Errorf("unknown approvalMode %q (want always-ask|write|yolo)", s.ApprovalMode)
	}
	switch s.Personality {
	case "default", "friendly", "pragmatic", "none":
	default:
		return fmt.Errorf("unknown personality %q (want default|friendly|pragmatic|none)", s.Personality)
	}
	return nil
}

// ProviderDisabled reports whether a provider is switched off by
// disabledProviders. Matching is case-insensitive on the models.yml key.
func (s *Settings) ProviderDisabled(name string) bool {
	if s == nil {
		return false
	}
	for _, d := range s.DisabledProviders {
		if strings.EqualFold(strings.TrimSpace(d), name) {
			return true
		}
	}
	return false
}

// CheckProvider gates a provider before it is used. Absent credentials are
// reported only for providers that can actually authenticate (a keyless
// local server like ollama is legitimately credential-free), so the rule is
// "disabled → refuse; enabled but unconfigured → refuse with the fix".
func (s *Settings) CheckProvider(name string, hasCredential bool) error {
	// A nil layer (tests, pre-settings code paths) means "no gating": the
	// zero configuration must not refuse to start.
	if s == nil {
		return nil
	}
	if s.ProviderDisabled(name) {
		return fmt.Errorf("config: provider %q is disabled (remove it from disabledProviders to use it)", name)
	}
	if !hasCredential {
		return fmt.Errorf("config: provider %q has no credential: set apiKey in models.yml, run /login, or export the provider key", name)
	}
	return nil
}

// KeylessAuth reports a provider that needs no credential (auth: none).
func (pc *ProviderConfig) KeylessAuth() bool {
	return pc != nil && strings.EqualFold(strings.TrimSpace(pc.Auth), "none")
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
		"showThinking " + fmt.Sprint(s.ShowThinkingOn()),
		"advisor " + fmt.Sprint(s.Advisor),
		"memory " + memoryOrDefault(s.Memory),
		"memoryPipeline " + memoryOrDefault(s.MemoryPipeline),
		"personality " + s.Personality,
		"compaction.methodOrder " + s.CompactionMethodOrder(),
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
	backlog := "off"
	if s.AdvisorSyncBacklog > 0 {
		backlog = fmt.Sprint(s.AdvisorSyncBacklog)
	}
	immune := s.AdvisorImmuneTurns
	if immune == 0 {
		immune = 3
	}
	taskAdvisor := s.TaskAgentAdvisor
	if taskAdvisor == "" {
		taskAdvisor = "off"
	}
	out = append(out, "advisorSyncBacklog "+backlog,
		"advisorImmuneTurns "+fmt.Sprint(immune),
		"taskAgentAdvisor "+taskAdvisor)
	out = append(out, "config "+globalPath)
	if len(s.DisabledProviders) > 0 {
		out = append(out, "disabledProviders "+strings.Join(s.DisabledProviders, ","))
	}
	if len(s.EnabledProviders) > 0 {
		out = append(out, "enabledProviders "+strings.Join(s.EnabledProviders, ","))
	}
	if s.Ask.Timeout > 0 {
		out = append(out, "ask.timeout "+fmt.Sprint(s.Ask.Timeout))
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
