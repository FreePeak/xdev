package tool

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Approval policy resolution (M9 #10 / M3 follow-up #16): per-tool rules
// plus ordered bash glob patterns, resolved deny > prompt > allow, and
// consulted by the agent loop through the Prompter seam.
//
// Compound commands are resolved conservatively: `a && rm -rf /` is judged
// on BOTH sides, and the strictest verdict wins, so an innocuous prefix
// can't smuggle a denied command.

// Action is one policy verdict for a single tool invocation.
type Action int

const (
	// ActionAllow runs the call.
	ActionAllow Action = iota
	// ActionPrompt asks the user first.
	ActionPrompt
	// ActionDeny refuses the call.
	ActionDeny
)

func (a Action) String() string {
	switch a {
	case ActionAllow:
		return "allow"
	case ActionPrompt:
		return "prompt"
	case ActionDeny:
		return "deny"
	}
	return fmt.Sprintf("Action(%d)", int(a))
}

// ParseAction reads a policy token (allow|deny|prompt).
func ParseAction(s string) (Action, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return ActionAllow, nil
	case "deny":
		return ActionDeny, nil
	case "prompt", "ask":
		return ActionPrompt, nil
	}
	return ActionAllow, fmt.Errorf("tool: unknown action %q (want allow|deny|prompt)", s)
}

// PolicyRule is one ordered pattern rule for bash.
type PolicyRule struct {
	Pattern string // glob over the command text, e.g. "git push *"
	Action  Action
}

// ApprovalPolicy is the resolved approval configuration. Its zero Mode is
// Yolo — the product default, matching an agent built without a policy
// (subagent children, tests). Strict modes arrive only via settings.
type ApprovalPolicy struct {
	Mode ApprovalMode
	// PerTool overrides the mode for one tool (allow|deny|prompt).
	PerTool map[string]Action
	// BashPatterns is ORDERED: the first matching rule decides.
	BashPatterns []PolicyRule
}

// ModeOf exposes a policy's mode for tests and diagnostics.
func ModeOf(p ApprovalPolicy) ApprovalMode { return p.Mode }

// Decision is a policy verdict plus why, so a denial can be explained.
type Decision struct {
	Action Action
	Reason string
}

// Decide resolves one invocation. Precedence:
//
//	deny rule > explicit per-tool rule > bash pattern > mode+tier default
//
// A tool name outside the tier table (grep/glob/ast tools, ext_*/mcp_*)
// classifies conservatively as TierExec, so dynamically registered tools
// are subject to the same policy as bash instead of silently exempt — the
// most destructive assumption is the only safe default for a tool nobody
// modeled. Decide errors only on malformed policy state.
func (p ApprovalPolicy) Decide(toolName string, args json.RawMessage) (Decision, error) {
	tier := TierExec // conservative default for unmodeled tools
	if t, err := Classify(toolName, args); err == nil {
		tier = t
	}
	if toolName == "bash" {
		cmd := bashCommand(args)
		// deny is absolute: no per-tool allow or mode can lift it.
		for _, r := range p.BashPatterns {
			if r.Action == ActionDeny && matchCommand(r.Pattern, cmd) {
				return Decision{Action: ActionDeny, Reason: "bash.patterns deny " + quote(r.Pattern)}, nil
			}
		}
	}
	if act, ok := p.PerTool[toolName]; ok {
		switch act {
		case ActionDeny:
			return Decision{Action: ActionDeny, Reason: "tools.approval denies " + toolName}, nil
		case ActionPrompt:
			return Decision{Action: ActionPrompt, Reason: "tools.approval prompts for " + toolName}, nil
		case ActionAllow:
			// An explicit allow still cannot bypass a bash deny above.
			return Decision{Action: ActionAllow}, nil
		}
	}
	if toolName == "bash" {
		cmd := bashCommand(args)
		for _, r := range p.BashPatterns {
			if r.Action != ActionPrompt {
				continue
			}
			if matchCommand(r.Pattern, cmd) {
				return Decision{Action: ActionPrompt, Reason: "bash.patterns prompt " + quote(r.Pattern)}, nil
			}
		}
	}
	if NeedsApproval(p.Mode, tier) {
		return Decision{Action: ActionPrompt, Reason: "approval mode " + p.Mode.String() + " requires a prompt for " + tier.String() + " tools"}, nil
	}
	return Decision{Action: ActionAllow}, nil
}

// bashCommand extracts the command text from bash arguments.
func bashCommand(args json.RawMessage) string {
	var v struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &v)
	return strings.TrimSpace(v.Command)
}

// matchCommand reports whether a rule matches a (possibly compound)
// command. Compound operators split the command, and the rule must match a
// whole segment — a prefix like "echo x; git push" still hits "git push *".
func matchCommand(pattern, command string) bool {
	pattern = strings.TrimSpace(pattern)
	command = strings.TrimSpace(command)
	if pattern == "" || command == "" {
		return false
	}
	for _, seg := range splitCompound(command) {
		if globMatch(pattern, seg) {
			return true
		}
	}
	return false
}

// splitCompound breaks a shell line on the operators that sequence
// commands. Quotes are respected so `echo "a && b"` stays one segment.
func splitCompound(command string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte
	s := command
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == ';' || (c == '&' && i+1 < len(s) && s[i+1] == '&') || (c == '|' && i+1 < len(s) && s[i+1] == '|'):
			segs = append(segs, strings.TrimSpace(cur.String()))
			cur.Reset()
			if c != ';' {
				i++ // consume the second char of && / ||
			}
		case c == '\n':
			segs = append(segs, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	segs = append(segs, strings.TrimSpace(cur.String()))
	out := make([]string, 0, len(segs))
	for _, g := range segs {
		if g != "" {
			out = append(out, g)
		}
	}
	return out
}

// globMatch matches command text with `*` (any run, INCLUDING spaces and
// path separators, and possibly empty) and `?` (one rune).
//
// This is deliberately NOT path.Match: that documents `*` as "any sequence
// of non-Separator characters", so a rule like `rm -rf *` would silently
// never match `rm -rf /tmp/x` — a deny rule that quietly does nothing is
// worse than no rule at all (a test caught exactly this).
//
// A pattern with no metacharacters matches the whole command or its first
// word-run, so `git` covers `git push origin main` but not `target`; and a
// trailing ` *` behaves the same way, so `git push *` also denies a bare
// `git push`.
func globMatch(pattern, name string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return name == pattern || strings.HasPrefix(name, pattern+" ")
	}
	if globHere(pattern, name) {
		return true
	}
	// "cmd *" means "this command", with or without arguments: a deny rule
	// must not miss the bare form (the trailing-space-star is the idiomatic
	// way people write it).
	if head, ok := strings.CutSuffix(pattern, " *"); ok && globHere(head, name) {
		return true
	}
	return false
}

// globHere is the backtracking matcher for the two supported wildcards.
func globHere(pat, name string) bool {
	pi, ni := 0, 0
	starPat, starName := -1, -1
	for ni < len(name) {
		switch {
		case pi < len(pat) && pat[pi] == '*':
			starPat, starName = pi, ni
			pi++
		case pi < len(pat) && (pat[pi] == '?' || pat[pi] == name[ni]):
			pi++
			ni++
		case starPat >= 0:
			// Backtrack: the last * absorbs one more byte.
			starName++
			pi = starPat + 1
			ni = starName
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}

func quote(s string) string { return "\"" + s + "\"" }

// ParsePolicyRules builds ordered bash rules from config strings
// ("deny:rm -rf *", "prompt:git push *"). A bare pattern defaults to prompt.
func ParsePolicyRules(entries []string) ([]PolicyRule, error) {
	var out []PolicyRule
	for i, e := range entries {
		raw := strings.TrimSpace(e)
		if raw == "" {
			continue
		}
		act := ActionPrompt
		head, tail, hasPrefix := strings.Cut(raw, ":")
		if hasPrefix {
			a, err := ParseAction(head)
			if err != nil {
				return nil, fmt.Errorf("bash.patterns[%d]: %w", i, err)
			}
			act, raw = a, strings.TrimSpace(tail)
		}
		if raw == "" {
			return nil, fmt.Errorf("bash.patterns[%d]: empty pattern", i)
		}
		out = append(out, PolicyRule{Pattern: raw, Action: act})
	}
	return out, nil
}
