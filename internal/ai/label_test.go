package ai

import "testing"

// TestMessageLabel covers the rows Text() leaves empty. A real agent run is
// mostly tool-call-only assistant turns and tool results, and neither carries a
// text block the way Text() needs (45% of the messages in a 714-message
// session), so a LIST built on Text() alone paints nearly half its rows blank.
// MessageLabel must name every one of them.
func TestMessageLabel(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{"text wins, and reads as it always did", Message{Role: RoleUser, Content: []Block{
			TextBlock{Text: "the answer\nwith more lines"},
		}}, "the answer with more lines"}, // every whitespace collapsed, exactly as the old clipSummary(Text()) did
		{"tool call named by its command", Message{Role: RoleAssistant, Content: []Block{
			ToolCallBlock{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"go test ./...\n-extras"}`)},
		}}, "bash · go test ./... …"}, // " …" = the argument runs past the line, as the transcript marks it
		{"single-line argument, no marker", Message{Role: RoleAssistant, Content: []Block{
			ToolCallBlock{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"go test ./..."}`)},
		}}, "bash · go test ./..."},
		{"arg precedence: command beats path", Message{Role: RoleAssistant, Content: []Block{
			ToolCallBlock{ID: "c1", Name: "task", Arguments: []byte(`{"path":"x","command":"y"}`)},
		}}, "task · y"},
		{"extra calls counted", Message{Role: RoleAssistant, Content: []Block{
			ToolCallBlock{ID: "c1", Name: "read", Arguments: []byte(`{"path":"a.go"}`)},
			ToolCallBlock{ID: "c2", Name: "read", Arguments: []byte(`{"path":"b.go"}`)},
		}}, "read · a.go (+1)"},
		{"a call that names nothing stays bare", Message{Role: RoleAssistant, Content: []Block{
			ToolCallBlock{ID: "c1", Name: "ask", Arguments: []byte(`{oops`)},
		}}, "ask"},
		{"tool result names its tool", Message{Role: RoleToolResult, ToolName: "edit"}, "↩ edit"},
		{"error result marked", Message{Role: RoleToolResult, ToolName: "edit", IsError: true},
			"↩ edit (error)"},
		{"reasoning read alone", Message{Role: RoleAssistant, Content: []Block{
			ThinkingBlock{Thinking: "hmm,\n  so this"},
		}}, "… hmm, so this"},
		{"image", Message{Role: RoleUser, Content: []Block{ImageBlock{}}}, "(image)"},
		{"aborted turn", Message{Role: RoleAssistant, StopReason: StopReasonAborted}, "(aborted)"},
		{"error turn", Message{Role: RoleAssistant, StopReason: StopReasonError}, "(error turn)"},
		{"truly empty", Message{Role: RoleAssistant}, ""},
	}
	for _, c := range cases {
		m := c.msg
		if got := MessageLabel(&m); got != c.want {
			t.Errorf("%s: MessageLabel = %q, want %q", c.name, got, c.want)
		}
	}
}
