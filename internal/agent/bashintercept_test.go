package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// bashInterceptorPolicy is a yolo policy whose bash interceptor is the given
// shell line: the interceptor speaks JSON in / JSON out on stdin/stdout.
func bashInterceptorPolicy(line string) tool.ApprovalPolicy {
	return tool.ApprovalPolicy{
		Mode:            tool.Yolo,
		BashInterceptor: tool.BashInterceptor{Command: line},
	}
}

// runBashCall runs one agent run whose first script calls a tool with args,
// and returns what actually reached a tool plus the model-visible results.
func runBashCall(t *testing.T, pol tool.ApprovalPolicy, approve ApprovalFunc, toolName, args string) (*[]string, *hookLog) {
	t.Helper()
	a, ran, log := policyAgent(t, []fakeScript{
		{events: toolCallEvents(toolName, args)},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("ok"), doneEvent("ok")}},
	}, pol, approve)
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatal(err)
	}
	return ran, log
}

// ranCommand decodes the command of the single bash call that ran.
func ranCommand(t *testing.T, ran []string) string {
	t.Helper()
	if len(ran) != 1 {
		t.Fatalf("tool runs = %v, want exactly one", ran)
	}
	var v struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(ran[0]), &v); err != nil {
		t.Fatal(err)
	}
	return v.Command
}

// denied reports whether the model was told the call did not run.
func denied(log *hookLog) bool {
	for _, r := range log.results {
		if strings.Contains(r, "denied") || strings.Contains(r, "refused") {
			return true
		}
	}
	return false
}

func TestAgentBashInterceptorDeniesBeforeExecuting(t *testing.T) {
	ran, log := runBashCall(t, bashInterceptorPolicy(`echo '{"action":"deny","reason":"no shell for you"}'`),
		nil, "bash", `{"command":"rm -rf /tmp/oops"}`)
	if len(*ran) != 0 {
		t.Fatalf("an interceptor denial reached the tool: %v", *ran)
	}
	if len(log.results) == 0 || !strings.Contains(log.results[0], "no shell for you") {
		t.Fatalf("model was not told why the call was denied: %v", log.results)
	}
}

func TestAgentBashInterceptorFailureDenies(t *testing.T) {
	// Fail-closed: an interceptor that exits non-zero (or hangs, or prints
	// garbage — same code path) must stop the call, never allow it.
	ran, log := runBashCall(t, bashInterceptorPolicy(`exit 7`), nil, "bash", `{"command":"ls"}`)
	if len(*ran) != 0 {
		t.Fatalf("a broken interceptor let the call through: %v", *ran)
	}
	if !denied(log) {
		t.Fatalf("model was not told: %v", log.results)
	}
}

func TestAgentBashInterceptorRewriteRunsTheReplacement(t *testing.T) {
	ran, _ := runBashCall(t, bashInterceptorPolicy(`echo '{"action":"allow","rewrite":"git status"}'`),
		nil, "bash", `{"command":"git push --force"}`)
	if got := ranCommand(t, *ran); got != "git status" {
		t.Fatalf("the tool ran %q, want the rewrite", got)
	}
}

func TestAgentBashInterceptorRewriteIsReEvaluated(t *testing.T) {
	// A rewrite replaces the command, it does not exempt it: a rewrite into
	// a denied command is denied, so an interceptor can't smuggle one in.
	pol := tool.ApprovalPolicy{
		Mode:            tool.Yolo,
		BashPatterns:    []tool.PolicyRule{{Pattern: "rm -rf *", Action: tool.ActionDeny}},
		BashInterceptor: tool.BashInterceptor{Command: `echo '{"action":"allow","rewrite":"rm -rf /"}'`},
	}
	ran, log := runBashCall(t, pol, nil, "bash", `{"command":"ls"}`)
	if len(*ran) != 0 {
		t.Fatalf("rewrite bypassed the policy: %v", *ran)
	}
	if !denied(log) {
		t.Fatalf("model was not told: %v", log.results)
	}
}

func TestAgentBashInterceptorPromptNeverBypassesApproval(t *testing.T) {
	// "prompt" forces a human verdict even under yolo...
	var reasons []string
	ran, _ := runBashCall(t, bashInterceptorPolicy(`echo '{"action":"prompt","reason":"review this"}'`),
		func(_ ai.ToolCallBlock, reason string) bool {
			reasons = append(reasons, reason)
			return true
		}, "bash", `{"command":"ls"}`)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "review this") {
		t.Fatalf("approver reasons = %v, want the interceptor's", reasons)
	}
	if len(*ran) != 1 {
		t.Fatalf("approved call did not run: %v", *ran)
	}
	// ...and with nobody to ask, it refuses instead of allowing.
	ran2, log2 := runBashCall(t, bashInterceptorPolicy(`echo '{"action":"prompt","reason":"review this"}'`),
		nil, "bash", `{"command":"ls"}`)
	if len(*ran2) != 0 {
		t.Fatalf("unattended prompt executed the tool: %v", *ran2)
	}
	if !denied(log2) {
		t.Fatalf("model was not told: %v", log2.results)
	}
}

func TestAgentBashInterceptorAllowIsNotAnApproval(t *testing.T) {
	// The interceptor is separate from approval: its "allow" only means it
	// has no objection, so the approval path still runs (and refuses here,
	// because an unattended run has no user to ask).
	pol := tool.ApprovalPolicy{
		Mode:            tool.AlwaysAsk,
		BashInterceptor: tool.BashInterceptor{Command: `echo '{"action":"allow"}'`},
	}
	ran, log := runBashCall(t, pol, nil, "bash", `{"command":"ls"}`)
	if len(*ran) != 0 {
		t.Fatalf("interceptor allow bypassed the approval path: %v", *ran)
	}
	if !denied(log) {
		t.Fatalf("model was not told: %v", log.results)
	}
}

func TestAgentBashInterceptorCannotBypassPlanMode(t *testing.T) {
	// Plan mode is resolved before the interceptor, so an interceptor that
	// would allow a shell command cannot revive a call planning already
	// refused: the interceptor only ever adds restrictions, never lifts one.
	pol := bashInterceptorPolicy(`echo '{"action":"allow","rewrite":"ls"}'`)
	a, ran, log := policyAgent(t, []fakeScript{
		{events: toolCallEvents("bash", `{"command":"rm -rf /tmp/x"}`)},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("ok"), doneEvent("ok")}},
	}, pol, nil)
	a.PlanMode = &PlanMode{Active: true}
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Fatalf("plan mode was bypassed by the interceptor: %v", *ran)
	}
	if len(log.results) == 0 || !strings.Contains(log.results[0], "plan mode") {
		t.Fatalf("model was not told plan mode refused the call: %v", log.results)
	}
}

func TestAgentBashInterceptorIsBashScoped(t *testing.T) {
	// The interceptor reviews bash calls only: a denial it never sees must
	// not leak into other tools' calls.
	ran, _ := runBashCall(t, bashInterceptorPolicy(`echo '{"action":"deny","reason":"nope"}'`),
		nil, "spy", `{}`)
	if len(*ran) != 1 {
		t.Fatalf("non-bash call was intercepted: %v", *ran)
	}
}
