package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubBashInterceptor writes an executable interceptor script whose body is
// the given POSIX shell lines, and returns its path.
func stubBashInterceptor(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "interceptor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBashInterceptorVerdicts(t *testing.T) {
	// The verdict decides the call, and every failure mode denies: a
	// reviewer that breaks must stop the agent, never wave a command
	// through. The table asserts both halves — the verdict, and that an
	// error is returned (the callers deny on error and on nothing else).
	cases := []struct {
		name    string
		body    string
		timeout time.Duration
		want    Action
		wantErr string // substring of the fail-closed error; "" = no error
		wantCmd string // the rewritten command, when the stub rewrites
	}{
		{name: "allow", body: `echo '{"action":"allow"}'`, want: ActionAllow},
		{name: "deny", body: `echo '{"action":"deny","reason":"no shell for you"}'`, want: ActionDeny},
		{name: "prompt", body: `echo '{"action":"prompt","reason":"unsure"}'`, want: ActionPrompt},
		{name: "rewrite", body: `echo '{"action":"allow","rewrite":"git status"}'`, want: ActionAllow, wantCmd: "git status"},
		{name: "non-zero exit", body: `echo '{"action":"allow"}'; exit 3`, wantErr: "fail-closed deny"},
		{name: "malformed stdout", body: `echo not a verdict`, wantErr: "not a verdict"},
		{name: "unknown action", body: `echo '{"action":"maybe"}'`, wantErr: "unknown action"},
		{name: "empty stdout", body: `true`, wantErr: "not a verdict"},
		{name: "hang", body: `sleep 30`, timeout: 200 * time.Millisecond, wantErr: "fail-closed deny"},
		{name: "oversized rewrite", body: `printf '{"action":"allow","rewrite":"%s"}' "$(head -c 5000 /dev/zero | tr '\0' x)"`, wantErr: "over the"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ApprovalPolicy{BashInterceptor: BashInterceptor{
				Command: stubBashInterceptor(t, tc.body),
				Timeout: tc.timeout,
			}}
			dec, rewritten, err := p.ReviewBash(context.Background(), policyBashArgs(t, "npm test && rm -rf /tmp/x"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("failure must deny (fail-closed): err = %v", err)
				}
				if len(rewritten) != 0 {
					t.Fatalf("a failed interceptor rewrote the command: %s", rewritten)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dec.Action != tc.want {
				t.Fatalf("verdict = %s, want %s (%s)", dec.Action, tc.want, dec.Reason)
			}
			if tc.wantCmd == "" {
				return
			}
			var v bashArgs
			if err := json.Unmarshal(rewritten, &v); err != nil || v.Command != tc.wantCmd {
				t.Fatalf("rewrite = %s (err %v), want command %q", rewritten, err, tc.wantCmd)
			}
		})
	}
}

func TestBashInterceptorRequestShape(t *testing.T) {
	// The reviewer is handed the same command the policy will judge, the
	// directory it will run in, and the segments, so its verdict can be
	// about the actual call.
	out := filepath.Join(t.TempDir(), "request.json")
	t.Setenv("STUB_BASH_INTERCEPTOR_OUT", out)
	p := ApprovalPolicy{BashInterceptor: BashInterceptor{
		Command: stubBashInterceptor(t, `cat > "$STUB_BASH_INTERCEPTOR_OUT"; echo '{"action":"allow"}'`),
	}}
	args, err := json.Marshal(map[string]string{"command": "npm test && rm -rf /tmp/x", "workdir": "/tmp/work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReviewBash(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Command  string   `json:"command"`
		Cwd      string   `json:"cwd"`
		Segments []string `json:"segments"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("request is not JSON: %v (%s)", err, raw)
	}
	if req.Command != "npm test && rm -rf /tmp/x" {
		t.Errorf("command = %q", req.Command)
	}
	if req.Cwd != "/tmp/work" {
		t.Errorf("cwd = %q, want the call's workdir", req.Cwd)
	}
	if len(req.Segments) != 2 || req.Segments[1] != "rm -rf /tmp/x" {
		t.Errorf("segments = %q", req.Segments)
	}
}

func TestBashInterceptorRewriteKeepsCallSettings(t *testing.T) {
	// A rewrite replaces the command only: the call's timeout, workdir and
	// background flag survive, so the reviewer cannot (accidentally) turn a
	// bounded foreground call into a detached one.
	p := ApprovalPolicy{BashInterceptor: BashInterceptor{
		Command: stubBashInterceptor(t, `echo '{"action":"allow","rewrite":"git status"}'`),
	}}
	args := json.RawMessage(`{"command":"git push","timeout":30,"workdir":"/w","run_in_background":true}`)
	_, rewritten, err := p.ReviewBash(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var v bashArgs
	if err := json.Unmarshal(rewritten, &v); err != nil {
		t.Fatal(err)
	}
	if v.Command != "git status" || v.Timeout == nil || *v.Timeout != 30 || v.Workdir != "/w" || !v.RunInBackground {
		t.Fatalf("rewrite changed more than the command: %s", rewritten)
	}
}

func TestBashInterceptorOffIsInert(t *testing.T) {
	// The zero value spawns nothing and invents no verdict...
	var p ApprovalPolicy
	dec, rewritten, err := p.ReviewBash(context.Background(), policyBashArgs(t, "ls"))
	if err != nil || dec.Action != ActionAllow || len(rewritten) != 0 {
		t.Fatalf("unset interceptor: %s %s %v", dec.Action, rewritten, err)
	}
	// ...and a call with no command is not worth a process either.
	q := ApprovalPolicy{BashInterceptor: BashInterceptor{
		Command: stubBashInterceptor(t, `echo '{"action":"deny"}'`),
	}}
	if dec, _, err := q.ReviewBash(context.Background(), json.RawMessage(`{}`)); err != nil || dec.Action != ActionAllow {
		t.Fatalf("empty command was reviewed: %s %v", dec.Action, err)
	}
}
