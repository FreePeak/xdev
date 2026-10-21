package memory

import (
	"encoding/json"
	"testing"

	"github.com/FreePeak/xdev/internal/tool"
)

// The memory tools' caps are load-bearing for the keep-going gate.
//
// Live 2026-10-21: the model recalls on almost every run — it is how it
// finds out what it already knows — and `recall` was UNDECLARED as far as
// the gate was concerned, so the call counted as a MUTATION and the gate
// armed on a turn that had only looked something up. A run that asks the
// bank a question and then answers it got pushed past its own answer.
//
// The declaration lives on the tool (Capser), not in the name table: the
// name table cannot know what a tool in another package says about itself,
// and `callMutated` used to read the table alone, so every such tool read as
// a mutation.
func TestTheMemoryToolsDeclareWhatTheyDo(t *testing.T) {
	recall := &RecallTool{}
	reflectTool := &ReflectTool{}
	retain := &RetainTool{}

	if c := recall.Caps(); !c.ReadOnly {
		t.Error("recall reads the bank and writes nothing — it must be read-only")
	}
	if c := reflectTool.Caps(); !c.ReadOnly {
		t.Error("reflect answers from the bank and writes nothing — it must be read-only")
	}
	if c := retain.Caps(); c.ReadOnly {
		t.Error("retain STORES — it must not be read-only")
	}

	// And the verdict the gate actually reads.
	reg := tool.NewRegistry()
	reg.Register(recall)
	reg.Register(reflectTool)
	reg.Register(retain)

	for _, tc := range []struct {
		tool string
		want bool
		why  string
	}{
		{"recall", true, "a lookup is a read"},
		{"reflect", true, "a question is a read"},
		{"retain", false, "storing is a write"},
	} {
		if got := tool.CallIsReadOnly(reg, tc.tool, json.RawMessage(`{}`)); got != tc.want {
			t.Errorf("CallIsReadOnly(%s) = %v, want %v — %s", tc.tool, got, tc.want, tc.why)
		}
	}
}
