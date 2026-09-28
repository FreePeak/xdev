package tui

import (
	"fmt"
	"strings"
	"time"
)

// ScheduleOps wires /schedule to the session-local reminder state. The
// callbacks are deliberately small: scheduling belongs to cmd/agent, while
// this package only parses the human-facing command and renders its result.
type ScheduleOps struct {
	List   func() string
	Create func(prompt, selector string) (string, error)
	Delete func(id string) error
}

// Dispatch handles /schedule [create|delete|list]. Create syntax is
// "/schedule create <after-seconds|every-seconds|RFC3339> <prompt>".
func (o *ScheduleOps) Dispatch(args string) (string, error) {
	if o == nil {
		return "", fmt.Errorf("schedule not wired")
	}
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 || strings.EqualFold(fields[0], "list") {
		if o.List == nil {
			return "", fmt.Errorf("schedule list not wired")
		}
		return o.List(), nil
	}
	verb := strings.ToLower(fields[0])
	switch verb {
	case "create":
		if len(fields) < 3 {
			return "", fmt.Errorf("usage: /schedule create <after-seconds|every-seconds|RFC3339> <prompt>")
		}
		if o.Create == nil {
			return "", fmt.Errorf("schedule create not wired")
		}
		return o.Create(strings.Join(fields[2:], " "), fields[1])
	case "delete":
		if len(fields) != 2 {
			return "", fmt.Errorf("usage: /schedule delete <id>")
		}
		if o.Delete == nil {
			return "", fmt.Errorf("schedule delete not wired")
		}
		return "deleted " + fields[1], o.Delete(fields[1])
	default:
		return "", fmt.Errorf("usage: /schedule [list|create <selector> <prompt>|delete <id>]")
	}
}

// ScheduleSelector parses the human-facing selector. Bare positive integers
// are seconds; an integer at least 300 is accepted as a recurring interval.
func ScheduleSelector(value string) (kind string, seconds int64, at time.Time, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0, time.Time{}, fmt.Errorf("schedule selector is required")
	}
	if n, parseErr := parsePositiveInt(value); parseErr == nil {
		if n >= 300 {
			return "every", n, time.Time{}, nil
		}
		if n > 0 {
			return "after", n, time.Time{}, nil
		}
	}
	at, err = time.Parse(time.RFC3339, value)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("selector must be positive seconds or RFC3339: %w", err)
	}
	return "at", 0, at.UTC(), nil
}

func parsePositiveInt(value string) (int64, error) {
	var n int64
	if value == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not an integer")
		}
		n = n*10 + int64(r-'0')
		if n > 1<<62 {
			return 0, fmt.Errorf("too large")
		}
	}
	return n, nil
}
