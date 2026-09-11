package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// HubTool is the agent-facing coordination surface over background
// subagents (M11, research §2 CORE: registry + send/wait; the TUI roster
// is the NICE polish). One hub per session; the task tool registers
// background jobs in it via TaskTool.Hub.
type HubTool struct {
	Hub *Hub
}

const HubToolName = "hub"

func (t *HubTool) Name() string { return HubToolName }

func (t *HubTool) Description() string {
	return "coordinate background subagent jobs: list them (jobs), block until one settles (wait), steer a running one (send), abort one (cancel), or fetch a settled one's result (result)"
}

func (t *HubTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {"type": "string", "enum": ["jobs", "send", "wait", "cancel", "result"], "description": "which coordination action to take"},
    "id": {"type": "string", "description": "job id (send/cancel/result); omitted = all jobs (wait/jobs)"},
    "text": {"type": "string", "description": "steering message for send"},
    "ids": {"type": "array", "items": {"type": "string"}, "description": "job ids for wait (empty = any job)"},
    "timeout": {"type": "number", "description": "wait timeout in seconds (default 60)"}
  },
  "required": ["op"]
}`)
}

func (t *HubTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if t.Hub == nil {
		return tool.Result{Text: "hub: not configured", IsError: true}, nil
	}
	var a struct {
		Op      string   `json:"op"`
		ID      string   `json:"id"`
		Text    string   `json:"text"`
		IDs     []string `json:"ids"`
		Timeout float64  `json:"timeout"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "hub: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	switch a.Op {
	case "jobs":
		jobs := t.Hub.Jobs()
		if len(jobs) == 0 {
			return tool.Result{Text: "no background jobs"}, nil
		}
		var b strings.Builder
		for _, j := range jobs {
			fmt.Fprintf(&b, "%s  %-12s %s\n", j.ID, j.Status, j.Label)
		}
		return tool.Result{Text: strings.TrimRight(b.String(), "\n")}, nil

	case "send":
		if strings.TrimSpace(a.ID) == "" || strings.TrimSpace(a.Text) == "" {
			return tool.Result{Text: "hub send: id and text are required", IsError: true}, nil
		}
		if err := t.Hub.Send(a.ID, a.Text); err != nil {
			return tool.Result{Text: "hub: " + err.Error(), IsError: true}, nil
		}
		return tool.Result{Text: "steered " + a.ID}, nil

	case "wait":
		timeout := time.Duration(a.Timeout * float64(time.Second))
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		settled := t.Hub.Wait(ctx, a.IDs, timeout)
		if len(settled) == 0 {
			return tool.Result{Text: "wait: nothing settled within the timeout"}, nil
		}
		var b strings.Builder
		for _, id := range settled {
			info, _ := t.Hub.Status(id)
			fmt.Fprintf(&b, "%s  %s\n", id, info.Status)
		}
		return tool.Result{Text: strings.TrimRight(b.String(), "\n")}, nil

	case "cancel":
		if strings.TrimSpace(a.ID) == "" {
			return tool.Result{Text: "hub cancel: id is required", IsError: true}, nil
		}
		if !t.Hub.Cancel(a.ID) {
			return tool.Result{Text: "hub: job " + a.ID + " not running", IsError: true}, nil
		}
		return tool.Result{Text: "canceled " + a.ID}, nil

	case "result":
		if strings.TrimSpace(a.ID) == "" {
			return tool.Result{Text: "hub result: id is required", IsError: true}, nil
		}
		res, ok := t.Hub.Result(a.ID)
		if !ok {
			return tool.Result{Text: "hub: job " + a.ID + " has no result yet (still running?)", IsError: true}, nil
		}
		return tool.Result{
			Text:    renderSubagentResult(res),
			Details: res,
			IsError: res.Status == "failed" || res.Status == "schema-mismatch",
		}, nil

	default:
		return tool.Result{Text: "hub: unknown op " + a.Op + " (jobs|send|wait|cancel|result)", IsError: true}, nil
	}
}
