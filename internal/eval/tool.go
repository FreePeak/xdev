package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/tool"
)

// DefaultBackgroundAfter is how long a tool call may block before the cell is
// handed to the background: the model gets a job handle instead of waiting.
const DefaultBackgroundAfter = 30 * time.Second

// Tool is the `eval` tool: one persistent python kernel per session.
type Tool struct {
	Kernel *Kernel
	Jobs   *Jobs

	// DefaultTimeout is the cell clock used when the caller omits timeout.
	DefaultTimeout time.Duration
	// BackgroundAfter hands the cell to the background once it exceeds this.
	BackgroundAfter time.Duration
}

// NewTool returns the eval tool whose kernel runs in cwd.
func NewTool(cwd string) *Tool {
	return &Tool{
		Kernel:          NewKernel(cwd),
		Jobs:            NewJobs(),
		DefaultTimeout:  DefaultCellTimeout,
		BackgroundAfter: DefaultBackgroundAfter,
	}
}

// Name implements tool.Tool.
func (t *Tool) Name() string { return "eval" }

// Description implements tool.Tool.
func (t *Tool) Description() string {
	// Terse on purpose: both the prompt recap and the native schema carry
	// this text, and PRD §1 Goal 4 budgets the system prompt at <1000 tokens
	// across every bundled tool (internal/agent/prompt.go caps each at 400
	// chars). The argument semantics live in the schema, which is the
	// authoritative channel.
	return "Persistent Python kernel: state survives between eval calls (language \"py\")."
}

// Parameters implements tool.Tool.
func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "code": {"type": "string", "description": "Python source for the cell. The last expression's value is returned."},
    "language": {"type": "string", "enum": ["py"], "description": "Kernel language; only \"py\" is supported (\"js\" is rejected)."},
    "timeout": {"type": "number", "description": "Cell time limit in seconds (default 30, 0 disables the limit, max 3600). On expiry the cell is interrupted and the kernel stays usable; a cell still running after 30s is backgrounded and its output reported on the next eval call."},
    "reset": {"type": "boolean", "description": "Wipe the kernel namespace before running the cell."}
  },
  "required": ["code"]
}`)
}

type evalArgs struct {
	Code     string   `json:"code"`
	Language string   `json:"language"`
	Timeout  *float64 `json:"timeout"`
	Reset    bool     `json:"reset"`
}

// Execute implements tool.Tool.
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in evalArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return fail("eval: invalid arguments: " + err.Error()), nil
	}
	language := strings.ToLower(strings.TrimSpace(in.Language))
	switch language {
	case "", "py", "python":
	default:
		return fail(fmt.Sprintf("eval: language %q is not supported (only \"py\")", in.Language)), nil
	}

	notices := t.drainNotices()
	if in.Reset {
		if err := t.Kernel.Reset(ctx); err != nil {
			return fail(join(notices, "eval: reset failed: "+t.kernelErr(err))), nil
		}
		if strings.TrimSpace(in.Code) == "" {
			return tool.Result{Text: join(notices, "eval: kernel namespace reset")}, nil
		}
	}
	if strings.TrimSpace(in.Code) == "" {
		return fail(join(notices, "eval: code is required")), nil
	}

	timeout, err := t.cellTimeout(in.Timeout)
	if err != nil {
		return fail(join(notices, err.Error())), nil
	}

	// The cell outlives the tool call when it is backgrounded, so the caller's
	// cancellation is handled below instead of through ctx.
	cellCtx := context.WithoutCancel(ctx)
	results := make(chan runResult, 1)
	go func() {
		out, err := t.Kernel.RunCell(cellCtx, in.Code, timeout)
		results <- runResult{out: out, err: err}
	}()

	// Only arm the background clock when the cell can outlive it: a cell that
	// dies at its own timeout is waited on so the timeout is reported inline
	// instead of through a useless handle.
	var clock <-chan time.Time
	if timeout == 0 || timeout > t.backgroundAfter() {
		timer := time.NewTimer(t.backgroundAfter())
		defer timer.Stop()
		clock = timer.C
	}

	select {
	case r := <-results:
		if r.err != nil {
			return fail(join(notices, "eval: "+t.kernelErr(r.err))), nil
		}
		return t.done(notices, r.out, timeout), nil
	case <-clock:
		job := t.Jobs.Add(in.Code)
		go func() {
			r := <-results
			out := r.out
			if r.err != nil {
				out = Outcome{Status: "error", Text: "eval: " + t.kernelErr(r.err)}
			}
			job.Finish(out)
		}()
		return tool.Result{
			Text: join(notices, fmt.Sprintf(
				"eval: cell still running after %s — backgrounded as job #%d; its output is reported on the next eval call.",
				t.backgroundAfter(), job.ID)),
			Details: map[string]any{"job_id": job.ID, "status": "running"},
		}, nil
	case <-ctx.Done():
		t.Kernel.Interrupt()
		select {
		case r := <-results:
			r.out.Interrupted = true
			return t.done(notices, r.out, timeout), nil
		case <-time.After(KillGrace):
			return fail(join(notices, "eval: interrupted")), nil
		}
	}
}

type runResult struct {
	out Outcome
	err error
}

// done renders a finished cell, annotating a timeout.
func (t *Tool) done(notices []string, out Outcome, timeout time.Duration) tool.Result {
	text := out.Text
	if out.TimedOut {
		text = join([]string{text}, fmt.Sprintf("[eval: timed out after %s; the cell was interrupted]", timeout))
	} else if out.Interrupted {
		text = join([]string{text}, "[eval: interrupted]")
	}
	if out.Status == "exited" {
		text = join([]string{text}, "[eval: kernel exited; the next cell starts a fresh interpreter]")
	}
	return tool.Result{
		Text:    join(notices, text),
		IsError: out.Status != "ok" || out.TimedOut || out.Interrupted,
		Details: map[string]any{
			"cell":        out.ID,
			"status":      out.Status,
			"duration_ms": out.Duration.Milliseconds(),
		},
	}
}

// drainNotices reports backgrounded cells that finished since the last call.
func (t *Tool) drainNotices() []string {
	var notices []string
	for _, job := range t.Jobs.DrainFinished() {
		text := strings.TrimSpace(job.Text())
		notice := fmt.Sprintf("[eval background job #%d finished: %s]", job.ID, job.State())
		if text != "" {
			notice += "\n" + text
		}
		notices = append(notices, notice)
	}
	return notices
}

func (t *Tool) cellTimeout(requested *float64) (time.Duration, error) {
	if requested == nil {
		if t.DefaultTimeout > 0 {
			return t.DefaultTimeout, nil
		}
		return DefaultCellTimeout, nil
	}
	secs := *requested
	if secs < 0 {
		return 0, errors.New("eval: timeout must be >= 0 seconds (0 disables the limit)")
	}
	switch {
	case secs == 0:
		return 0, nil
	case secs >= MaxCellTimeout.Seconds():
		return MaxCellTimeout, nil
	default:
		return time.Duration(secs * float64(time.Second)), nil
	}
}

func (t *Tool) backgroundAfter() time.Duration {
	if t.BackgroundAfter > 0 {
		return t.BackgroundAfter
	}
	return DefaultBackgroundAfter
}

func (t *Tool) kernelErr(err error) string {
	switch {
	case errors.Is(err, ErrBusy):
		return "a cell is still running in the background; its output is reported on the next eval call"
	case errors.Is(err, ErrClosed):
		return "the kernel is closed"
	default:
		return err.Error()
	}
}

// Close shuts the kernel down.
func (t *Tool) Close() error { return t.Kernel.Close() }

func fail(text string) tool.Result { return tool.Result{Text: text, IsError: true} }

func join(parts []string, last string) string {
	if last != "" {
		parts = append(parts, last)
	}
	return strings.Join(parts, "\n")
}
