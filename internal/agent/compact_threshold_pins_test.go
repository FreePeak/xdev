package agent

import "testing"

// The two trigger pins are omp's (compaction.thresholdTokens and
// compaction.thresholdPercent): they outrank the head-room threshold, and
// each falls back to it when it cannot mean anything.
func TestTheCompactionThresholdPinsOutrankTheHeadRoom(t *testing.T) {
	tests := []struct {
		name         string
		cfg          CompactionConfig
		wantThreshol int64
	}{
		{
			name:         "an absolute pin decides",
			cfg:          CompactionConfig{ContextWindow: 128000, ThresholdTokens: 50000},
			wantThreshol: 50000,
		},
		{
			name:         "a pin at the window falls back to the head-room",
			cfg:          CompactionConfig{ContextWindow: 128000, ThresholdTokens: 128000},
			wantThreshol: 108800,
		},
		{
			name:         "a percentage pin decides",
			cfg:          CompactionConfig{ContextWindow: 128000, ThresholdPercent: 50},
			wantThreshol: 64000,
		},
		{
			name:         "a percentage is clamped to 99",
			cfg:          CompactionConfig{ContextWindow: 100000, ThresholdPercent: 150},
			wantThreshol: 99000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.threshold(); got != tc.wantThreshol {
				t.Errorf("threshold = %d, want %d", got, tc.wantThreshol)
			}
		})
	}
}
