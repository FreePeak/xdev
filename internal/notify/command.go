package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

const (
	// defaultCommandTimeout bounds a user's notification program. A program
	// slower than this is killed: a notification may not outlive the moment it
	// describes.
	defaultCommandTimeout = 5 * time.Second
	// commandOutputLimit caps how much of the program's output is kept for the
	// failure message.
	commandOutputLimit = 512
)

// CommandSink hands the event to a program the user configured, as JSON on
// stdin.
//
// It exists for what the terminal escapes cannot do: push to a phone, log to a
// file, play a sound, chain another tool. The wire format is the whole Event
// rather than a positional argument list, so a command never has to re-parse
// prose or guess where the title ended.
//
// The command runs under a deadline and with its stdin closed behind it, so a
// wedged or misconfigured program costs one goroutine for one timeout and
// never a stuck session.
type CommandSink struct {
	cmdline string
	timeout time.Duration
}

// NewCommandSink builds a sink running cmdline through the shell. A cmdline of
// "" yields a sink that does nothing, and a timeout of zero or less uses
// defaultCommandTimeout.
func NewCommandSink(cmdline string, timeout time.Duration) *CommandSink {
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	return &CommandSink{cmdline: cmdline, timeout: timeout}
}

// Enabled reports whether a command was configured at all.
func (s *CommandSink) Enabled() bool { return s != nil && s.cmdline != "" }

// Emit runs the command with the event as JSON on stdin. A non-zero exit is an
// error, which the dispatcher logs and drops.
func (s *CommandSink) Emit(ev Event) error {
	if !s.Enabled() {
		return nil
	}
	payload, err := json.Marshal(ev.Sanitized())
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	payload = append(payload, '\n')

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", s.cmdline)
	cmd.Stdin = bytes.NewReader(payload)
	out := &boundedBuffer{limit: commandOutputLimit}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("command timed out after %s", s.timeout)
		}
		return fmt.Errorf("command failed: %w: %s", err, out.String())
	}
	return nil
}

// boundedBuffer keeps the first limit bytes and discards the rest, so a chatty
// notification program cannot grow the heap.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }
