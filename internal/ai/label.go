package ai

import (
	"encoding/json"
	"strconv"
	"strings"
)

// entryArgKeys name the argument that says what a call is ABOUT, in omp's
// precedence (command, path, input) extended with xdev's search and fetch
// tools. The first one the call carries wins; one that names nothing stays bare.
var entryArgKeys = []string{"command", "path", "file_path", "pattern", "query", "url", "input"}

// MessageLabel is the one-line preview of a message: its text when it has any,
// and otherwise whatever the transcript itself would label the row with. Text()
// alone answers "what did this say", and across most of a real agent run the
// answer is nothing — an assistant turn that only calls tools, or whose text
// came back empty, holds no text block at all (45% of the messages in a
// 714-message session do), and a reasoning-only turn holds none either. A
// caller that LISTS entries (the /tree selector) then paints a row with nothing
// after its id, which is what a user reads as a blank line in the history they
// just opened.
//
// "" remains only for a message that is genuinely empty.
func MessageLabel(m *Message) string {
	if s := flat(m.Text()); s != "" {
		return s
	}
	if calls := m.ToolCalls(); len(calls) > 0 {
		label := calls[0].Name
		if arg := toolNamingArg(calls[0].Arguments); arg != "" {
			label += " · " + arg
		}
		if len(calls) > 1 {
			label += " (+" + strconv.Itoa(len(calls)-1) + ")"
		}
		return label
	}
	if m.Role == RoleToolResult && m.ToolName != "" {
		if m.IsError {
			return "↩ " + m.ToolName + " (error)"
		}
		return "↩ " + m.ToolName
	}
	for _, b := range m.Content {
		switch t := b.(type) {
		case ThinkingBlock:
			if s := flat(t.Thinking); s != "" {
				return "… " + s
			}
		case ImageBlock:
			return "(image)"
		}
	}
	switch m.StopReason {
	case StopReasonError:
		return "(error turn)"
	case StopReasonAborted:
		return "(aborted)"
	}
	return ""
}

// toolNamingArg is the one argument that says what a call is about, worded the
// way the transcript's own tool row words it (tui.toolDetail): the first
// entryArgKeys field the call carries, first line only, whitespace collapsed,
// " …" when the argument runs past that line. "" when the call names nothing, or
// when its arguments are not a JSON object at all — then the bare tool name says
// as much as anything would.
func toolNamingArg(args json.RawMessage) string {
	var fields map[string]json.RawMessage
	if len(args) == 0 || json.Unmarshal(args, &fields) != nil {
		return ""
	}
	for _, key := range entryArgKeys {
		var s string
		if v, ok := fields[key]; !ok || json.Unmarshal(v, &s) != nil {
			continue
		}
		head, rest, multiline := strings.Cut(strings.TrimRight(s, "\n"), "\n")
		if len(head) > 400 {
			// A write call's first line can be enormous; the row shows a
			// phrase, so stop working past what any terminal can display.
			head, multiline = head[:400], true
		}
		if head = strings.Join(strings.Fields(head), " "); head == "" {
			continue
		}
		if multiline && strings.TrimSpace(rest) != "" {
			head += " …"
		}
		return head
	}
	return ""
}

// flat collapses a block's whitespace to single spaces so it fits one row; the
// caller clips the length.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }
