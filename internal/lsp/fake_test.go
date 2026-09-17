package lsp

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// fakeLSP is an in-process language server for the client/manager/tool
// tests: a goroutine reads Content-Length framed requests from the client's
// stdin pipe and answers each with whatever respond returns; the params of
// every message the client sent are recorded (notifications included).
type fakeLSP struct {
	t       *testing.T
	client  *Client
	respond func(method string, params json.RawMessage) (json.RawMessage, error)

	writeMu  sync.Mutex
	toClient *os.File

	mu     sync.Mutex
	notifs map[string]int
	last   map[string]json.RawMessage
}

func newFakeLSP(t *testing.T, respond func(method string, params json.RawMessage) (json.RawMessage, error)) *fakeLSP {
	t.Helper()
	toServerR, toServerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	toClientR, toClientW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeLSP{
		t:        t,
		client:   newClient(toServerW, toClientR, "fake"),
		respond:  respond,
		toClient: toClientW,
		notifs:   map[string]int{},
		last:     map[string]json.RawMessage{},
	}
	f.client.start()
	go f.serve(bufio.NewReader(toServerR))
	t.Cleanup(func() {
		_ = f.client.Close()
		_ = toClientW.Close()
	})
	return f
}

func (f *fakeLSP) serve(r *bufio.Reader) {
	for {
		body, err := readFrame(r)
		if err != nil {
			return
		}
		var msg rpcMsg
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		f.mu.Lock()
		f.last[msg.Method] = msg.Params
		if msg.ID == nil {
			f.notifs[msg.Method]++
		}
		respond := f.respond
		f.mu.Unlock()
		if msg.ID == nil {
			continue
		}
		reply := &rpcMsg{JSONRPC: "2.0", ID: msg.ID}
		if respond != nil {
			res, rerr := respond(msg.Method, msg.Params)
			if rerr != nil {
				reply.Error = &rpcError{Code: -32000, Message: rerr.Error()}
			} else {
				reply.Result = res
			}
		}
		f.writeMu.Lock()
		_ = writeFrame(f.toClient, reply)
		f.writeMu.Unlock()
	}
}

// setRespond swaps the handler; the serve goroutine reads it under the same
// lock, so a mid-test swap is race-free.
func (f *fakeLSP) setRespond(respond func(method string, params json.RawMessage) (json.RawMessage, error)) {
	f.mu.Lock()
	f.respond = respond
	f.mu.Unlock()
}

// notify sends a server->client notification (e.g. publishDiagnostics).
func (f *fakeLSP) notify(method string, params any) {
	raw, err := json.Marshal(params)
	if err != nil {
		f.t.Fatalf("marshal %s: %v", method, err)
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := writeFrame(f.toClient, &rpcMsg{JSONRPC: "2.0", Method: method, Params: raw}); err != nil {
		f.t.Fatalf("notify %s: %v", method, err)
	}
}

func (f *fakeLSP) publish(uri string, ds []Diagnostic) {
	f.notify("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": ds})
}

func (f *fakeLSP) notificationCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifs[method]
}

func (f *fakeLSP) lastParams(method string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[method]
}

// waitFor polls cond for up to 3s — servers answer on their own goroutine, so
// assertions on cross-goroutine effects must not be instantaneous.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// echoResponder answers initialize/shutdown generically and delegates every
// other method to handler (nil handler = "method not supported").
func echoResponder(handler func(method string, params json.RawMessage) (json.RawMessage, error)) func(string, json.RawMessage) (json.RawMessage, error) {
	return func(method string, params json.RawMessage) (json.RawMessage, error) {
		switch method {
		case "initialize":
			return json.RawMessage(`{"capabilities":{}}`), nil
		case "shutdown":
			return json.RawMessage(`null`), nil
		}
		if handler == nil {
			return nil, errString("method not supported: " + method)
		}
		return handler(method, params)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
