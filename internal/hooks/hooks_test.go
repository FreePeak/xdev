package hooks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestFromSettingsShapes(t *testing.T) {
	if b := FromSettings(nil); b != nil {
		t.Fatal("nil settings must give nil bus")
	}
	if b := FromSettings(map[string]any{}); b != nil {
		t.Fatal("empty settings must give nil bus")
	}
	b := FromSettings(map[string]any{
		"tool_call": "echo blocked",
		"lifecycle": []any{"echo one", "echo two"},
	})
	if b == nil || len(b.Hooks) != 3 {
		t.Fatalf("bus = %+v", b)
	}
	if got := b.Events(); len(got) != 2 || got[0] != "lifecycle" || got[1] != "tool_call" {
		t.Fatalf("events = %v", got)
	}
}

func TestRunBlockShortCircuits(t *testing.T) {
	b := FromSettings(map[string]any{
		"tool_call": `echo '{"block":true,"reason":"no shell on fridays"}'`,
	})
	_, err := b.Run(context.Background(), "tool_call", map[string]any{"input": "x"})
	if err == nil || !strings.Contains(err.Error(), "no shell on fridays") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunInputLastWins(t *testing.T) {
	b := FromSettings(map[string]any{
		"tool_call": []any{
			`echo '{"input":"first"}'`,
			`echo '{"input":"second"}'`,
		},
	})
	res, err := b.Run(context.Background(), "tool_call", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if res["input"] != "second" {
		t.Fatalf("input = %v, want last-wins", res["input"])
	}
}

// Exit-code contract (#92): 2 blocks with the stderr reason, any other
// non-zero is a non-blocking warning. This test used to pin "exit 3 fails
// closed", which is precisely the over-blocking the issue reports.
func TestRunExitCodeContract(t *testing.T) {
	// Exit 2 blocks, and the reason is the hook's stderr.
	b := FromSettings(map[string]any{"tool_call": "echo 'rm -rf is not allowed' >&2; exit 2"})
	_, err := b.Run(context.Background(), "tool_call", map[string]any{})
	if err == nil {
		t.Fatal("exit 2 must block the call")
	}
	if !strings.Contains(err.Error(), "rm -rf is not allowed") {
		t.Fatalf("the block reason must be the stderr text, got %v", err)
	}
	// Any other non-zero exit warns and lets the call through.
	for _, code := range []string{"1", "3"} {
		b := FromSettings(map[string]any{"tool_call": "echo boom >&2; exit " + code})
		res, err := b.Run(context.Background(), "tool_call", map[string]any{})
		if err != nil {
			t.Fatalf("exit %s must not block a call (ported advisory hook): %v", code, err)
		}
		if res == nil {
			t.Fatalf("exit %s must still return the payload", code)
		}
	}
	// Malformed stdout stays fail-closed: a pre-hook whose verdict cannot be
	// read must not be treated as an approval.
	b2 := FromSettings(map[string]any{"tool_call": "echo not-json"})
	if _, err := b2.Run(context.Background(), "tool_call", map[string]any{}); err == nil {
		t.Fatal("non-JSON stdout must fail closed")
	}
}

func TestNotifyIgnoresFailures(t *testing.T) {
	b := FromSettings(map[string]any{"tool_result": "exit 9"})
	b.Notify(context.Background(), "tool_result", map[string]any{"text": "x"}) // must not panic/error out
	b.Notify(context.Background(), "tool_result", nil)
}

func TestNilBusIsSafe(t *testing.T) {
	var b *Bus
	res, err := b.Run(context.Background(), "tool_call", map[string]any{"input": 1})
	if err != nil || res["input"] != 1 {
		t.Fatalf("nil bus Run = %v, %v", res, err)
	}
	b.Notify(context.Background(), "tool_result", nil) // no panic
	if b.Events() != nil {
		t.Fatal("nil bus events must be nil")
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	// The hook receives the payload as JSON on stdin and its stdout JSON
	// patch flows back — pin the wire shape.
	b := FromSettings(map[string]any{
		"tool_result": `python3 -c "import json,sys; d=json.load(sys.stdin); print(json.dumps({'text': d['args']['x'] + '!' }))"`,
	})
	res, err := b.Run(context.Background(), "tool_result", map[string]any{
		"args": map[string]any{"x": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res["text"] != "hello!" {
		t.Fatalf("text = %v", res["text"])
	}
	_ = json.Marshal // keep the import if assertions change
}
