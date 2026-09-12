package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/browser"
	"github.com/FreePeak/xdev/internal/imagegen"
	"github.com/FreePeak/xdev/internal/tool"
	"github.com/FreePeak/xdev/internal/tts"
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

// CompactionMethodNames is the full compaction.methodOrder vocabulary: the
// shipped ladder plus the M5 #24 method-ladder tails. threshold/overflow/
// promotion are triggers, not products; remote, snapcompact, handoff, shake
// and soft are the methods a boundary compaction can run, tried in the
// order the user lists them (agent owns the implementations).
var CompactionMethodNames = []string{
	"threshold", "overflow", "promotion",
	"remote", "snapcompact", "handoff", "shake", "soft",
}

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

// CompactionSettings holds context-maintenance knobs (PRD M5 #6, #24).
type CompactionSettings struct {
	// MethodOrder is the strategy priority order; empty means the shipped
	// default (CompactionMethodNames is the vocabulary).
	MethodOrder MethodOrderSetting `yaml:"methodOrder"`
	// IdleAfter compacts a session that sat idle between two runs for at
	// least this long (a Go duration string, e.g. "10m"); empty disables
	// the trigger.
	IdleAfter string `yaml:"idleAfter"`
	// Async runs the provider summarize in the background and applies it
	// at the next step boundary instead of blocking the turn; nil (unset)
	// keeps the synchronous ladder.
	Async *bool `yaml:"async"`
}

// HandoffSettings holds the handoff-document knobs (M5 #23).
type HandoffSettings struct {
	// SaveToDisk mirrors each generated handoff document under
	// <dataDir>/handoffs/<shortid>.md, so a handed-off context survives
	// the session file being deleted (omp compaction.handoffSaveToDisk).
	SaveToDisk bool `yaml:"saveToDisk"`
}

// MnemopiSettings is the memoryMnemopi config block (M12 #44): bank
// scoping for the SQLite backend, the synthesis mode, and the recall /
// injection bounds.
//
// The YAML key is top-level (memoryMnemopi.*) for the same reason
// memoryPipeline is: the memory scalar already occupies the `memory` key,
// so an omp-style dotted memory.mnemopi path cannot live beside it.
type MnemopiSettings struct {
	// Scope is the bank scoping rule: global | project (default) |
	// project-tagged. A recall always also reads the global bank.
	Scope string `yaml:"scope"`
	// Tag is the literal bank tag used by the project-tagged scope.
	Tag string `yaml:"tag"`
	// LLMMode is smol (default) | remote | none. It selects which role's
	// completion the reflect pass runs on; none disables reflect.
	LLMMode string `yaml:"llmMode"`
	// RetainEveryNTurns enqueues a consolidation every N turns
	// (default 3; -1 disables the turn trigger).
	RetainEveryNTurns int `yaml:"retainEveryNTurns"`
	// RecallLimit bounds how many memories one recall returns
	// (default 8; the backend clamps at 32).
	RecallLimit int `yaml:"recallLimit"`
	// InjectionTokenLimit bounds the injected recall/summary block
	// (~4 chars/token; default 1200).
	InjectionTokenLimit int `yaml:"injectionTokenLimit"`
	// QueueDrainMillis is the session-exit drain budget for the retain
	// queue (default 1500ms).
	QueueDrainMillis int `yaml:"queueDrainMillis"`
}

// DefaultMnemopiSettings is the shipped memoryMnemopi block.
func DefaultMnemopiSettings() MnemopiSettings {
	return MnemopiSettings{
		Scope:               "project",
		LLMMode:             "smol",
		RetainEveryNTurns:   3,
		RecallLimit:         8,
		InjectionTokenLimit: 1200,
		QueueDrainMillis:    1500,
	}
}

// MnemopiConfig returns the memoryMnemopi block with the shipped defaults
// applied field by field (nil/zero-safe: what a caller gets is always
// usable, the same rule WebSearchConfig follows).
func (s *Settings) MnemopiConfig() MnemopiSettings {
	out := DefaultMnemopiSettings()
	if s == nil {
		return out
	}
	if s.Mnemopi.Scope != "" {
		out.Scope = s.Mnemopi.Scope
	}
	if s.Mnemopi.Tag != "" {
		out.Tag = s.Mnemopi.Tag
	}
	if s.Mnemopi.LLMMode != "" {
		out.LLMMode = s.Mnemopi.LLMMode
	}
	if s.Mnemopi.RetainEveryNTurns != 0 {
		out.RetainEveryNTurns = s.Mnemopi.RetainEveryNTurns
	}
	if s.Mnemopi.RecallLimit != 0 {
		out.RecallLimit = s.Mnemopi.RecallLimit
	}
	if s.Mnemopi.InjectionTokenLimit != 0 {
		out.InjectionTokenLimit = s.Mnemopi.InjectionTokenLimit
	}
	if s.Mnemopi.QueueDrainMillis != 0 {
		out.QueueDrainMillis = s.Mnemopi.QueueDrainMillis
	}
	return out
}

// mergeMnemopi applies a later layer's block over an earlier one, field by
// field (zero-skip, the same rule Settings.merge uses for scalars).
func mergeMnemopi(dst, layer MnemopiSettings) MnemopiSettings {
	if layer.Scope != "" {
		dst.Scope = layer.Scope
	}
	if layer.Tag != "" {
		dst.Tag = layer.Tag
	}
	if layer.LLMMode != "" {
		dst.LLMMode = layer.LLMMode
	}
	if layer.RetainEveryNTurns != 0 {
		dst.RetainEveryNTurns = layer.RetainEveryNTurns
	}
	if layer.RecallLimit != 0 {
		dst.RecallLimit = layer.RecallLimit
	}
	if layer.InjectionTokenLimit != 0 {
		dst.InjectionTokenLimit = layer.InjectionTokenLimit
	}
	if layer.QueueDrainMillis != 0 {
		dst.QueueDrainMillis = layer.QueueDrainMillis
	}
	return dst
}

// HindsightSettings is the `hindsight` group (memory: hindsight). Every key
// is optional; an unset key takes the built-in default, then the
// HINDSIGHT_* environment override.
type HindsightSettings struct {
	// APIURL is the server base URL (default http://localhost:8888).
	APIURL string `yaml:"apiUrl"`
	// APIToken is sent as `Authorization: Bearer <token>` when set.
	APIToken string `yaml:"apiToken"`
	// BankID is the bank base; empty derives it from the scoping mode.
	BankID string `yaml:"bankId"`
	// BankMission is an advisory mission string carried for parity.
	BankMission string `yaml:"bankMission"`
	// Scoping is global | per-project | per-project-tagged (default).
	Scoping string `yaml:"scoping"`
	// RetainMode is full-session | last-turn (default full-session).
	RetainMode string `yaml:"retainMode"`
	// RecallBudget is low | mid | high (default mid).
	RecallBudget string `yaml:"recallBudget"`
	// AutoRecall recalls once at the first turn (default on).
	AutoRecall *bool `yaml:"autoRecall"`
	// AutoRetain retains every RetainEveryNTurns user turns (default on).
	AutoRetain *bool `yaml:"autoRetain"`
	// Debug logs every request.
	Debug bool `yaml:"debug"`
	// RecallMaxTokens bounds what the server may return (default 1024).
	RecallMaxTokens int `yaml:"recallMaxTokens"`
	// RecallContextTurns is how many recent turns seed the recall query.
	RecallContextTurns int `yaml:"recallContextTurns"`
	// RecallMaxQueryChars caps the query text sent to the server (800).
	RecallMaxQueryChars int `yaml:"recallMaxQueryChars"`
	// RetainEveryNTurns is the autoRetain cadence in user turns (3).
	RetainEveryNTurns int `yaml:"retainEveryNTurns"`
	// InjectionTokenLimit bounds the recalled block injected into the
	// prompt and appended to a compaction summary (default 1024).
	InjectionTokenLimit int `yaml:"injectionTokenLimit"`
	// QueueLimit bounds retains kept while the server is unreachable (8).
	QueueLimit int `yaml:"queueLimit"`
	// SummaryCapChars bounds /memory view and memory://root (6000).
	SummaryCapChars int `yaml:"summaryCapChars"`
	// RecallTTLSeconds caches the injected recall block (default 60).
	RecallTTLSeconds int `yaml:"recallTTLSeconds"`
	// Timeouts, in milliseconds: the request default and the per-operation
	// overrides (30000 / 30000 / 60000 / 120000).
	RequestTimeoutMS int `yaml:"requestTimeoutMs"`
	RecallTimeoutMS  int `yaml:"recallTimeoutMs"`
	RetainTimeoutMS  int `yaml:"retainTimeoutMs"`
	ReflectTimeoutMS int `yaml:"reflectTimeoutMs"`
}

// merge applies a later hindsight layer over the receiver. The enums are
// validated here so a typo is reported instead of silently taking the
// default.
func (h *HindsightSettings) merge(layer HindsightSettings) error {
	if layer.APIURL != "" {
		h.APIURL = layer.APIURL
	}
	if layer.APIToken != "" {
		h.APIToken = layer.APIToken
	}
	if layer.BankID != "" {
		h.BankID = layer.BankID
	}
	if layer.BankMission != "" {
		h.BankMission = layer.BankMission
	}
	if layer.Scoping != "" {
		switch layer.Scoping {
		case "global", "per-project", "per-project-tagged":
			h.Scoping = layer.Scoping
		default:
			return fmt.Errorf("unknown hindsight.scoping %q (want global|per-project|per-project-tagged)", layer.Scoping)
		}
	}
	if layer.RetainMode != "" {
		switch layer.RetainMode {
		case "full-session", "last-turn":
			h.RetainMode = layer.RetainMode
		default:
			return fmt.Errorf("unknown hindsight.retainMode %q (want full-session|last-turn)", layer.RetainMode)
		}
	}
	if layer.RecallBudget != "" {
		switch layer.RecallBudget {
		case "low", "mid", "high":
			h.RecallBudget = layer.RecallBudget
		default:
			return fmt.Errorf("unknown hindsight.recallBudget %q (want low|mid|high)", layer.RecallBudget)
		}
	}
	if layer.AutoRecall != nil {
		h.AutoRecall = layer.AutoRecall
	}
	if layer.AutoRetain != nil {
		h.AutoRetain = layer.AutoRetain
	}
	if layer.Debug {
		h.Debug = true
	}
	for _, f := range []struct {
		src *int
		dst *int
		key string
	}{
		{&layer.RecallMaxTokens, &h.RecallMaxTokens, "recallMaxTokens"},
		{&layer.RecallContextTurns, &h.RecallContextTurns, "recallContextTurns"},
		{&layer.RecallMaxQueryChars, &h.RecallMaxQueryChars, "recallMaxQueryChars"},
		{&layer.RetainEveryNTurns, &h.RetainEveryNTurns, "retainEveryNTurns"},
		{&layer.InjectionTokenLimit, &h.InjectionTokenLimit, "injectionTokenLimit"},
		{&layer.QueueLimit, &h.QueueLimit, "queueLimit"},
		{&layer.SummaryCapChars, &h.SummaryCapChars, "summaryCapChars"},
		{&layer.RecallTTLSeconds, &h.RecallTTLSeconds, "recallTTLSeconds"},
		{&layer.RequestTimeoutMS, &h.RequestTimeoutMS, "requestTimeoutMs"},
		{&layer.RecallTimeoutMS, &h.RecallTimeoutMS, "recallTimeoutMs"},
		{&layer.RetainTimeoutMS, &h.RetainTimeoutMS, "retainTimeoutMs"},
		{&layer.ReflectTimeoutMS, &h.ReflectTimeoutMS, "reflectTimeoutMs"},
	} {
		if *f.src == 0 {
			continue
		}
		if *f.src < 0 {
			return fmt.Errorf("hindsight.%s must be positive, got %d", f.key, *f.src)
		}
		*f.dst = *f.src
	}
	return nil
}

type Settings struct {
	Theme string `yaml:"theme"`
	// ColorBlindMode moves the palette's red/green-only distinctions
	// (error/success, diff add/remove) onto a blue/orange pair that
	// survives protanopia and deuteranopia (M12 F4). Applied to the
	// resolved theme at load.
	ColorBlindMode bool `yaml:"colorBlindMode"`
	// StatusLine configures the TUI HUD (M12 F5, omp's status-line
	// segment model): statusLine.segments lists the segments to render,
	// in order. Unset keeps the shipped layout.
	StatusLine   *StatusLineSettings `yaml:"statusLine"`
	DefaultModel string              `yaml:"defaultModel"`
	ApprovalMode string              `yaml:"approvalMode"` // always-ask|write|yolo
	// Prewalk turns the one-shot model handoff on for every run
	// (prewalk.enabled); prewalk.into is the handoff target (a model ref or
	// @role, default @smol). The -prewalk flag forces it on and
	// -no-prewalk forces it off.
	Prewalk PrewalkSettings `yaml:"prewalk"`
	// Models tunes runtime model switching (models.cycle): the ordered
	// patterns Ctrl+P / --models cycles through.
	Models      ModelsSettings `yaml:"models"`
	MemoryLimit int64          `yaml:"memoryLimit"`
	MaxTurns    int            `yaml:"maxTurns"`
	// Compaction tunes context maintenance (compaction.methodOrder).
	Compaction CompactionSettings `yaml:"compaction"`
	// Retry tunes the resilience ladder (M5 #25): the fallback chain
	// table, the usage-reserve policy, and the revert-to-primary policy.
	Retry      RetrySettings     `yaml:"retry"`
	ModelRoles map[string]string `yaml:"modelRoles"`
	// Handoff tunes the handoff-document compaction (M5 #23): the
	// compaction.methodOrder member `handoff` and its artifacts.
	Handoff HandoffSettings `yaml:"handoff"`
	// ToolsApproval sets an action per tool (allow|deny|prompt).
	ToolsApproval map[string]string `yaml:"toolsApproval"`
	// BashPatterns are ordered command rules, "deny:rm -rf *" style.
	BashPatterns []string `yaml:"bashPatterns"`
	// Bash is the `bash` group (M13 #56): the compound-command opt-in and
	// the external interceptor.
	Bash BashSettings `yaml:"bash"`
	// ModelRolesEffort pins a reasoning effort per role (":effort" suffix
	// on a @role reference overrides it).
	ModelRolesEffort map[string]string `yaml:"modelRolesEffort"`
	// Memory selects the long-term memory backend (M12 F1, M15 #73): "off"
	// (default), "local" (MEMORY.md + learned.md under the data dir, with the
	// memory:// read seam and the learn tool), "mnemopi" (local SQLite store
	// with banks, a fact link graph and polyphonic recall, M12 #44),
	// "hindsight" (remote Hindsight HTTP server, M12 #43), or "sharpshooter"
	// (friction-gated decision files under <dataDir>/memories, consolidated in
	// the background through the @smol role). Every backend exposes the same
	// Store seam, so memory://, the learn tool and /memory work unchanged.
	Memory string `yaml:"memory"`
	// MemoryPipeline enables the local backend's two-phase consolidation
	// pipeline (M12 #13): "on" or "off" (the default). The YAML key is
	// top-level memoryPipeline, because the omp-style dotted memory.pipeline
	// path cannot sit next to the memory backend scalar; the pipeline is
	// inert unless memory is also "local" (it needs somewhere to write).
	MemoryPipeline string `yaml:"memoryPipeline"`
	// Mnemopi tunes the mnemopi SQLite backend (M12 #44); see
	// MnemopiSettings and MnemopiConfig.
	Mnemopi MnemopiSettings `yaml:"memoryMnemopi"`
	// Hindsight configures the remote Hindsight memory backend (M12 #43):
	// the sandboxed group mirrors omp's hindsight.* keys, and the HINDSIGHT_*
	// environment variables override them at the backend (see
	// internal/memory/hindsight.go for the precedence table).
	Hindsight HindsightSettings `yaml:"hindsight"`
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
	// Debug declares debug adapters for the debug tool (M15 #66). Absent =
	// the built-in adapters (dlv, debugpy, lldb-dap), all started lazily on
	// the first launch or attach.
	Debug *DebugConfig `yaml:"debug"`
	// WebSearch configures the web_search provider chain (M13 #48):
	// ordered providers, per-provider timeout, API keys.
	WebSearch WebSearchSettings `yaml:"webSearch"`
	// Browser configures the browser tool (M13 #50): the CDP discovery
	// endpoint of an already-running Chrome. xdev never launches a browser,
	// so an empty block means "attach to 127.0.0.1:9222".
	Browser BrowserSettings `yaml:"browser"`
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
	// ImageProviders configures the generate_image chain (M15 #69):
	// ordered cloud image providers, per-attempt timeout, response cap.
	// Absent = the built-in order (openai, gemini) with environment keys.
	ImageProviders imagegen.Settings `yaml:"imageProviders"`
	// TTS configures the tts tool and `xdev say` (M15 #68): voice name
	// and words-per-minute rate, both backend-relative, both
	// overridable per call.
	TTS TTSSettings `yaml:"tts"`
	// Plugins configures the marketplace/plugin manager (M13 #53):
	// marketplaces lists catalog locations (a local directory or a git URL)
	// for `xdev plugin list|search|install`. Installed plugins live in
	// <dataDir>/plugins and contribute commands/skills/agents/hooks at the
	// lowest discovery priority.
	Plugins PluginsSettings `yaml:"plugins"`
	// Computer gates the computer tool (M15 #65): desktop control
	// (screenshot, pointer, keyboard) is opt-in, because synthetic input
	// is a real-world side effect.
	Computer ComputerSettings `yaml:"computer"`
}

// TTSSettings is the `tts` group. It is the engine's own Settings type,
// aliased rather than redeclared (same reason as WebSearchSettings: the
// struct the registry hands to the tool must live in internal/tts).
type TTSSettings = tts.Settings

// TTSConfig returns the tts group, nil-safe.
func (s *Settings) TTSConfig() tts.Settings {
	if s == nil {
		return tts.Settings{}
	}
	return s.TTS
}

// SkillsSettings is the `skills` group.
type SkillsSettings struct {
	// CustomDirectories are extra `<dir>/<name>/SKILL.md` roots. A
	// relative entry resolves against the project cwd; a missing or
	// unreadable directory is skipped, never fatal.
	CustomDirectories []string `yaml:"customDirectories"`
}

// StatusLineSettings is the `statusLine` group (M12 F5). Segments is the
// HUD segment order; the TUI knows the vocabulary (model, tokens, context,
// cost, theme) and skips unknown names with a warning.
type StatusLineSettings struct {
	Segments StringList `yaml:"segments"`
}

// StringList is a list that also decodes the comma-separated scalar
// `xdev config set` writes: a plain []string would reject the CLI's own
// output and get the user's config quarantined as broken (the same trap
// MethodOrderSetting documents).
type StringList []string

// UnmarshalYAML accepts a sequence (["a","b"]) or a scalar ("a,b").
func (l *StringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		v := strings.TrimSpace(node.Value)
		if v == "" {
			*l = nil
			return nil
		}
		parts := strings.Split(v, ",")
		out := make(StringList, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		*l = out
		return nil
	case yaml.SequenceNode:
		var out []string
		if err := node.Decode(&out); err != nil {
			return err
		}
		*l = out
		return nil
	}
	return fmt.Errorf("statusLine.segments must be a list or a comma-separated value")
}

// StatusLineSegments returns the configured HUD segments in display order
// (nil = the shipped layout, which the TUI owns).
func (s *Settings) StatusLineSegments() []string {
	if s == nil || s.StatusLine == nil {
		return nil
	}
	return s.StatusLine.Segments
}

// PluginsSettings is the `plugins` group.
type PluginsSettings struct {
	// Marketplaces are catalog locations in precedence order: a local
	// directory holding a catalog manifest, or a git URL that is cloned into
	// <dataDir>/plugins/marketplaces. A relative path resolves against the
	// process working directory.
	Marketplaces []string `yaml:"marketplaces"`
}

// ComputerSettings is the `computer` group.
type ComputerSettings struct {
	// Enabled opts into desktop control. A pointer so a later layer can
	// turn it back off through the zero-skip merge; nil means off (the
	// shipped default).
	Enabled *bool `yaml:"enabled"`
	// Timeout bounds one op in seconds (0 = the tool's own default).
	Timeout int `yaml:"timeout"`
}

// BashSettings is the `bash` group (M13 #56).
type BashSettings struct {
	// AllowCompoundCommands matches a compound command against bashPatterns
	// as one string before falling back to per-segment resolution. A
	// pointer, so a project layer can express "off" over a global "on";
	// nil/unset keeps the shipped default (off) — see
	// tool.ApprovalPolicy.AllowCompoundCommands for the tradeoff.
	AllowCompoundCommands *bool `yaml:"allowCompoundCommands"`
	// Interceptor is an external command that reviews a proposed bash
	// command before the approval policy runs (hook-style: JSON request on
	// stdin, JSON verdict on stdout). Empty means off, and any failure of
	// a configured interceptor denies the call; see
	// internal/tool/interceptor.go.
	Interceptor string `yaml:"interceptor"`
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

// DebugConfig is the debug: settings block (M15 #66).
type DebugConfig struct {
	// Timeout is a Go duration ("30s") bounding one DAP request and one
	// post-continue stop wait.
	Timeout string `yaml:"timeout"`
	// Adapters is keyed by adapter id (dlv, debugpy, lldb-dap, or any
	// custom name). An entry for a built-in id merges onto it, so
	// overriding just the command keeps the built-in languages.
	Adapters map[string]DebugAdapter `yaml:"adapters"`
}

// DebugAdapter is one debug adapter command.
type DebugAdapter struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// Languages are the languages this adapter serves (go, python, c, cpp,
	// rust, or a custom name), used to pick an adapter from the debugged
	// file's extension when none is named.
	Languages []string `yaml:"languages"`
	// Socket marks an adapter that answers DAP on a TCP port instead of
	// stdio: xdev listens on a loopback port and passes
	// --client-addr=<host:port> so the adapter dials back (dlv's mode).
	Socket bool `yaml:"socket"`
}

// WebSearchSettings is the webSearch config block: ordered providers,
// per-provider timeout, API keys (M13 #48). It is the engine's own Settings
// type, aliased rather than redeclared — internal/config imports
// internal/tool through internal/agent, so the struct the registry hands to
// the tool cannot live in either of those packages without an import cycle.
type WebSearchSettings = websearch.Settings

// BrowserSettings is the browser: config block (M13 #50). Same alias rule as
// WebSearchSettings: the engine (internal/browser) owns the struct.
type BrowserSettings = browser.Settings

// defaultSettings is the schema-defaults layer.
// memoryOrDefault reports the effective memory backend ("" = off).
func memoryOrDefault(v string) string {
	if v == "" {
		return "off"
	}
	return v
}

// ValidMemoryBackend reports whether name selects a shipped memory backend
// ("" is the off default). The settings validation and the `xdev config set
// memory` CLI both read the vocabulary from here, so the two lists cannot
// drift apart.
func ValidMemoryBackend(name string) bool {
	switch name {
	case "", "off", "local", "mnemopi", "hindsight", "sharpshooter":
		return true
	}
	return false
}

// PrewalkSettings is the prewalk.* group: the handoff default and its
// target. The -prewalk / -no-prewalk flags override Enabled; -prewalk-into
// overrides Into.
type PrewalkSettings struct {
	Enabled bool   `yaml:"enabled"`
	Into    string `yaml:"into"`
}

// ModelsSettings is the models.* group. Cycle is the ordered pattern list
// Ctrl+P (and --models) advances through: a pattern matches a model ref or
// bare id by substring, and the first catalog entry wins.
type ModelsSettings struct {
	Cycle []string `yaml:"cycle"`
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
		Prewalk:            PrewalkSettings{Into: "@smol"},
		// The group ships enabled but rule-less (no rules = inert).
		TTSR: &TTSRSettings{Enabled: &show, InterruptMode: "always", ContextMode: "discard", RepeatGap: 3},
	}
}

// ShowThinkingOn reports whether thinking output should be displayed;
// unset follows the schema default (on).
func (s *Settings) ShowThinkingOn() bool {
	return s == nil || s.ShowThinking == nil || *s.ShowThinking
}

// AllowCompoundCommandsOn reports the effective bash.allowCompoundCommands
// (nil-safe: the shipped default is off, and a layer that never set it
// can't turn it on).
func (s *Settings) AllowCompoundCommandsOn() bool {
	return s != nil && s.Bash.AllowCompoundCommands != nil && *s.Bash.AllowCompoundCommands
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

// CompactionIdleAfter reports the idle-compaction gap; 0 disables the
// trigger (nil-safe; a malformed value never reaches storage — merge rejects
// it — so this only guards hand-built Settings).
func (s *Settings) CompactionIdleAfter() time.Duration {
	if s == nil {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(s.Compaction.IdleAfter))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// CompactionAsyncOn reports the compaction.async setting (nil-safe: the
// shipped default is synchronous).
func (s *Settings) CompactionAsyncOn() bool {
	return s != nil && s.Compaction.Async != nil && *s.Compaction.Async
}

// idleAfterOrDefault renders the idle-compaction gap for the settings list
// ("off" when the trigger is disabled).
func idleAfterOrDefault(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "off"
	}
	return raw
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

// ImageGenSettings is the imageProviders config block: generate_image's own
// Settings type, aliased rather than redeclared for the same import-cycle
// reason as WebSearchSettings (M15 #69).
type ImageGenSettings = imagegen.Settings

// ImageGenConfig returns the imageProviders block with ${VAR} references in
// each provider expanded (settings files, unlike models.yml, are not
// env-expanded at load time), so an inline key can live in the environment
// without being written into config.yml. Nil-safe: the zero value is the
// built-in provider order, credentialed from the environment.
func (s *Settings) ImageGenConfig() imagegen.Settings {
	if s == nil {
		return imagegen.Settings{}
	}
	cfg := s.ImageProviders
	if len(cfg.Providers) > 0 {
		cfg.Providers = append([]imagegen.Provider(nil), cfg.Providers...)
		for i := range cfg.Providers {
			cfg.Providers[i].APIKey = Resolve(cfg.Providers[i].APIKey)
			cfg.Providers[i].BaseURL = Resolve(cfg.Providers[i].BaseURL)
		}
	}
	return cfg
}

// HandoffSaveToDisk reports whether handoff documents are mirrored to disk
// (nil-safe: no layer means no artifacts).
func (s *Settings) HandoffSaveToDisk() bool {
	return s != nil && s.Handoff.SaveToDisk
}

// BrowserConfig returns the browser block; the zero value is the default
// endpoint on Chrome's standard debugging port (same nil tolerance as
// WebSearchConfig for pre-main callers).
func (s *Settings) BrowserConfig() browser.Settings {
	if s == nil {
		return browser.Settings{}
	}
	return s.Browser
}

// AskTimeout returns the ask tool's headless wait: ask.timeout seconds,
// or the tool's schema default when unset.
func (s *Settings) AskTimeout() time.Duration {
	if s == nil || s.Ask.Timeout <= 0 {
		return tool.DefaultAskTimeout
	}
	return time.Duration(s.Ask.Timeout) * time.Second
}

// ComputerOn reports whether the computer tool is enabled (nil-safe:
// desktop control is off unless a layer explicitly opts in).
func (s *Settings) ComputerOn() bool {
	return s != nil && s.Computer.Enabled != nil && *s.Computer.Enabled
}

// ComputerTimeout bounds one computer op (nil-safe: 0 = the tool's own
// default of 15s).
func (s *Settings) ComputerTimeout() time.Duration {
	if s == nil || s.Computer.Timeout <= 0 {
		return 0
	}
	return time.Duration(s.Computer.Timeout) * time.Second
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
	if layer.ColorBlindMode {
		// bool with a false default: only a layer that turns it ON
		// contributes (same rule as advisor).
		s.ColorBlindMode = true
	}
	if layer.StatusLine != nil {
		if s.StatusLine == nil {
			s.StatusLine = &StatusLineSettings{}
		}
		if layer.StatusLine.Segments != nil {
			// A layer that lists segments means exactly that list.
			s.StatusLine.Segments = append([]string(nil), layer.StatusLine.Segments...)
		}
	}
	if layer.DefaultModel != "" {
		s.DefaultModel = layer.DefaultModel
	}
	if layer.ApprovalMode != "" {
		s.ApprovalMode = layer.ApprovalMode
	}
	if layer.Prewalk.Enabled {
		s.Prewalk.Enabled = true
	}
	if layer.Prewalk.Into != "" {
		s.Prewalk.Into = layer.Prewalk.Into
	}
	if len(layer.Models.Cycle) > 0 {
		s.Models.Cycle = append([]string(nil), layer.Models.Cycle...)
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
	// compaction.idleAfter is validated here so a typo ("10 min") is
	// reported instead of silently disabling the idle trigger; a
	// non-positive duration is rejected the same way.
	if v := strings.TrimSpace(layer.Compaction.IdleAfter); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("compaction.idleAfter %q: %w", v, err)
		}
		if d <= 0 {
			return fmt.Errorf("compaction.idleAfter %q: want a positive duration (e.g. 10m)", v)
		}
		s.Compaction.IdleAfter = v
	}
	if layer.Compaction.Async != nil {
		s.Compaction.Async = layer.Compaction.Async
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
	if layer.Bash.AllowCompoundCommands != nil {
		s.Bash.AllowCompoundCommands = layer.Bash.AllowCompoundCommands
	}
	if layer.Bash.Interceptor != "" {
		s.Bash.Interceptor = layer.Bash.Interceptor
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
	if layer.Mnemopi != (MnemopiSettings{}) {
		s.Mnemopi = mergeMnemopi(s.Mnemopi, layer.Mnemopi)
	}
	// The hindsight group validates its enums while merging, so a typo is
	// reported against the layer that wrote it.
	if err := s.Hindsight.merge(layer.Hindsight); err != nil {
		return err
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
	if layer.Handoff.SaveToDisk {
		// Same plain-bool rule as Advisor: only a layer that turns it ON
		// contributes (the shipped default is off).
		s.Handoff.SaveToDisk = true
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
	if layer.TTS.Voice != "" {
		s.TTS.Voice = layer.TTS.Voice
	}
	if layer.TTS.Rate != 0 {
		if err := tts.ValidateRate(layer.TTS.Rate); err != nil {
			return err
		}
		s.TTS.Rate = layer.TTS.Rate
	}
	if layer.Plugins.Marketplaces != nil {
		s.Plugins.Marketplaces = append([]string(nil), layer.Plugins.Marketplaces...)
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
	// debug: adapters merge per id (a layer overriding one adapter keeps the
	// others), and the timeout is validated here so a typo is reported
	// instead of silently falling back to the default.
	if layer.Debug != nil {
		if s.Debug == nil {
			s.Debug = &DebugConfig{}
		}
		if layer.Debug.Timeout != "" {
			if _, err := time.ParseDuration(layer.Debug.Timeout); err != nil {
				return fmt.Errorf("debug.timeout %q: %w", layer.Debug.Timeout, err)
			}
			s.Debug.Timeout = layer.Debug.Timeout
		}
		if len(layer.Debug.Adapters) > 0 && s.Debug.Adapters == nil {
			s.Debug.Adapters = map[string]DebugAdapter{}
		}
		for k, v := range layer.Debug.Adapters {
			s.Debug.Adapters[k] = v
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
	// imageProviders: the provider list is replaced wholesale (the same rule
	// as webSearch — an overlay that names one provider means exactly that
	// chain), the timeout and the size cap are validated here, and an
	// unknown adapter name is reported rather than silently dropped from
	// the chain.
	if layer.ImageProviders.Providers != nil {
		s.ImageProviders.Providers = append([]imagegen.Provider(nil), layer.ImageProviders.Providers...)
	}
	if layer.ImageProviders.Timeout != "" {
		if _, err := time.ParseDuration(layer.ImageProviders.Timeout); err != nil {
			return fmt.Errorf("imageProviders.timeout %q: %w", layer.ImageProviders.Timeout, err)
		}
		s.ImageProviders.Timeout = layer.ImageProviders.Timeout
	}
	if layer.ImageProviders.MaxBytes < 0 {
		return fmt.Errorf("imageProviders.maxBytes must be positive, got %d", layer.ImageProviders.MaxBytes)
	}
	if layer.ImageProviders.MaxBytes != 0 {
		s.ImageProviders.MaxBytes = layer.ImageProviders.MaxBytes
	}
	for _, p := range s.ImageProviders.Providers {
		switch strings.ToLower(strings.TrimSpace(p.Name)) {
		case imagegen.OpenAI, imagegen.Gemini:
		default:
			return fmt.Errorf("imageProviders: unknown provider %q (want %s|%s)", p.Name, imagegen.OpenAI, imagegen.Gemini)
		}
	}
	// Retry group last: its validation must see the fully merged layer.
	if err := s.mergeRetry(layer); err != nil {
		return err
	}
	// browser: the endpoint must be a usable URL (a typo would otherwise
	// surface as a confusing "no Chrome DevTools endpoint" at call time),
	// and the timeout is bounded where it is set.
	if v := strings.TrimSpace(layer.Browser.CDPURL); v != "" {
		if err := validateCDPURL(v); err != nil {
			return err
		}
		s.Browser.CDPURL = v
	}
	if layer.Browser.Timeout != 0 {
		if layer.Browser.Timeout < 0 {
			return fmt.Errorf("browser.timeout must be positive, got %d", layer.Browser.Timeout)
		}
		s.Browser.Timeout = layer.Browser.Timeout
	}
	if layer.Ask.Timeout != 0 {
		s.Ask.Timeout = layer.Ask.Timeout
	}
	// computer: desktop control is opt-in (a layer that sets enabled wins,
	// including an explicit no), and the timeout is validated here so a
	// negative value is reported instead of silently meaning "default".
	if layer.Computer.Enabled != nil {
		s.Computer.Enabled = layer.Computer.Enabled
	}
	if layer.Computer.Timeout != 0 {
		if layer.Computer.Timeout < 0 {
			return fmt.Errorf("computer.timeout must be >= 0, got %d", layer.Computer.Timeout)
		}
		s.Computer.Timeout = layer.Computer.Timeout
	}
	if !ValidMemoryBackend(s.Memory) {
		return fmt.Errorf("unknown memory %q (want off|local|mnemopi|hindsight|sharpshooter)", s.Memory)
	}
	switch s.MemoryPipeline {
	case "", "on", "off":
	default:
		return fmt.Errorf("unknown memoryPipeline %q (want on|off)", s.MemoryPipeline)
	}
	switch s.Mnemopi.Scope {
	case "", "global", "project", "project-tagged":
	default:
		return fmt.Errorf("unknown memoryMnemopi.scope %q (want global|project|project-tagged)", s.Mnemopi.Scope)
	}
	switch s.Mnemopi.LLMMode {
	case "", "smol", "remote", "none":
	default:
		return fmt.Errorf("unknown memoryMnemopi.llmMode %q (want smol|remote|none)", s.Mnemopi.LLMMode)
	}
	if s.Mnemopi.Scope == "project-tagged" && s.Mnemopi.Tag == "" {
		return fmt.Errorf("memoryMnemopi.scope project-tagged needs memoryMnemopi.tag")
	}
	// retainEveryNTurns uses -1 for "no turn trigger", so it is checked
	// separately from the sizes (which must not be negative).
	for _, f := range []struct {
		key string
		val int
	}{
		{"memoryMnemopi.recallLimit", s.Mnemopi.RecallLimit},
		{"memoryMnemopi.injectionTokenLimit", s.Mnemopi.InjectionTokenLimit},
		{"memoryMnemopi.queueDrainMillis", s.Mnemopi.QueueDrainMillis},
	} {
		if f.val < 0 {
			return fmt.Errorf("%s must not be negative, got %d", f.key, f.val)
		}
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

// validateCDPURL checks browser.cdpUrl: either an http(s)/ws(s) URL or the
// host:port shorthand the browser tool normalizes. Caught here so a typo is
// reported at load instead of as a confusing "nothing to attach to" later.
func validateCDPURL(v string) error {
	if !strings.Contains(v, "://") {
		if _, _, err := net.SplitHostPort(v); err != nil {
			return fmt.Errorf("browser.cdpUrl %q: want host:port or an http(s):// URL", v)
		}
		return nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("browser.cdpUrl %q: %w", v, err)
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return fmt.Errorf("browser.cdpUrl %q: unsupported scheme %q", v, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("browser.cdpUrl %q has no host", v)
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

// List renders the resolved settings one key: value per line, in a stable
// order: map-backed groups (roles, per-role effort, per-tool approval) are
// sorted by key, and every grouped key the session actually enforces is
// listed, minus hook bodies — those carry arbitrary commands, so only the
// configured event count is reported. Credential-bearing values are never
// prewalkIntoOrDefault renders the handoff target for the settings list.
func prewalkIntoOrDefault(v string) string {
	if strings.TrimSpace(v) == "" {
		return "@smol"
	}
	return v
}

// cycleOrDefault renders the models.cycle pattern list ("off" when unset).
func cycleOrDefault(v []string) string {
	if len(v) == 0 {
		return "off"
	}
	return strings.Join(v, ",")
}

// part of Settings, but any key whose name suggests a secret is masked.
func List(s *Settings, globalPath string) []string {
	mm := s.MnemopiConfig()
	out := []string{
		"theme " + s.Theme,
		"colorBlindMode " + fmt.Sprint(s.ColorBlindMode),
		"approvalMode " + s.ApprovalMode,
		"prewalk.enabled " + fmt.Sprint(s.Prewalk.Enabled),
		"prewalk.into " + prewalkIntoOrDefault(s.Prewalk.Into),
		"models.cycle " + cycleOrDefault(s.Models.Cycle),
		"bash.allowCompoundCommands " + fmt.Sprint(s.AllowCompoundCommandsOn()),
		"maxTurns " + fmt.Sprint(s.MaxTurns),
		"memoryLimit " + fmt.Sprint(s.MemoryLimit),
		"showThinking " + fmt.Sprint(s.ShowThinkingOn()),
		"computer " + fmt.Sprint(s.ComputerOn()),
		"advisor " + fmt.Sprint(s.Advisor),
		"memory " + memoryOrDefault(s.Memory),
		"memoryPipeline " + memoryOrDefault(s.MemoryPipeline),
		"memoryMnemopi.scope " + mm.Scope,
		"memoryMnemopi.llmMode " + mm.LLMMode,
		"memoryMnemopi.retainEveryNTurns " + fmt.Sprint(mm.RetainEveryNTurns),
		"memoryMnemopi.recallLimit " + fmt.Sprint(mm.RecallLimit),
		"memoryMnemopi.injectionTokenLimit " + fmt.Sprint(mm.InjectionTokenLimit),
		"memoryMnemopi.queueDrainMillis " + fmt.Sprint(mm.QueueDrainMillis),
		"personality " + s.Personality,
		"compaction.methodOrder " + s.CompactionMethodOrder(),
		"compaction.idleAfter " + idleAfterOrDefault(s.Compaction.IdleAfter),
		"compaction.async " + fmt.Sprint(s.CompactionAsyncOn()),
	}
	if segs := s.StatusLineSegments(); segs != nil {
		out = append(out, "statusLine.segments "+strings.Join(segs, ","))
	}
	// The bank tag only exists for the project-tagged scope; an empty one
	// would just print a dangling key.
	if mm.Tag != "" {
		out = append(out, "memoryMnemopi.tag "+mm.Tag)
	}
	out = append(out, "handoff.saveToDisk "+fmt.Sprint(s.HandoffSaveToDisk()))
	if s.DefaultModel != "" {
		out = append(out, "defaultModel "+s.DefaultModel)
	}
	for _, k := range sortedKeys(s.ModelRoles) {
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
	for _, k := range sortedKeys(s.ModelRolesEffort) {
		out = append(out, "modelRolesEffort."+k+" "+s.ModelRolesEffort[k])
	}
	for _, k := range sortedKeys(s.ToolsApproval) {
		out = append(out, "toolsApproval."+k+" "+s.ToolsApproval[k])
	}
	if len(s.BashPatterns) > 0 {
		out = append(out, "bashPatterns "+strings.Join(s.BashPatterns, ", "))
	}
	if len(s.Hooks) > 0 {
		out = append(out, fmt.Sprintf("hooks %d configured", len(s.Hooks)))
	}
	out = append(out, "config "+globalPath)
	if len(s.DisabledProviders) > 0 {
		out = append(out, "disabledProviders "+strings.Join(s.DisabledProviders, ","))
	}
	if len(s.EnabledProviders) > 0 {
		out = append(out, "enabledProviders "+strings.Join(s.EnabledProviders, ","))
	}
	// The hindsight group is only surfaced when it is in play: an unset
	// block on a local/off setup would be noise. The token is never
	// printed, only whether one is configured.
	if s.Memory == "hindsight" || s.Hindsight.APIURL != "" {
		out = append(out, "hindsight.apiUrl "+hindsightOrDefault(s.Hindsight.APIURL),
			"hindsight.scoping "+hindsightOrDefault(s.Hindsight.Scoping),
			"hindsight.bankId "+hindsightOrDefault(s.Hindsight.BankID))
		if s.Hindsight.APIToken != "" {
			out = append(out, "hindsight.apiToken (set)")
		}
	}
	if s.Ask.Timeout > 0 {
		out = append(out, "ask.timeout "+fmt.Sprint(s.Ask.Timeout))
	}
	if s.TTS.Voice != "" {
		out = append(out, "tts.voice "+s.TTS.Voice)
	}
	if s.TTS.Rate != 0 {
		out = append(out, "tts.rate "+fmt.Sprint(s.TTS.Rate))
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

// hindsightOrDefault renders an unset hindsight key as the default marker
// (unlike memoryOrDefault, "" here is not "off").
func hindsightOrDefault(v string) string {
	if v == "" {
		return "(default)"
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
