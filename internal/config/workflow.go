package config

import (
	"fmt"
	"strings"
)

// Workflow is the named-preset group (workflow.presets). xdev ships no preset
// of its own: the list is whatever the user's config declares, and an empty
// list is a valid, complete configuration.
//
// A preset is a bundle a user writes, not a pipeline xdev owns. Everything it
// names must be something xdev can actually ask opencode for per session —
// agent, model, permissions, directory — because a knob that only looks
// effective is worse than no knob. See docs/research/opencode-v2-spec.md §5.2
// for which fields session.create actually accepts.
type WorkflowSettings struct {
	Presets []WorkflowPreset `yaml:"presets"`
}

// WorkflowPreset is one named, user-declared bundle.
//
// Names are spelled the way opencode's own schema spells them (agent, model,
// permissions, location), per the TabSettings rule: a config pasted from
// opencode should load, and a harmless unknown key should not cost the user
// their whole file.
type WorkflowPreset struct {
	// Name is the lookup key. Required and unique.
	Name string `yaml:"name"`
	// Description is one line shown in the picker. It is the only thing
	// standing between a list of names and a list the user can choose from.
	Description string `yaml:"description"`
	// Backend names the service this preset drives. Empty means the preset
	// is local and runs nothing. "opencode" is the only value that works
	// today; the Claude Code wrapper is phase 2.
	Backend string `yaml:"backend"`
	// Agent is an opencode agent name. This is the context lever (plan D2):
	// the agent's prompt, model and MCP posture live in opencode's own
	// config, and xdev only names one. Required on a backend preset.
	Agent string `yaml:"agent"`
	// Directory is the working directory the session is scoped to.
	// Empty leaves it to opencode.
	Directory string `yaml:"directory"`
	// Prompt is the task text. Substituted, not templated: the value is
	// passed through verbatim so a preset can carry braces without escaping.
	Prompt string `yaml:"prompt"`
	// Context is the per-session budget. It is MANDATORY on a backend
	// preset, and mandatory is the point: a preset with no declared context
	// is the naive setup this design exists to avoid, and an absent key is
	// how that setup would quietly come back.
	//
	// A present-but-empty block is legitimate — it is a deliberate choice to
	// run on opencode's own defaults — which is why this is a pointer. A nil
	// means "the author never thought about it" and is rejected.
	Context *PresetContext `yaml:"context"`
}

// PresetContext is the part of a preset that reaches opencode as session
// state rather than as a prompt.
//
// Everything here is a field session.create accepts. Anything a preset cannot
// express (MCP servers, skills, tool sets, boot-time env) belongs to
// opencode's own configuration, reachable only by naming an agent — which is
// why there is no mcp key here.
type PresetContext struct {
	// Model pins provider and model. Nil leaves opencode's own choice.
	Model *ModelRef `yaml:"model"`
	// Permissions overrides opencode's ruleset for this session only.
	Permissions []PermissionRule `yaml:"permissions"`
}

// ModelRef is opencode's Model.Ref: id and providerID are both required there,
// and a partial one is a load error here rather than a server-side surprise.
type ModelRef struct {
	ModelID    string `yaml:"modelID"`
	ProviderID string `yaml:"providerID"`
	Variant    string `yaml:"variant"`
}

// PermissionRule is opencode's Permission.Rule: action, resource and effect
// are all required, and effect is one of three values.
type PermissionRule struct {
	Action   string `yaml:"action"`
	Resource string `yaml:"resource"`
	Effect   string `yaml:"effect"`
}

// permissionEffects is opencode's Permission.Effect enum. Kept here so the
// load error can list what was allowed, which is the difference between a
// message a user can act on and one they go and grep for.
var permissionEffects = []string{"allow", "deny", "ask"}

// knownBackends is the set of backends a preset may name. Only opencode works
// today; naming claude-code is a forward-reference error rather than a
// silently inert preset.
var knownBackends = []string{"opencode"}

// Preset looks a preset up by name. A miss is nil, not an error: callers that
// care use Workflow.Validate, and callers that don't (the picker, rendering)
// should simply render less.
func (s *Settings) Preset(name string) *WorkflowPreset {
	if s == nil {
		return nil
	}
	for i := range s.Workflow.Presets {
		if s.Workflow.Presets[i].Name == name {
			return &s.Workflow.Presets[i]
		}
	}
	return nil
}

// PresetNames lists the declared preset names in declaration order.
func (s *Settings) PresetNames() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Workflow.Presets))
	for _, p := range s.Workflow.Presets {
		out = append(out, p.Name)
	}
	return out
}

// Validate checks the workflow group. It is called from the config load path
// so a broken preset is named at startup rather than at the moment a user
// reaches for it — which is the moment it is least convenient to fix.
//
// Every message names the offending preset. A config error that does not say
// which preset is wrong is a config error the user has to bisect by hand.
func (s *Settings) Validate() error {
	if s == nil {
		return nil
	}
	seen := make(map[string]bool, len(s.Workflow.Presets))
	for i := range s.Workflow.Presets {
		p := &s.Workflow.Presets[i]
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("workflow: preset %d has no name", i+1)
		}
		if seen[p.Name] {
			return fmt.Errorf("workflow: preset %q is declared more than once", p.Name)
		}
		seen[p.Name] = true

		if p.Backend == "" {
			// A local preset drives nothing, so it owes no context budget.
			continue
		}
		if !contains(knownBackends, p.Backend) {
			return fmt.Errorf("workflow: preset %q names backend %q, which xdev cannot drive (known: %s)",
				p.Name, p.Backend, strings.Join(knownBackends, ", "))
		}
		if strings.TrimSpace(p.Agent) == "" {
			return fmt.Errorf("workflow: preset %q runs on the %s backend, so it must name an agent; "+
				"without one it would silently run on whatever opencode last used", p.Name, p.Backend)
		}
		if p.Context == nil {
			return fmt.Errorf("workflow: preset %q runs on the %s backend, so it must declare a context block "+
				"(even an empty one) — a preset with no declared budget is the setup this exists to prevent",
				p.Name, p.Backend)
		}
		if err := validatePresetContext(p); err != nil {
			return err
		}
	}
	return nil
}

func validatePresetContext(p *WorkflowPreset) error {
	if m := p.Context.Model; m != nil {
		if strings.TrimSpace(m.ModelID) == "" || strings.TrimSpace(m.ProviderID) == "" {
			return fmt.Errorf("workflow: preset %q sets a model with providerID %q / modelID %q; "+
				"opencode requires both", p.Name, m.ProviderID, m.ModelID)
		}
	}
	for j, r := range p.Context.Permissions {
		if strings.TrimSpace(r.Action) == "" || strings.TrimSpace(r.Resource) == "" {
			return fmt.Errorf("workflow: preset %q permission %d needs both an action and a resource "+
				"(got action=%q resource=%q)", p.Name, j+1, r.Action, r.Resource)
		}
		if !contains(permissionEffects, r.Effect) {
			return fmt.Errorf("workflow: preset %q permission %d has effect %q, which opencode does not have "+
				"(allowed: %s)", p.Name, j+1, r.Effect, strings.Join(permissionEffects, ", "))
		}
	}
	return nil
}

func contains(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}
