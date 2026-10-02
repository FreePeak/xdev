// Package logx is the minimal leveled logger. Output goes to stderr so
// stdout stays reserved for the product (print mode text, RPC frames).
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level is log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelOff
)

var (
	mu      sync.Mutex
	out     io.Writer = os.Stderr
	level             = LevelWarn
	enabled           = false
)

// Enable turns logging on at the given level.
func Enable(l Level) {
	mu.Lock()
	defer mu.Unlock()
	enabled = true
	level = l
}

// Disable silences all logging (default for print mode).
func Disable() {
	mu.Lock()
	defer mu.Unlock()
	enabled = false
}

// SetOutput redirects log output (tests).
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	out = w
}

// SetOutputLocked redirects log output (caller holds mu).
func SetOutputLocked(w io.Writer) { out = w }

// ParseLevel maps a name to a Level.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	case "off", "none":
		return LevelOff, nil
	default:
		return LevelOff, fmt.Errorf("unknown level %q", s)
	}
}

func logf(l Level, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if !enabled || l < level {
		return
	}
	ts := time.Now().UTC().Format("15:04:05.000")
	fmt.Fprintf(out, "%s %s %s\n", ts, strings.ToUpper(l.String()), fmt.Sprintf(format, args...))
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "off"
	}
}

// Debugf logs at debug level.
func Debugf(format string, args ...any) { logf(LevelDebug, format, args...) }

// Infof logs at info level.
func Infof(format string, args ...any) { logf(LevelInfo, format, args...) }

// Warnf logs at warn level.
func Warnf(format string, args ...any) { logf(LevelWarn, format, args...) }

// Errorf logs at error level.
func Errorf(format string, args ...any) { logf(LevelError, format, args...) }
