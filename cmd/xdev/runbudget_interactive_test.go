package main

import (
	"testing"
	"time"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
)

func TestInteractiveRunsAreUnboundedUnlessConfigured(t *testing.T) {
	cases := []struct {
		name        string
		runBudget   *int
		interactive bool
		want        time.Duration
	}{
		{"unset interactive", nil, true, -1},
		{"unset non-interactive", nil, false, 0},
		{"180 interactive", new(180), true, 180 * time.Second},
		{"180 non-interactive", new(180), false, 180 * time.Second},
		{"-1 interactive", new(-1), true, -1 * time.Second},
		{"-1 non-interactive", new(-1), false, -1 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &config.Settings{}
			if tc.runBudget != nil {
				s.RunBudget = tc.runBudget
			}
			ag := &agent.Agent{Model: "m"}
			wireAgentMode(ag, &config.Config{}, s, "p", "m", t.TempDir(), tc.interactive)
			if ag.RunBudget != tc.want {
				t.Fatalf("RunBudget = %v, want %v", ag.RunBudget, tc.want)
			}
		})
	}
}
