// Package dap is a minimal Debug Adapter Protocol client (M15 #66): DAP over
// stdio with Content-Length framing, hand-rolled on bufio + encoding/json.
// xdev carries no DAP dependency, and an adapter is a plain subprocess.
//
// Scope is what the `debug` tool needs: initialize + launch/attach, breakpoint
// sets, stepping, threads/stackTrace/scopes/variables, evaluate, terminate and
// a raw custom-request escape hatch. Nothing beyond that is claimed to the
// adapter (no RunInTerminal, no variable paging).
package dap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxFrame caps one decoded frame: a corrupt Content-Length must not make
// xdev allocate it.
const maxFrame = 16 << 20

// DAP frame types.
const (
	typeRequest  = "request"
	typeResponse = "response"
	typeEvent    = "event"
)

// message is one DAP message in either direction. DAP keeps request, response
// and event fields flat in a single object; which ones are set follows from
// Type.
type message struct {
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	Command    string          `json:"command,omitempty"`   // request
	Arguments  json.RawMessage `json:"arguments,omitempty"` // request
	RequestSeq int             `json:"request_seq,omitempty"`
	Success    bool            `json:"success,omitempty"`
	Message    string          `json:"message,omitempty"`
	Event      string          `json:"event,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

// Event is one adapter-to-client event.
type Event struct {
	Name string
	Body json.RawMessage
}

// writeFrame writes one Content-Length framed message.
func writeFrame(w io.Writer, msg *message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// readFrame reads one Content-Length framed message. Header names are matched
// case-insensitively and unknown headers (DAP adds some) are skipped.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", v)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("frame missing Content-Length")
	}
	if length > maxFrame {
		return nil, fmt.Errorf("frame of %d bytes exceeds the %d byte cap", length, maxFrame)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// decodeList pulls the named array out of a response body.
func decodeList[T any](body json.RawMessage, key string) ([]T, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	raw, ok := fields[key]
	if !ok {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Thread is one entry of a threads response.
type Thread struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// SourceRef is the file a stack frame or breakpoint came from.
type SourceRef struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// StackFrame is one entry of a stackTrace response.
type StackFrame struct {
	ID     int       `json:"id"`
	Name   string    `json:"name"`
	Source SourceRef `json:"source"`
	Line   int       `json:"line"`
	Column int       `json:"column"`
}

// Scope is one entry of a scopes response.
type Scope struct {
	Name               string `json:"name"`
	VariablesReference int    `json:"variablesReference"`
	Expensive          bool   `json:"expensive"`
}

// Variable is one entry of a variables response.
type Variable struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	Type               string `json:"type"`
	VariablesReference int    `json:"variablesReference"`
}

// Breakpoint is one entry of a setBreakpoints / setFunctionBreakpoints
// response.
type Breakpoint struct {
	ID       int    `json:"id"`
	Verified bool   `json:"verified"`
	Line     int    `json:"line"`
	Message  string `json:"message"`
}

// EvaluateResult is an evaluate response body.
type EvaluateResult struct {
	Result             string `json:"result"`
	Type               string `json:"type"`
	VariablesReference int    `json:"variablesReference"`
}

// StoppedEvent is the body of a stopped event.
type StoppedEvent struct {
	Reason            string `json:"reason"`
	Description       string `json:"description"`
	Text              string `json:"text"`
	ThreadID          int    `json:"threadId"`
	AllThreadsStopped bool   `json:"allThreadsStopped"`
	HitBreakpointIDs  []int  `json:"hitBreakpointIds"`
}

// OutputEvent is the body of an output event.
type OutputEvent struct {
	Category string `json:"category"`
	Output   string `json:"output"`
}

// TerminatedEvent is the body of a terminated/exited event.
type TerminatedEvent struct {
	ExitCode *int `json:"exitCode"`
}
