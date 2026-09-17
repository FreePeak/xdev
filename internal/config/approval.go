package config

import (
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/tool"
)

// Policy resolves the approval configuration from layered settings. An
// unknown approvalMode is rejected by LoadSettings already; an unknown
// per-tool action or malformed bash rule errors here, at startup, rather
// than silently weakening the policy mid-session.
func (s *Settings) Policy() (tool.ApprovalPolicy, error) {
	mode, err := parseApprovalMode(s.ApprovalMode)
	if err != nil {
		return tool.ApprovalPolicy{}, err
	}
	pol := tool.ApprovalPolicy{Mode: mode, PerTool: map[string]tool.Action{}}
	for name, action := range s.ToolsApproval {
		a, err := tool.ParseAction(action)
		if err != nil {
			return tool.ApprovalPolicy{}, fmt.Errorf("toolsApproval[%s]: %w", name, err)
		}
		pol.PerTool[name] = a
	}
	rules, err := tool.ParsePolicyRules(s.BashPatterns)
	if err != nil {
		return tool.ApprovalPolicy{}, err
	}
	// bash.allowCompoundCommands + bash.interceptor (M13 #56): the
	// pattern rules are shared, so both ride the resolved policy.
	pol.AllowCompoundCommands = s.AllowCompoundCommandsOn()
	if cmd := strings.TrimSpace(s.Bash.Interceptor); cmd != "" {
		pol.BashInterceptor = tool.BashInterceptor{Command: cmd}
	}
	pol.BashPatterns = rules
	return pol, nil
}

func parseApprovalMode(v string) (tool.ApprovalMode, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "yolo":
		return tool.Yolo, nil
	case "write":
		return tool.Write, nil
	case "always-ask", "alwaysask", "ask":
		return tool.AlwaysAsk, nil
	}
	return tool.Yolo, fmt.Errorf("config: unknown approvalMode %q (want always-ask|write|yolo)", v)
}
