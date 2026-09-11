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
	if b == nil || len(b.Commands) != 2 {
		t.Fatalf("bus = %+v", b)
	}
	if got := b.Commands["lifecycle"]; len(got) != 2 {
		t.Fatalf("list commands = %v", got)
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

func TestRunFailsClosedOnBadCommand(t *testing.T) {
	b := FromSettings(map[string]any{"tool_call": "exit 3"})
	if _, err := b.Run(context.Background(), "tool_call", map[string]any{}); err == nil {
		t.Fatal("non-zero exit must fail closed")
	}
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
