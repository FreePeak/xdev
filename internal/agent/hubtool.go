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
	return "coordinate background agents and processes: list/steer/park jobs (list/jobs/send/wait/cancel/park/result) and supervise named processes (start/ps/logs/stop/restart/describe)"
}

func (t *HubTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "op": {"type": "string", "enum": ["jobs", "send", "wait", "cancel", "result", "park", "list", "inbox", "start", "ps", "logs", "stop", "restart", "describe"], "description": "which coordination action to take"},
    "id": {"type": "string", "description": "job id (send/cancel/result/park/inbox); omitted = all jobs (wait/jobs/list/inbox)"},
    "text": {"type": "string", "description": "steering message for send (revives a parked job)"},
    "ids": {"type": "array", "items": {"type": "string"}, "description": "job ids for wait (empty = any job)"},
    "timeout": {"type": "number", "description": "wait timeout in seconds (default 60)"},
    "name": {"type": "string", "description": "process name (start/stop/restart/describe/logs)"},
    "application": {"type": "string", "description": "start: executable path"},
    "args": {"type": "array", "items": {"type": "string"}, "description": "start: application arguments"},
    "cwd": {"type": "string", "description": "start: working directory (default: inherit)"},
    "ready_log": {"type": "string", "description": "start: regex over captured output that marks readiness"},
    "ready_port": {"type": "number", "description": "start: TCP port that must accept a connection"},
    "ready_timeout": {"type": "number", "description": "start: readiness wait in seconds (default 30)"},
    "lines": {"type": "number", "description": "logs: recent lines to return (default 100)"},
    "signal": {"type": "string", "description": "stop: SIGTERM (default), SIGINT, or SIGKILL"}
  },
  "required": ["op"]
}`)
}

func (t *HubTool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	if t.Hub == nil {
		return tool.Result{Text: "hub: not configured", IsError: true}, nil
	}
	var a struct {
		Op           string   `json:"op"`
		ID           string   `json:"id"`
		Text         string   `json:"text"`
		IDs          []string `json:"ids"`
		Timeout      float64  `json:"timeout"`
		Name         string   `json:"name"`
		Application  string   `json:"application"`
		AppArgs      []string `json:"args"`
		CWD          string   `json:"cwd"`
		ReadyLog     string   `json:"ready_log"`
		ReadyPort    int      `json:"ready_port"`
		ReadyTimeout float64  `json:"ready_timeout"`
		Lines        int      `json:"lines"`
		Signal       string   `json:"signal"`
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

	case "park":
		if strings.TrimSpace(a.ID) == "" {
			return tool.Result{Text: "hub park: id is required", IsError: true}, nil
		}
		if !t.Hub.Park(a.ID) {
			return tool.Result{Text: "hub: job " + a.ID + " not parked (must be settled)", IsError: true}, nil
		}
		return tool.Result{Text: "parked " + a.ID + " (send or revive brings it back)"}, nil

	case "list":
		rows := t.Hub.Roster()
		if len(rows) == 0 {
			return tool.Result{Text: "no background agents"}, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%-8s %-8s %-14s %-34s %s\n", "ID", "STATUS", "MODEL", "ACTIVITY", "COST")
		for _, r := range rows {
			fmt.Fprintf(&b, "%-8s %-8s %-14s %-34s %s\n",
				r.ID, r.Status, r.Model, rosterSnippet(r.Activity, 34), rosterCost(r))
		}
		return tool.Result{Text: strings.TrimRight(b.String(), "\n")}, nil

	case "inbox":
		inbox := t.Hub.Inbox(a.ID)
		if len(inbox) == 0 {
			return tool.Result{Text: "no steering messages recorded"}, nil
		}
		var b strings.Builder
		for jobID, msgs := range inbox {
			for _, m := range msgs {
				fmt.Fprintf(&b, "%s  %s  %s\n", jobID, m.At.Format("15:04:05"), rosterSnippet(m.Text, 80))
			}
		}
		return tool.Result{Text: strings.TrimRight(b.String(), "\n")}, nil

	case "start":
		if a.Application == "" {
			return tool.Result{Text: "hub start: application is required", IsError: true}, nil
		}
		spec := ProcSpec{
			Name:         a.Name,
			App:          a.Application,
			Args:         a.AppArgs,
			CWD:          a.CWD,
			ReadyLog:     a.ReadyLog,
			ReadyPort:    a.ReadyPort,
			ReadyTimeout: time.Duration(a.ReadyTimeout * float64(time.Second)),
		}
		info, err := t.Hub.Procs().Start(ctx, spec)
		if err != nil {
			return tool.Result{Text: "hub start: " + err.Error(), IsError: true}, nil
		}
		return tool.Result{Text: renderProcInfo(info)}, nil

	case "ps":
		infos := t.Hub.Procs().PS()
		if len(infos) == 0 {
			return tool.Result{Text: "no supervised processes"}, nil
		}
		var b strings.Builder
		for _, info := range infos {
			b.WriteString(renderProcInfo(info) + "\n")
		}
		return tool.Result{Text: strings.TrimRight(b.String(), "\n")}, nil

	case "logs":
		if a.Name == "" {
			return tool.Result{Text: "hub logs: name is required", IsError: true}, nil
		}
		lines, ok := t.Hub.Procs().Logs(a.Name, a.Lines)
		if !ok {
			return tool.Result{Text: "hub: unknown process " + a.Name, IsError: true}, nil
		}
		if len(lines) == 0 {
			return tool.Result{Text: "(no output captured)"}, nil
		}
		return tool.Result{Text: strings.Join(lines, "\n")}, nil

	case "stop":
		if a.Name == "" {
			return tool.Result{Text: "hub stop: name is required", IsError: true}, nil
		}
		if err := t.Hub.Procs().Stop(a.Name, a.Signal); err != nil {
			return tool.Result{Text: "hub stop: " + err.Error(), IsError: true}, nil
		}
		return tool.Result{Text: "stopped " + a.Name}, nil

	case "restart":
		if a.Name == "" {
			return tool.Result{Text: "hub restart: name is required", IsError: true}, nil
		}
		info, err := t.Hub.Procs().Restart(ctx, a.Name)
		if err != nil {
			return tool.Result{Text: "hub restart: " + err.Error(), IsError: true}, nil
		}
		return tool.Result{Text: renderProcInfo(info)}, nil

	case "describe":
		if a.Name == "" {
			return tool.Result{Text: "hub describe: name is required", IsError: true}, nil
		}
		info, ok := t.Hub.Procs().Describe(a.Name)
		if !ok {
			return tool.Result{Text: "hub: unknown process " + a.Name, IsError: true}, nil
		}
		spec, _ := json.Marshal(t.Hub.Procs().SpecOf(a.Name))
		return tool.Result{Text: renderProcInfo(info) + "\nspec: " + string(spec)}, nil

	default:
		return tool.Result{Text: "hub: unknown op " + a.Op + " (jobs|send|wait|cancel|result|park|list|inbox|start|ps|logs|stop|restart|describe)", IsError: true}, nil
	}
}

// renderProcInfo formats one process snapshot for tool output.
func renderProcInfo(info ProcInfo) string {
	s := fmt.Sprintf("%s  %-8s pid=%d %s", info.Name, info.Status, info.PID, info.App)
	if info.Ready != "" {
		s += " ready=" + info.Ready
	}
	if info.Err != "" {
		s += " err=" + info.Err
	}
	return s
}

// rosterSnippet trims s to max runes for table columns.
func rosterSnippet(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// rosterCost renders the cost column: USD when the provider priced the
// run, otherwise a token estimate, otherwise "-".
func rosterCost(r RosterEntry) string {
	if r.Cost > 0 {
		return fmt.Sprintf("$%.4f", r.Cost)
	}
	if r.Tokens > 0 {
		return fmt.Sprintf("~%.1fk tok", float64(r.Tokens)/1000)
	}
	return "-"
}
