// extfixture is a scriptable test extension (M7 #8): it speaks the ext
// JSONL protocol, with behavior selected by the EXT_MODE environment
// variable, so one binary covers handshake, block, revise, patch, hang,
// and crash cases.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type frame struct {
	Type     string          `json:"type"`
	ID       string          `json:"id,omitempty"`
	Event    string          `json:"event,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Protocol int             `json:"protocol,omitempty"`
	Allow    bool            `json:"allow,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Revise   json.RawMessage `json:"revise,omitempty"`
	Patch    json.RawMessage `json:"patch,omitempty"`
	Error    string          `json:"error,omitempty"`
	Action   string          `json:"action,omitempty"`
	Text     string          `json:"text,omitempty"`
	FailOpen bool            `json:"failOpen,omitempty"`
}

func main() {
	mode := os.Getenv("EXT_MODE")
	out := bufio.NewWriter(os.Stdout)
	emit := func(f frame) {
		b, _ := json.Marshal(f)
		out.Write(b)
		out.WriteString("\n")
		out.Flush()
	}

	// Handshake capabilities. The hang/crash modes announce the SAME
	// subscriptions and then misbehave, which is exactly what the
	// fail-closed tests need to observe.
	events := []string{"tool_call", "tool_result", "session_start"}
	if mode == "notoolcall" {
		events = []string{"session_start"}
	}
	caps := map[string]any{
		"events":   events,
		"commands": []map[string]string{{"name": "ping", "description": "pong"}},
		"renderers": []map[string]any{
			{"tool": "greet", "kind": "table", "spec": map[string]any{"columns": []string{"greeting", "who"}}},
		},
		"tools": []map[string]any{
			{"name": "greet", "description": "greet someone", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"who": map[string]string{"type": "string"}},
			}},
		},
	}
	if mode == "failopen" {
		caps["failOpen"] = true
	}
	cb, _ := json.Marshal(caps)
	emit(frame{Type: "capabilities", Payload: cb})

	switch mode {
	case "hang":
		// Consume every event and never answer: the host must time out and
		// SIGKILL. (A bare `select {}` would trip Go's deadlock detector
		// and panic the child instead of hanging it.)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
		}
		return
	case "crash":
		// Exit right after the handshake: policy dispatch must see a dead
		// pipe and fail closed.
		os.Exit(0)
	case "badframe":
		fmt.Fprintln(os.Stdout, "this is not json")
		return
	}

	errCount := 0
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var f frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		switch {
		case f.Type != "event":
			continue
		case f.Event == "tool_call" && mode == "errorreply":
			// First policy question errors (an answer, not a crash);
			// subsequent ones allow. Proves an error reply keeps the
			// extension alive and subscribed.
			if errCount == 0 {
				errCount++
				emit(frame{Type: "response", ID: f.ID, Error: "internal: cannot decide"})
			} else {
				emit(frame{Type: "response", ID: f.ID, Allow: true})
			}
		case f.Event == "tool_call" && mode == "block":
			emit(frame{Type: "response", ID: f.ID, Allow: false, Reason: "policy: no bash"})
		case f.Event == "tool_call" && mode == "revise":
			emit(frame{Type: "response", ID: f.ID, Allow: true,
				Revise: json.RawMessage(`{"command":"echo REVISED"}`)})
		case f.Event == "command":
			var in struct {
				Command string `json:"command"`
				Args    string `json:"arguments"`
			}
			_ = json.Unmarshal(f.Payload, &in)
			txt, _ := json.Marshal(map[string]any{
				"text": "pong from the extension (" + in.Command + " " + in.Args + ")",
			})
			emit(frame{Type: "response", ID: f.ID, Allow: true, Patch: txt})
		case f.Event == "tool_result" && mode != "render":
			emit(frame{Type: "response", ID: f.ID, Allow: true,
				Patch: json.RawMessage(`{"text":"patched by ext","isError":false}`)})
		case f.Event == "tool_invoke":
			emit(frame{Type: "response", ID: f.ID, Allow: true,
				Patch: json.RawMessage(`{"text":"{\"greeting\":\"hello\",\"who\":\"linh\"}","isError":false}`)})
		case f.Event == "session_start" && mode == "action":
			emit(frame{Type: "action", Action: "steer", Text: "extension steering"})
			emit(frame{Type: "response", ID: f.ID, Allow: true})
		default:
			emit(frame{Type: "response", ID: f.ID, Allow: true})
		}
	}
}
