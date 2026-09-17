package tool

import (
	"encoding/json"
	"testing"
)

func TestApprovalClassifyTiers(t *testing.T) {
	for _, tc := range []struct {
		tool string
		want Tier
	}{
		{"read", TierReadOnly},
		{"write", TierWrite},
		{"edit", TierWrite},
		{"bash", TierExec},
	} {
		got, err := Classify(tc.tool, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Classify(%q): %v", tc.tool, err)
		}
		if got != tc.want {
			t.Errorf("Classify(%q) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

func TestApprovalClassifyUnknownTool(t *testing.T) {
	if _, err := Classify("deploy_prod", nil); err == nil {
		t.Error("unknown tool must error, not silently classify")
	}
}

func TestApprovalNeedsApprovalTruthTable(t *testing.T) {
	for _, mode := range []ApprovalMode{AlwaysAsk, Write, Yolo} {
		for _, tier := range []Tier{TierReadOnly, TierWrite, TierExec} {
			want := mode == AlwaysAsk || (mode == Write && tier >= TierWrite)
			if got := NeedsApproval(mode, tier); got != want {
				t.Errorf("NeedsApproval(%v, %v) = %v, want %v", mode, tier, got, want)
			}
		}
	}
}

func TestApprovalYoloNeverPrompts(t *testing.T) {
	for _, tier := range []Tier{TierReadOnly, TierWrite, TierExec} {
		if NeedsApproval(Yolo, tier) {
			t.Fatalf("Yolo must never prompt (tier %v)", tier)
		}
	}
}

func TestApprovalDefaultModeIsYolo(t *testing.T) {
	if DefaultApprovalMode != Yolo {
		t.Fatalf("MVP default must be Yolo, got %v", DefaultApprovalMode)
	}
}
