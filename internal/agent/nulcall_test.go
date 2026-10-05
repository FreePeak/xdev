package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/tool"
)

// Measured 2026-10-05 on onegw/opencode-space-bunny-free: 51 of 83,060 stored
// tool calls carried a U+0000 spliced into a string argument, and 3 more had it
// spliced into the tool NAME. Both halves surfaced as failures the model then
// misread — `bash: start: fork/exec /bin/bash: invalid argument` (execve(2)
// answers any argv string containing a NUL with EINVAL; the shell was never
// broken) and `unknown tool "\x00bash"`, which reads to the model as a session
// with no bash tool at all.
//
// runOneTool is the one chokepoint every executed call passes through, so the
// scrub lives there: the tool receives the call without the poison, it runs,
// and the model is never told a tool is missing.
func TestAgentNULToolCallReachesTheToolClean(t *testing.T) {
	for _, tc := range []struct {
		name     string
		callName string
		args     string
		wantCmd  string
	}{
		{"nul spliced onto the tool name", "\x00bash", `{"command":"echo ok"}`, "echo ok"},
		{"nul spliced into the command", "bash", `{"command":"echo a \u0000 b"}`, "echo a  b"},
		{"nul spliced between path segments", "bash", `{"command":"ls -1 Library/Preferences\u0000/com.apple.commerce.plist"}`, "ls -1 Library/Preferences/com.apple.commerce.plist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran, log := runNULCall(t, tc.callName, tc.args)
			if len(ran) != 1 {
				t.Fatalf("tool ran %d times, want 1: the call never resolved (results %q)", len(ran), log.results)
			}
			if strings.ContainsRune(ran[0], 0) {
				t.Fatalf("a NUL reached the tool: %q", ran[0])
			}
			var got struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(ran[0]), &got); err != nil {
				t.Fatalf("tool received undecodable args: %v (%s)", err, ran[0])
			}
			if got.Command != tc.wantCmd {
				t.Fatalf("command = %q, want %q", got.Command, tc.wantCmd)
			}
			for _, r := range log.results {
				if strings.Contains(r, "unknown tool") {
					t.Fatalf("the model was told a tool is missing: %q", r)
				}
			}
		})
	}
}

// The whole failure was diagnostic: a NUL produced a kernel-level EINVAL that
// the model rationalised into "the shell wrapper is choking on some characters"
// and then went hunting for. This pins that nothing EINVAL-shaped reaches the
// model once the scrub is in.
func TestAgentNULToolCallReportsNoExecError(t *testing.T) {
	_, log := runNULCall(t, "bash", `{"command":"ls -1 Library/Accounts \u0000 2>/dev/null"}`)
	if len(log.results) == 0 {
		t.Fatal("no tool result reached the model")
	}
	for _, r := range log.results {
		for _, bad := range []string{"invalid argument", "fork/exec"} {
			if strings.Contains(r, bad) {
				t.Fatalf("the model was shown the kernel EINVAL: %q", r)
			}
		}
	}
}

// The scrub is idempotent and must not disturb a clean call: a multi-line
// command with tabs and newlines — the real payload of `write` and `edit` —
// reaches the tool byte-identical.
func TestAgentCleanToolCallIsUntouched(t *testing.T) {
	const cmd = "for i in 1 2; do\techo \"$i\"\ndone"
	ran, _ := runNULCall(t, "bash", `{"command":"for i in 1 2; do\techo \"$i\"\ndone"}`)
	if len(ran) != 1 {
		t.Fatalf("tool ran %d times, want 1", len(ran))
	}
	var got struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(ran[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Command != cmd {
		t.Fatalf("command = %q, want %q (the scrub must not touch \\t or \\n)", got.Command, cmd)
	}
}

// runNULCall runs one agent turn that issues a single tool call and returns
// what reached the tool plus the results the model saw.
func runNULCall(t *testing.T, name, args string) ([]string, *hookLog) {
	t.Helper()
	a, ran, log := policyAgent(t, []fakeScript{
		{events: toolCallEvents(name, args)},
		{events: []ai.Event{{Type: ai.EventStart}, textEvent("ok"), doneEvent("stop")}},
	}, tool.ApprovalPolicy{Mode: tool.Yolo}, nil)
	hist := []ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock{Text: "go"}}}}
	if _, err := a.Run(context.Background(), "sys", hist); err != nil {
		t.Fatal(err)
	}
	return *ran, log
}
