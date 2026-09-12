package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/logx"
)

// Bash interceptor (M13 #56): a settings-declared external command that
// reviews a proposed bash call BEFORE the approval policy runs (bash.interceptor).
// It is not an approver — it may deny, ask for a prompt, or rewrite the
// command, but it can never grant a call the policy would refuse, and the
// (possibly rewritten) command still travels the normal plan-mode and
// approval path.
//
// The protocol is the hook protocol: one JSON request on stdin, one JSON
// verdict on stdout.
//
//	stdin  {"command":"…","cwd":"…","segments":["…"]}
//	stdout {"action":"allow"|"deny"|"prompt","reason":"…","rewrite":"…"}
//
// Every failure mode is fail-closed: a non-zero exit, a timeout, unparseable
// stdout, an unknown action, or an over-long rewrite denies the call. There
// is deliberately no path from a failure to an allow — an interceptor that
// breaks must stop the agent, not silently wave its calls through.

// DefaultBashInterceptorTimeout bounds one interceptor run: a reviewer that
// hangs must not hang the tool loop.
const DefaultBashInterceptorTimeout = 10 * time.Second

// MaxBashInterceptorRewrite bounds an accepted rewrite. A longer one is
// denied rather than truncated: a truncated command is a different command.
const MaxBashInterceptorRewrite = 4096

// BashInterceptor is the resolved bash.interceptor setting. Its zero value
// (and a blank Command) means "no interceptor".
type BashInterceptor struct {
	// Command is the interceptor program line, run through the platform
	// shell with the request JSON on stdin (the same quoting rules as a
	// hook command).
	Command string
	// Timeout bounds one run (0 → DefaultBashInterceptorTimeout).
	Timeout time.Duration
}

// Off reports whether no interceptor is configured.
func (b BashInterceptor) Off() bool { return strings.TrimSpace(b.Command) == "" }

// ReviewBash runs the configured interceptor for one proposed bash call. It
// returns the verdict plus, when the interceptor rewrote the command, the
// arguments carrying the replacement command (nil otherwise). A non-nil
// error means DENY: the caller must fail closed, and must not execute the
// call on any other reading.
//
// The verdict's ActionAllow only means "the interceptor has no objection":
// the caller still runs the approval policy, so an interceptor can never
// hand itself the authority of an approver.
func (p ApprovalPolicy) ReviewBash(ctx context.Context, args json.RawMessage) (Decision, json.RawMessage, error) {
	in := p.BashInterceptor
	if in.Off() {
		return Decision{Action: ActionAllow}, nil, nil
	}
	cmd := bashCommand(args)
	if cmd == "" {
		return Decision{Action: ActionAllow}, nil, nil
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = DefaultBashInterceptorTimeout
	}
	// The context kills a hung interceptor; its error (a deadline or a
	// signal) surfaces as a fail-closed denial below.
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := json.Marshal(map[string]any{
		"command":  cmd,
		"cwd":      bashWorkdir(args),
		"segments": splitCompound(cmd),
	})
	if err != nil {
		return Decision{}, nil, fmt.Errorf("bash.interceptor: encode request: %w", err)
	}
	name, argv := shellCommand(in.Command)
	c := exec.CommandContext(cctx, name, argv...)
	c.Stdin = bytes.NewReader(req)
	// The environment is inherited (an interceptor may need credentials and
	// PATH like any hook); its stderr is discarded, because a verdict is
	// stdout JSON and nothing else — noise must not read as one.
	var out bytes.Buffer
	c.Stdout = &out
	if err := c.Run(); err != nil {
		return Decision{}, nil, fmt.Errorf("bash.interceptor failed (fail-closed deny): %w", err)
	}
	var verdict struct {
		Action  string `json:"action"`
		Reason  string `json:"reason"`
		Rewrite string `json:"rewrite"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &verdict); err != nil {
		return Decision{}, nil, fmt.Errorf("bash.interceptor stdout is not a verdict (fail-closed deny): %w", err)
	}
	act, err := ParseAction(verdict.Action)
	if err != nil {
		return Decision{}, nil, fmt.Errorf("bash.interceptor: %w (fail-closed deny)", err)
	}

	rewrite := strings.TrimSpace(verdict.Rewrite)
	if len(rewrite) > MaxBashInterceptorRewrite {
		return Decision{}, nil, fmt.Errorf("bash.interceptor rewrite is %d bytes, over the %d-byte bound (fail-closed deny)",
			len(rewrite), MaxBashInterceptorRewrite)
	}
	var rewritten json.RawMessage
	if rewrite != "" && act != ActionDeny {
		rewritten = RewriteBashCommand(args, rewrite)
		if len(rewritten) == 0 {
			return Decision{}, nil, fmt.Errorf("bash.interceptor: rewrite cannot be applied to %s (fail-closed deny)", quote(string(args)))
		}
		logx.Infof("bash.interceptor rewrote %s to %s", quote(cmd), quote(rewrite))
	}
	if act == ActionDeny {
		return Decision{Action: ActionDeny, Reason: "bash.interceptor denied: " + withReason(verdict.Reason)}, nil, nil
	}
	if act == ActionPrompt {
		return Decision{Action: ActionPrompt, Reason: "bash.interceptor asks for approval: " + withReason(verdict.Reason)}, rewritten, nil
	}
	logx.Debugf("bash.interceptor allowed %s (%s)", quote(cmd), withReason(verdict.Reason))
	return Decision{Action: ActionAllow}, rewritten, nil
}

// withReason keeps a denial/prompt message readable when the interceptor
// offered no reason of its own.
func withReason(reason string) string {
	if r := strings.TrimSpace(reason); r != "" {
		return r
	}
	return "no reason given"
}

// bashWorkdir is the best-effort directory the call will run in: the workdir
// argument as the model gave it (the bash tool resolves a relative one
// against its root), else the process cwd.
func bashWorkdir(args json.RawMessage) string {
	var v bashArgs
	if err := json.Unmarshal(args, &v); err == nil {
		if w := strings.TrimSpace(v.Workdir); w != "" {
			return w
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}
