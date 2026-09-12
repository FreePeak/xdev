// Package computer implements the computer tool (M15 #65): desktop control
// — screenshot, pointer, keyboard, focused window — by shelling out to the
// platform's own tooling. Nothing here links a native framework, so the
// single static CGO-free binary and the <100 MB RSS budget both hold.
//
// The backend is chosen by GOOS at run time (backend_macos.go,
// backend_x11.go, backend_powershell.go — named without a GOOS suffix so
// all three compile everywhere: the cross-compile matrix stays green and
// one test run covers every backend's command mapping). Every backend
// shares the same invariants:
//
//   - computer.enabled must be true (the shipped default is off, because
//     synthetic input is a real-world side effect);
//   - coordinates are clamped to the size the backend reports;
//   - one op is bounded by computer.timeout (default 15s);
//   - a missing platform tool is an actionable error naming the install,
//     never a crash.
package computer

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png" // screenshot dimensions come from the stdlib PNG header
	"os"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/tool"
)

const (
	// DefaultTimeout bounds one op when settings pin nothing.
	DefaultTimeout = 15 * time.Second
	// maxScreenshotBytes refuses a capture above this size: a 6K display
	// at Retina scale stays well under it, while a runaway capture does
	// not silently fill the disk.
	maxScreenshotBytes = 64 << 20
	// maxTextBytes bounds one `type` payload (every character is a
	// synthetic key event).
	maxTextBytes = 4096
	// maxScroll bounds one scroll op in wheel lines.
	maxScroll = 100
)

// Config is the tool's configuration, resolved from the `computer`
// settings group.
type Config struct {
	// Enabled is the computer.enabled gate: every op is refused while it
	// is false (the shipped default).
	Enabled bool
	// Timeout bounds one op (<= 0 = DefaultTimeout).
	Timeout time.Duration
	// Dir holds screenshots ("" = os.TempDir()).
	Dir string
}

// FromSettings maps the `computer` settings group onto Config. Nil-safe:
// pre-main callers and tests get the disabled default.
func FromSettings(s *config.Settings) Config {
	return Config{Enabled: s.ComputerOn(), Timeout: s.ComputerTimeout()}
}

// Tool is the `computer` tool. Ops are serialized: two synthetic input
// events must never interleave, and a screenshot must not race a click.
type Tool struct {
	cfg     Config
	backend backend
	// runFn executes one argv; tests replace it to assert the command
	// without stubbing a platform binary. nil = runCmd.
	runFn func(ctx context.Context, argv []string) ([]byte, error)
	mu    sync.Mutex
}

// NewTool returns the computer tool for the running OS.
func NewTool(cfg Config) *Tool {
	return &Tool{cfg: cfg, backend: defaultBackend()}
}

// Name implements tool.Tool.
func (t *Tool) Name() string { return "computer" }

// Description implements tool.Tool.
func (t *Tool) Description() string {
	return "Control the desktop: screenshot (captures to a PNG file and returns its path), click/move at x,y, type text, press a key chord (\"ctrl+c\"), scroll (dx/dy wheel lines), or read the focused window. Disabled unless computer.enabled is true; coordinates are clamped to the reported screen; one op is bounded by computer.timeout (default 15s). macOS also needs Accessibility for input and Screen Recording for screenshots."
}

// Parameters implements tool.Tool.
func (t *Tool) Parameters() json.RawMessage { return json.RawMessage(paramSchema) }

const paramSchema = `{
  "type": "object",
  "properties": {
    "op": {
      "type": "string",
      "enum": ["screenshot", "click", "move", "type", "key", "scroll", "window"],
      "description": "action to perform"
    },
    "x": {"type": "integer", "description": "click/move: screen x (points, 0 = left edge)"},
    "y": {"type": "integer", "description": "click/move: screen y (points, 0 = top edge)"},
    "dx": {"type": "integer", "description": "scroll: horizontal wheel lines (positive = right)"},
    "dy": {"type": "integer", "description": "scroll: vertical wheel lines (positive = down)"},
    "text": {"type": "string", "description": "type: text to type (newlines press Return)"},
    "key": {"type": "string", "description": "key: chord like \"ctrl+c\", \"cmd+shift+s\", \"enter\"; use shift+ for capitals"}
  },
  "required": ["op"]
}`

// args is the decoded tool call.
type args struct {
	Op   string `json:"op"`
	X    *int   `json:"x"`
	Y    *int   `json:"y"`
	DX   *int   `json:"dx"`
	DY   *int   `json:"dy"`
	Text string `json:"text"`
	Key  string `json:"key"`
}

// Execute implements tool.Tool. Failures are results (the model must see
// them), not Go errors; only malformed arguments are a harness error.
func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return tool.Result{}, fmt.Errorf("computer: bad arguments: %w", err)
	}
	if !t.cfg.Enabled {
		return failf("computer is disabled: desktop control is off by default — enable it with `xdev config set computer.enabled true` (%s)", config.GlobalSettingsPath()), nil
	}
	op := op(strings.ToLower(strings.TrimSpace(a.Op)))
	if !op.valid() {
		return failf("computer: unknown op %q (want %s)", a.Op, strings.Join(opNames, ", ")), nil
	}

	// One op, one deadline, one at a time: the size query, the preflight
	// and the event itself all live inside the same budget.
	t.mu.Lock()
	defer t.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()

	// Argument checks, then the platform gate, then whatever needs the
	// platform (the screen size, the screenshot file): a malformed call
	// never touches the machine, and a closed gate is reported before a
	// command can fail obscurely.
	req, err := validate(op, a)
	if err != nil {
		return failf("%v", err), nil
	}
	if op.needsInput() {
		if err := t.preflight(ctx); err != nil {
			return failf("%v", err), nil
		}
	}
	req, err = t.resolve(ctx, req)
	if err != nil {
		return failf("%v", err), nil
	}
	argv, err := t.backend.Argv(req)
	if err != nil {
		return failf("%v", err), nil
	}
	out, err := t.run(ctx, argv)
	if err != nil {
		return failf("%v", err), nil
	}
	return t.result(req, out)
}

// preflight runs the backend's gate command (if it has one) and interprets
// its answer. Without this check macOS drops every synthetic event without
// an error, which would look like a successful click that never happened.
func (t *Tool) preflight(ctx context.Context) error {
	argv, err := t.backend.Preflight()
	if err != nil || argv == nil {
		return err
	}
	out, err := t.run(ctx, argv)
	if err != nil {
		return err
	}
	return t.backend.ParsePreflight(out)
}

// validate checks one call's arguments and returns the request with
// everything that needs no platform query filled in. Coordinates and
// scroll deltas are kept raw here; resolve clamps them against the size
// the backend reports.
func validate(o op, a args) (request, error) {
	req := request{op: o}
	switch o {
	case opClick, opMove:
		if a.X == nil || a.Y == nil {
			return req, fmt.Errorf("computer: %s needs integer x and y", o)
		}
		req.x, req.y = *a.X, *a.Y
	case opType:
		if a.Text == "" {
			return req, fmt.Errorf("computer: type needs text")
		}
		if len(a.Text) > maxTextBytes {
			return req, fmt.Errorf("computer: text is %d bytes, over the %d-byte cap — send it in smaller pieces", len(a.Text), maxTextBytes)
		}
		req.text = a.Text
	case opKey:
		chord, err := parseCombo(a.Key)
		if err != nil {
			return req, err
		}
		req.chord = chord
	case opScroll:
		dx, dy := value(a.DX), value(a.DY)
		if dx == 0 && dy == 0 {
			return req, fmt.Errorf("computer: scroll needs a non-zero dx or dy")
		}
		req.dx, req.dy = dx, dy
	}
	return req, nil
}

// resolve fills in what needs the platform: the clamped coordinates, the
// clamped scroll, and the screenshot's destination file.
func (t *Tool) resolve(ctx context.Context, req request) (request, error) {
	switch req.op {
	case opScreenshot:
		f, err := os.CreateTemp(t.dir(), "xdev-screenshot-*.png")
		if err != nil {
			return req, fmt.Errorf("computer: screenshot: %w", err)
		}
		req.path = f.Name()
		if err := f.Close(); err != nil {
			return req, fmt.Errorf("computer: screenshot: %w", err)
		}
	case opClick, opMove:
		x, y, size, err := t.clamp(ctx, req.x, req.y)
		if err != nil {
			return req, err
		}
		if x != req.x || y != req.y {
			req.note = fmt.Sprintf(" (clamped from %d,%d)", req.x, req.y)
		}
		req.x, req.y, req.screen = x, y, size
	case opScroll:
		dx, dy := clampRange(req.dx, -maxScroll, maxScroll), clampRange(req.dy, -maxScroll, maxScroll)
		if dx != req.dx || dy != req.dy {
			req.note = fmt.Sprintf(" (clamped from dx=%d dy=%d)", req.dx, req.dy)
		}
		req.dx, req.dy = dx, dy
	}
	return req, nil
}

// clamp fits x,y into the pointer space the backend reports. A failed size
// query fails the op: an unclamped coordinate would land outside every
// display, which is worse than refusing.
func (t *Tool) clamp(ctx context.Context, x, y int) (int, int, size, error) {
	out, err := t.run(ctx, t.backend.SizeArgv())
	if err != nil {
		return 0, 0, size{}, err
	}
	s, err := t.backend.ParseSize(out)
	if err != nil {
		return 0, 0, size{}, err
	}
	return clampRange(x, 0, s.w-1), clampRange(y, 0, s.h-1), s, nil
}

// result renders the op's outcome. The `window` op passes the backend's
// own output through; a screenshot reports the file, never its bytes.
func (t *Tool) result(req request, out []byte) (tool.Result, error) {
	details := map[string]any{"op": string(req.op), "backend": t.backend.Name()}
	var text string
	var isErr bool
	switch req.op {
	case opScreenshot:
		text, details, isErr = screenshotResult(req.path, details)
	case opWindow:
		text = strings.TrimSpace(string(out))
		if text == "" {
			return tool.Result{Text: "computer: the backend reported no focused window", IsError: true, Details: details}, nil
		}
	case opClick, opMove:
		text = fmt.Sprintf("%s %d,%d (screen %dx%d)", req.op, req.x, req.y, req.screen.w, req.screen.h)
	case opScroll:
		text = fmt.Sprintf("scroll dx=%d dy=%d", req.dx, req.dy)
	case opType:
		text = fmt.Sprintf("typed %d bytes", len(req.text))
	case opKey:
		text = "key " + req.chord.String()
	}
	if req.note != "" {
		text += req.note
		details["clamped"] = true
	}
	return tool.Result{Text: text, Details: details, IsError: isErr}, nil
}

// screenshotResult turns the captured file into the result: path, real
// dimensions (the stdlib PNG header, so no image decoding into memory),
// and size. An empty file is the shape a denied Screen Recording grant
// takes on macOS, so it gets its own message; every failure branch also
// flags the result as an error, so the model never reads a missing capture
// as success.
func screenshotResult(path string, details map[string]any) (string, map[string]any, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		os.Remove(path)
		return fmt.Sprintf("computer: screenshot produced no file at %s: %v", path, err), details, true
	}
	switch {
	case fi.Size() == 0:
		os.Remove(path)
		return fmt.Sprintf("computer: screenshot file %s is empty — on macOS the process running xdev needs a Screen Recording grant (System Settings → Privacy & Security → Screen Recording)", path), details, true
	case fi.Size() > maxScreenshotBytes:
		os.Remove(path)
		return fmt.Sprintf("computer: screenshot is %d bytes, over the %d-byte cap: %s", fi.Size(), maxScreenshotBytes, path), details, true
	}
	dims := ""
	if f, err := os.Open(path); err == nil {
		if cfg, _, err := image.DecodeConfig(f); err == nil {
			dims = fmt.Sprintf("%dx%d, ", cfg.Width, cfg.Height)
		}
		f.Close()
	}
	details["path"] = path
	details["bytes"] = fi.Size()
	return fmt.Sprintf("screenshot %s (%s%.1f MB) — read it with the read tool", path, dims, float64(fi.Size())/(1<<20)), details, false
}

// run executes one argv through the tool's runner seam.
func (t *Tool) run(ctx context.Context, argv []string) ([]byte, error) {
	if t.runFn != nil {
		return t.runFn(ctx, argv)
	}
	return runCmd(ctx, argv)
}

// timeout is the per-op bound: computer.timeout, else DefaultTimeout.
func (t *Tool) timeout() time.Duration {
	if t.cfg.Timeout <= 0 {
		return DefaultTimeout
	}
	return t.cfg.Timeout
}

// dir is where screenshots land.
func (t *Tool) dir() string {
	if t.cfg.Dir != "" {
		return t.cfg.Dir
	}
	return os.TempDir()
}

// failf builds an error Result.
func failf(format string, a ...any) tool.Result {
	return tool.Result{Text: fmt.Sprintf(format, a...), IsError: true}
}

func value(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// clampRange bounds v to [lo, hi]; an inverted range yields lo.
func clampRange(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	}
	return v
}
