package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
)

// The settings → agent seam for retry.infinite / -retry-forever. No build
// site sets ag.Retry at all, so wireAgentMode is the single place it can
// ride — and the single place it can be silently dropped (#79/#80 class).
func TestWireAgentModePropagatesInfiniteRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name string
		s    *config.Settings
		want bool
	}{
		{"on", &config.Settings{Retry: config.RetrySettings{Infinite: true}}, true},
		{"off by default", &config.Settings{}, false},
	} {
		ag := &agent.Agent{Model: "m"}
		wireAgentMode(ag, nil, &config.Config{}, tc.s, "p", "m", t.TempDir(), false)
		if ag.Retry.Infinite != tc.want {
			t.Errorf("%s: ag.Retry.Infinite = %v, want %v", tc.name, ag.Retry.Infinite, tc.want)
		}
	}
}

// An unbounded-wait round is the one transient worth printing verbatim: it
// is the message that reads "still waiting" instead of "hung", and a script
// parsing the stream needs the round number and the wait.
func TestAllTargetsDownPrintsVerbatim(t *testing.T) {
	down := ai.Event{Type: ai.EventError, Err: &agent.AllTargetsDownError{
		Round: 3, Delay: 8 * time.Second, LastErr: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused"),
	}}
	if ai.Classify(down.Err) != ai.ClassTransient {
		t.Fatal("the announcement must stay classified transient")
	}
	out := captureStderr(t, func() { (&printHooks{}).OnEvent(down) })
	for _, want := range []string{"all targets down", "round 3", "connection refused"} {
		if !strings.Contains(out, want) {
			t.Fatalf("announcement display = %q, want it to name %q", out, want)
		}
	}
	if strings.Contains(out, "stream error: retrying") {
		t.Fatalf("the round announcement collapsed to the per-attempt notice: %q", out)
	}
}
