package config

import (
	"fmt"
	"sort"
	"strings"
)

// Model roles (M9 #10, parity-tools-providers §F): named slots a session
// or subagent asks for instead of a literal provider/model, resolved
// through modelRoles with an optional ":effort" suffix.
//
//	@smol          → roles["smol"]                    ("onegw/dev")
//	@slow:high     → roles["slow"] + effort "high"
//	onegw/free     → literal, unchanged (no role lookup)

// RoleNames are the canonical slots (omp's set; every one is optional).
var RoleNames = []string{"default", "smol", "slow", "vision", "plan", "commit", "tiny", "task", "advisor"}

// EffortLevels are accepted, ordered low→high; "minimal" is the off switch.
var EffortLevels = []string{"minimal", "low", "medium", "high"}

// EffortTokens maps an effort name onto a reasoning token budget
// (ai.ThinkingBudget.Tokens is the transport-agnostic carrier; adapters
// translate it to their own effort vocabulary).
var EffortTokens = map[string]int{
	"minimal": 0,
	"low":     2048,
	"medium":  8192,
	"high":    16384,
}

// RoleRef is a resolved role reference.
type RoleRef struct {
	Ref    string // "provider/model"
	Effort string // "" when unset
	Role   string // the role name when it came from one
}

// ResolveModelRef expands one user-supplied model string:
//
//	""                  → the settings default, else the models.yml default
//	"@role[:effort]"    → the role's model (+ effort override), following
//	                      chained aliases (`plan: "@slow"`) with a cycle guard
//	"provider/model"    → literal
//
// Unknown roles and bad efforts are errors: silently falling back to the
// default model would run a task on a model the user did not choose.
func ResolveModelRef(s *Settings, ref string) (RoleRef, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		if s.DefaultModel != "" {
			return RoleRef{Ref: s.DefaultModel}, nil
		}
		return RoleRef{}, fmt.Errorf("config: no model configured (set defaultModel or pass -model)")
	}
	literal, effort, hasEffort := ref, "", false
	if strings.HasPrefix(ref, "@") {
		role, eff, has := strings.Cut(ref[1:], ":")
		if role == "" {
			return RoleRef{}, fmt.Errorf("config: empty role name in %q", ref)
		}
		effort, hasEffort = eff, has
		// A role may point at another role (`plan: "@slow"`), so follow the
		// chain with a visited set: an alias cycle must be reported, never
		// resolved by luck of iteration order.
		literal = ref
		for seen, depth := map[string]bool{}, 0; strings.HasPrefix(literal, "@"); depth++ {
			if depth > len(s.ModelRoles)+1 {
				return RoleRef{}, fmt.Errorf("config: role chain too deep from %q", ref)
			}
			name, suffix, hasSuffix := strings.Cut(strings.TrimPrefix(literal, "@"), ":")
			if name == "" {
				return RoleRef{}, fmt.Errorf("config: empty role name in %q", literal)
			}
			if seen[name] {
				return RoleRef{}, fmt.Errorf("config: role alias cycle at @%s (from %q)", name, ref)
			}
			seen[name] = true
			target, ok := s.ModelRoles[name]
			if !ok || target == "" {
				// Name the configured roles AND the canonical slots: a role
				// that is merely unconfigured (`@vision`) is one `config set`
				// away, so the message has to say which command does that.
				return RoleRef{}, fmt.Errorf("config: unknown role @%s (configured: %s; canonical: %s; set via xdev config set modelRoles.<name> provider/model or /model)", name, configuredRoles(s), strings.Join(RoleNames, ", "))
			}
			if hasSuffix && !isEffort(suffix) {
				return RoleRef{}, fmt.Errorf("config: unknown effort %q for @%s (want %s)", suffix, name, strings.Join(EffortLevels, "|"))
			}
			if hasSuffix {
				effort, hasEffort = suffix, true // a chained :effort overrides the outer one
			} else if effort == "" {
				if pinned, has := s.ModelRolesEffort[name]; has && isEffort(pinned) {
					effort = pinned
				}
			}
			literal = strings.TrimSpace(target)
			if literal == "" {
				return RoleRef{}, fmt.Errorf("config: role @%s resolves to empty", name)
			}
		}
	}
	if hasEffort && !isEffort(effort) {
		return RoleRef{}, fmt.Errorf("config: unknown effort %q for %q (want %s)", effort, ref, strings.Join(EffortLevels, "|"))
	}
	out := RoleRef{Ref: literal, Effort: effort}
	if strings.HasPrefix(ref, "@") {
		out.Role, _, _ = strings.Cut(strings.TrimPrefix(ref, "@"), ":")
	}
	return out, nil
}

// configuredRoles lists the role names for error messages.
func configuredRoles(s *Settings) string {
	names := make([]string, 0, len(s.ModelRoles))
	for k := range s.ModelRoles {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// IsKnownRole reports whether name is one of the canonical slots (used by
// config validation and the `config set` guard).
func IsKnownRole(name string) bool {
	for _, r := range RoleNames {
		if r == name {
			return true
		}
	}
	return false
}

func isEffort(v string) bool {
	for _, e := range EffortLevels {
		if e == v {
			return true
		}
	}
	return false
}

// EffortBudget returns the reasoning token budget for an effort name
// ("" → no thinking requested).
func EffortBudget(effort string) (tokens int, ok bool) {
	if effort == "" {
		return 0, false
	}
	n, known := EffortTokens[effort]
	if !known {
		return 0, false
	}
	return n, true
}
