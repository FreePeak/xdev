package rules

import "testing"

// #108: `agents:` was parsed and read by nobody, so a rule its author scoped to
// one agent fired for every agent. The predicate is the whole fix that needs no
// plumbing; the consumer (ForPathAgent) plus an agent identity in
// wireAgentMode is the follow-up.
func TestRuleAppliesToAgent(t *testing.T) {
	cases := []struct {
		name   string
		agents []string
		who    string
		want   bool
	}{
		{"no scope applies to everyone", nil, "", true},
		{"no scope applies to a subagent", nil, "reviewer", true},
		{"empty list is unrestricted, not empty", []string{}, "reviewer", true},
		{"named agent matches itself", []string{"reviewer"}, "reviewer", true},
		{"named agent does not leak to main", []string{"reviewer"}, "", false},
		{"named agent does not leak to another", []string{"reviewer"}, "explore", false},
		{"match is case-insensitive", []string{"Reviewer"}, "reviewer", true},
		{"whitespace is trimmed", []string{" reviewer "}, "reviewer", true},
		{"wildcard applies to all", []string{"*"}, "anything", true},
		{"wildcard applies to main", []string{"*"}, "", true},
		{"one of several matches", []string{"reviewer", "explore"}, "explore", true},
		{"main can be named explicitly", []string{"main"}, "main", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Rule{Agents: tc.agents}
			if got := r.AppliesToAgent(tc.who); got != tc.want {
				t.Fatalf("AppliesToAgent(%q) with agents=%v = %v, want %v", tc.who, tc.agents, got, tc.want)
			}
		})
	}
}

// The two axes compose: path and agent are independent filters.
func TestForPathAndAgentCompose(t *testing.T) {
	rule := Rule{Name: "go-only-for-reviewer", Globs: []string{"*.go"}, Agents: []string{"reviewer"}}
	if !rule.MatchesPath("a.go") || !rule.AppliesToAgent("reviewer") {
		t.Fatal("fixture must match on both axes")
	}
	if !rule.MatchesPath("a.go") || rule.AppliesToAgent("") {
		t.Fatal("a reviewer-scoped rule must not apply to the main agent")
	}
	if rule.MatchesPath("a.py") {
		t.Fatal("glob must still gate independently of agent")
	}
}
