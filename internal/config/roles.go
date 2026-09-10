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
//	"@role[:effort]"    → the role's model (+ effort override)
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
	if !strings.HasPrefix(ref, "@") {
		return RoleRef{Ref: ref}, nil
	}
	body := ref[1:]
	role, effort, hasEffort := strings.Cut(body, ":")
	if role == "" {
		return RoleRef{}, fmt.Errorf("config: empty role name in %q", ref)
	}
	model, ok := s.ModelRoles[role]
	if !ok || model == "" {
		available := make([]string, 0, len(s.ModelRoles))
		for k := range s.ModelRoles {
			available = append(available, k)
		}
		sort.Strings(available)
		return RoleRef{}, fmt.Errorf("config: unknown role @%s (configured: %s)", role, strings.Join(available, ", "))
	}
	out := RoleRef{Ref: model, Role: role}
	if hasEffort {
		if !isEffort(effort) {
			return RoleRef{}, fmt.Errorf("config: unknown effort %q for @%s (want %s)", effort, role, strings.Join(EffortLevels, "|"))
		}
		out.Effort = effort
	} else if pinned, has := s.ModelRolesEffort[role]; has && isEffort(pinned) {
		out.Effort = pinned
	}
	return out, nil
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
