package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// op is one computer action.
type op string

const (
	opScreenshot op = "screenshot"
	opClick      op = "click"
	opMove       op = "move"
	opType       op = "type"
	opKey        op = "key"
	opScroll     op = "scroll"
	opWindow     op = "window"
)

// opNames is the schema's enum order.
var opNames = []string{"screenshot", "click", "move", "type", "key", "scroll", "window"}

func (o op) valid() bool {
	for _, n := range opNames {
		if op(n) == o {
			return true
		}
	}
	return false
}

// needsInput reports whether the op synthesizes user input, and therefore
// needs the platform's display/accessibility gate to be open.
func (o op) needsInput() bool { return o != opScreenshot && o != opWindow }

// size is the pointer coordinate space a backend reports: coordinates are
// clamped into [0, w-1] x [0, h-1]. macOS reports points (the unit CGEvent
// uses) while its screenshots are in pixels, so the two differ on Retina
// by design.
type size struct{ w, h int }

// request is one validated op.
type request struct {
	op     op
	x, y   int
	dx, dy int
	text   string
	chord  combo
	path   string
	screen size
	// note reports a clamp that changed the op (" (clamped from 5000,10)").
	note string
}

// backend turns a request into a platform command. Every implementation
// shells out to tooling the OS already ships or the user installs; none
// links cgo. A backend is pure — it builds commands and parses their
// output, and the tool owns every execution — so the whole mapping is
// testable without touching the machine.
type backend interface {
	// Name identifies the backend in results ("darwin", "linux", "windows").
	Name() string
	// SizeArgv is the command whose stdout ParseSize decodes.
	SizeArgv() []string
	// ParseSize decodes SizeArgv's output.
	ParseSize(out []byte) (size, error)
	// Preflight is the command whose stdout ParsePreflight interprets, or
	// nil when the platform needs no check. A backend that can already
	// prove input is impossible (no display) returns the error instead.
	Preflight() ([]string, error)
	// ParsePreflight interprets Preflight's stdout; it is only called when
	// Preflight returned a command.
	ParsePreflight(out []byte) error
	// Argv is the command that performs req. An op the platform cannot
	// express is an error naming the limitation.
	Argv(req request) ([]string, error)
}

// defaultBackend returns the backend for the running OS.
func defaultBackend() backend {
	switch runtime.GOOS {
	case "darwin":
		return darwinBackend{}
	case "windows":
		return windowsBackend{}
	}
	return linuxBackend{}
}

// runCmd executes argv under ctx and returns stdout. Failures are
// actionable: a missing binary names what provides it, a deadline names
// the setting that bounds it.
func runCmd(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("computer: empty command")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, missingTool(argv[0])
	}
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return nil, fmt.Errorf("computer: %s timed out: one op is bounded by computer.timeout (default %s)", argv[0], DefaultTimeout)
		case errors.Is(ctx.Err(), context.Canceled):
			return nil, fmt.Errorf("computer: %s canceled", argv[0])
		}
		msg := strings.TrimSpace(stderr.String())
		if hint := annotateError(argv[0], msg); hint != "" {
			msg += "; " + hint
		}
		if msg == "" {
			return nil, fmt.Errorf("computer: %s failed: %w", argv[0], err)
		}
		return nil, fmt.Errorf("computer: %s failed: %w: %s", argv[0], err, msg)
	}
	return stdout.Bytes(), nil
}

// installHints names what provides a platform tool, so a missing binary is
// actionable instead of a bare "executable file not found".
var installHints = map[string]string{
	"screencapture": "macOS ships it at /usr/sbin/screencapture — check PATH",
	"osascript":     "macOS ships it at /usr/bin/osascript — check PATH",
	"xdotool":       "install xdotool (Debian/Ubuntu: apt install xdotool; Fedora: dnf install xdotool)",
	"import":        "install ImageMagick for `import` (Debian/Ubuntu: apt install imagemagick)",
	"powershell":    "Windows ships powershell.exe; a PowerShell 7-only host needs pwsh on PATH",
}

// missingTool is the not-found error for one platform tool.
func missingTool(name string) error {
	if hint, ok := installHints[name]; ok {
		return fmt.Errorf("computer: %s not found in PATH: %s", name, hint)
	}
	return fmt.Errorf("computer: %s not found in PATH", name)
}

// parseWxH decodes the "WIDTHxHEIGHT" line the osascript and PowerShell
// size commands print.
func parseWxH(out []byte, toolName string) (size, error) {
	txt := strings.TrimSpace(string(out))
	w, h, ok := strings.Cut(txt, "x")
	if ok {
		nw, errW := strconv.Atoi(strings.TrimSpace(w))
		nh, errH := strconv.Atoi(strings.TrimSpace(h))
		if errW == nil && errH == nil && nw > 0 && nh > 0 {
			return size{nw, nh}, nil
		}
	}
	return size{}, fmt.Errorf("computer: %s screen size: unexpected output %q (want WIDTHxHEIGHT)", toolName, txt)
}

// parseTwoInts decodes xdotool's "WIDTH HEIGHT".
func parseTwoInts(out []byte, toolName string) (size, error) {
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) == 2 {
		nw, errW := strconv.Atoi(f[0])
		nh, errH := strconv.Atoi(f[1])
		if errW == nil && errH == nil && nw > 0 && nh > 0 {
			return size{nw, nh}, nil
		}
	}
	return size{}, fmt.Errorf("computer: %s screen size: unexpected output %q (want \"WIDTH HEIGHT\")", toolName, strings.TrimSpace(string(out)))
}
