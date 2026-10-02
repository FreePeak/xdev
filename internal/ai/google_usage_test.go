package ai

import (
	"testing"
)

// TestGoogleUsageNormalization pins the three Gemini counter traps against
// the unified Usage contract the other providers already satisfy (see
// googleUsage.toUsage): promptTokenCount includes the cached content,
// totalTokenCount is prompt+thoughts+candidates, and reasoning is billed
// output. Without this the HUD's ↑ counts a whole cached prefix as fresh
// input, ↓ omits the thinking, and /usage never shows a cache line.
func TestGoogleUsageNormalization(t *testing.T) {
	cases := []struct {
		name       string
		wire       googleUsage
		wantInput  int64
		wantCache  int64
		wantOutput int64
		wantTotal  int64
		wantThink  int64
	}{
		{
			name:       "cached prompt is netted out of input",
			wire:       googleUsage{PromptTokenCount: 30000, CachedContentTokenCount: 28000, CandidatesTokenCount: 120},
			wantInput:  2000,
			wantCache:  28000,
			wantOutput: 120,
			wantTotal:  30120,
		},
		{
			name:       "reasoning counts as output",
			wire:       googleUsage{PromptTokenCount: 11, CandidatesTokenCount: 20, ThoughtsTokenCount: 4, TotalTokenCount: 35},
			wantInput:  11,
			wantOutput: 24,
			wantTotal:  35,
			wantThink:  4,
		},
		{
			name:       "a cache larger than the prompt cannot go negative",
			wire:       googleUsage{PromptTokenCount: 100, CachedContentTokenCount: 150, CandidatesTokenCount: 5},
			wantInput:  0,
			wantCache:  150,
			wantOutput: 5,
			wantTotal:  155,
		},
		{
			name:       "an unreported total is rebuilt from the parts",
			wire:       googleUsage{PromptTokenCount: 40, CachedContentTokenCount: 10, CandidatesTokenCount: 7},
			wantInput:  30,
			wantCache:  10,
			wantOutput: 7,
			wantTotal:  47,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.wire.toUsage()
			if u.Input != tc.wantInput || u.CacheRead != tc.wantCache ||
				u.Output != tc.wantOutput || u.TotalTokens != tc.wantTotal ||
				u.ReasoningTokens != tc.wantThink {
				t.Fatalf("usage = %+v", u)
			}
			// The identity every other provider maintains: the total is the
			// sum, so a caller may reconstruct any half from the others.
			if u.TotalTokens != u.Input+u.Output+u.CacheRead {
				t.Fatalf("total %d != input %d + output %d + cache %d", u.TotalTokens, u.Input, u.Output, u.CacheRead)
			}
		})
	}
	if (*googleUsage)(nil).toUsage() != nil {
		t.Fatal("a missing usage block must stay nil, not become a zero Usage")
	}
}
