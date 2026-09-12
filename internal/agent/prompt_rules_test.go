package agent

import (
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/rules"
)

func TestBuildRulesBlockEmpty(t *testing.T) {
	if got := BuildRulesBlock(nil); got != "" {
		t.Fatalf("no rules must yield no block, got %q", got)
	}
}

func TestBuildRulesBlockRendersAlwaysApplyAndConditional(t *testing.T) {
	got := BuildRulesBlock([]rules.Rule{
		{Name: "go-style", Content: "use tabs\n", AlwaysApply: true, Priority: rules.PriorityNative, Source: "native"},
		{Name: "ts-only", Content: "strict mode\n", Globs: []string{"*.ts"}, Priority: rules.PriorityCursor, Source: "cursor", Description: "TS rules"},
	})
	if !strings.Contains(got, "# Rules") || !strings.Contains(got, "## go-style") || !strings.Contains(got, "use tabs") {
		t.Fatalf("always-apply rule not rendered in full:\n%s", got)
	}
	// Conditional rules list the edit/write shorthand, not the body.
	if strings.Contains(got, "strict mode") {
		t.Fatalf("conditional rule body must stay out of the static prompt:\n%s", got)
	}
	if !strings.Contains(got, "ts-only — tool:edit()/tool:write() when path matches `*.ts`") ||
		!strings.Contains(got, "rule://ts-only") {
		t.Fatalf("conditional shorthand listing missing:\n%s", got)
	}
}

func TestBuildRulesBlockPerRuleCap(t *testing.T) {
	big := rules.Rule{Name: "big", AlwaysApply: true, Priority: rules.PriorityNative, Content: strings.Repeat("x", MaxRuleBytes+100)}
	got := BuildRulesBlock([]rules.Rule{big})
	if len(got) > MaxRuleBytes+200 {
		t.Fatalf("per-rule cap not applied: %d bytes", len(got))
	}
	if !strings.Contains(got, "… [truncated]") {
		t.Fatal("truncation marker missing")
	}
}

func TestBuildRulesBlockDropsLowestPriorityOnOverflow(t *testing.T) {
	// Four per-rule-capped (~8KB) rules exceed the 32KB total. Dropping
	// stops once under the cap and always removes from the tail of the
	// priority-desc order (lowest priority first).
	var rs []rules.Rule
	for _, p := range []struct {
		name     string
		priority int
	}{
		{"low1", rules.PriorityBuiltin},
		{"low2", rules.PriorityBuiltin},
		{"high1", rules.PriorityNative},
		{"high2", rules.PriorityNative},
	} {
		rs = append(rs, rules.Rule{Name: p.name, AlwaysApply: true, Priority: p.priority, Content: strings.Repeat("y", MaxRuleBytes)})
	}
	got := BuildRulesBlock(rs)
	if len(got) > MaxRulesBytes {
		t.Fatalf("total cap not applied: %d bytes", len(got))
	}
	if !strings.Contains(got, "## high1") || !strings.Contains(got, "## high2") {
		t.Fatal("highest-priority rules must survive the cap")
	}
	if strings.Contains(got, "## low2") {
		t.Fatal("the lowest-priority tail must be dropped first on overflow")
	}
}
