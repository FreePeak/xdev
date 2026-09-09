package tool

import (
	"encoding/json"
	"fmt"
)

// Tier classifies how destructive a tool invocation is (PRD §3.6 tier model).
type Tier int

const (
	// TierReadOnly inspects state without mutating anything.
	TierReadOnly Tier = iota
	// TierWrite mutates the workspace (write, edit).
	TierWrite
	// TierExec runs arbitrary commands (bash).
	TierExec
)

// String implements fmt.Stringer.
func (t Tier) String() string {
	switch t {
	case TierReadOnly:
		return "read"
	case TierWrite:
		return "write"
	case TierExec:
		return "exec"
	default:
		return fmt.Sprintf("Tier(%d)", int(t))
	}
}

// ApprovalMode controls when the harness asks the user before running a tool.
type ApprovalMode int

const (
	// AlwaysAsk prompts for every tool call.
	AlwaysAsk ApprovalMode = iota
	// Write prompts only for TierWrite and TierExec calls.
	Write
	// Yolo never prompts (pi philosophy, xdev MVP default).
	Yolo
)

// Classify maps a tool name to its approval tier. Unknown tools are an
// error rather than a default so a registered-but-unmodeled tool cannot
// silently run without approval. bash arguments are not yet inspected
// (pattern rules are a later milestone).
func Classify(toolName string, _ json.RawMessage) (Tier, error) {
	switch toolName {
	case "read":
		return TierReadOnly, nil
	case "write", "edit":
		return TierWrite, nil
	case "bash":
		return TierExec, nil
	default:
		return TierReadOnly, fmt.Errorf("tool: unknown tool %q", toolName)
	}
}

// NeedsApproval reports whether a tool call of the given tier requires a
// user prompt under the given mode.
func NeedsApproval(mode ApprovalMode, tier Tier) bool {
	switch mode {
	case Yolo:
		return false
	case Write:
		return tier >= TierWrite
	default: // AlwaysAsk
		return true
	}
}

// DefaultApprovalMode is the MVP default (Yolo, pi philosophy).
const DefaultApprovalMode = Yolo

// Tiers returns every tier in ascending order.
func Tiers() []Tier { return []Tier{TierReadOnly, TierWrite, TierExec} }

// ApprovalModes returns every mode in ascending strictness order.
func ApprovalModes() []ApprovalMode {
	return []ApprovalMode{AlwaysAsk, Write, Yolo}
}

// String implements fmt.Stringer.
func (m ApprovalMode) String() string {
	switch m {
	case AlwaysAsk:
		return "always-ask"
	case Write:
		return "write"
	case Yolo:
		return "yolo"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}
