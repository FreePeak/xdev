package dap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// responder answers one DAP request the way an adapter would.
type responder func(command string, args json.RawMessage) (json.RawMessage, error)

// fakeAdapter is an in-process debug adapter: a goroutine reads DAP frames
// from the client's stdin pipe and answers whatever respond returns, while
// events can be pushed at any time (emit).
type fakeAdapter struct {
	t        *testing.T
	client   *Client
	toClient *os.File

	writeMu sync.Mutex

	mu       sync.Mutex
	respond  responder
	received []string
	spawned  []string
	last     map[string]json.RawMessage
	seq      int
}

func newFakeAdapter(t *testing.T, respond responder) *fakeAdapter {
	t.Helper()
	toServerR, toServerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	toClientR, toClientW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAdapter{
		t:        t,
		client:   newClient(toServerW, toClientR, "fake"),
		respond:  respond,
		toClient: toClientW,
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

func (f *fakeAdapter) serve(r *bufio.Reader) {
	for {
		body, err := readFrame(r)
		if err != nil {
			return
		}
		var msg message
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		if msg.Type != typeRequest {
			continue
		}
		f.mu.Lock()
		f.received = append(f.received, msg.Command)
		f.last[msg.Command] = msg.Arguments
		respond := f.respond
		f.mu.Unlock()

		var out json.RawMessage
		failure := ""
		if respond != nil {
			res, err := respond(msg.Command, msg.Arguments)
			out, failure = res, errString(err)
		} else {
			failure = "unsupported request " + msg.Command
		}
		f.write(&message{
			Seq:        f.nextSeq(),
			Type:       typeResponse,
			RequestSeq: msg.Seq,
			Command:    msg.Command,
			Success:    failure == "",
			Message:    failure,
			Body:       out,
		})
	}
}

// exit makes the adapter side of the pipe vanish, the way a crashed adapter
// process does.
func (f *fakeAdapter) exit() { _ = f.toClient.Close() }

// spawn makes the fake usable as a Tool's adapter launcher: the tool still
// runs its real claim and handshake path, just without a subprocess.
func (f *fakeAdapter) spawn(name string, _ AdapterSpec, _ string) (*Client, error) {
	f.mu.Lock()
	f.spawned = append(f.spawned, name)
	f.mu.Unlock()
	return f.client, nil
}

// spawnCount counts the launcher calls made through spawn.
func (f *fakeAdapter) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawned)
}

func (f *fakeAdapter) nextSeq() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.seq
}

func (f *fakeAdapter) write(msg *message) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = writeFrame(f.toClient, msg)
}

// emit pushes one adapter->client event.
func (f *fakeAdapter) emit(event string, body any) {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal %s event: %v", event, err)
		}
		raw = b
	}
	f.write(&message{Seq: f.nextSeq(), Type: typeEvent, Event: event, Body: raw})
}

func (f *fakeAdapter) setRespond(respond responder) {
	f.mu.Lock()
	f.respond = respond
	f.mu.Unlock()
}

func (f *fakeAdapter) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func (f *fakeAdapter) lastArgs(command string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[command]
}

// waitFor polls cond for up to 3s: adapters answer on their own goroutine, so
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

// equalStrings compares two string slices (test assertion helper).
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// errString renders a responder error, "": nil error.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// defaultRespond answers the debug tool's op set the way dlv does: initialize
// emits the `initialized` event, launch stops on entry, breakpoints verify,
// one thread, two frames, two scopes, two variables, and continue stops at the
// breakpoint. Tests override single commands with withOverride.
func defaultRespond(f *fakeAdapter) responder {
	return func(command string, args json.RawMessage) (json.RawMessage, error) {
		switch command {
		case "initialize":
			f.emit("initialized", map[string]any{})
			return json.RawMessage(`{"supportsConfigurationDoneRequest":true}`), nil
		case "launch", "attach":
			f.emit("stopped", map[string]any{"reason": "entry", "threadId": 1})
			return json.RawMessage(`{}`), nil
		case "setBreakpoints":
			return json.RawMessage(`{"breakpoints":[{"id":1,"verified":true,"line":12}]}`), nil
		case "setFunctionBreakpoints":
			return json.RawMessage(`{"breakpoints":[{"id":9,"verified":true,"message":"resolved"}]}`), nil
		case "threads":
			return json.RawMessage(`{"threads":[{"id":1,"name":"main"}]}`), nil
		case "continue":
			f.emit("stopped", map[string]any{"reason": "breakpoint", "threadId": 1, "hitBreakpointIds": []int{1}})
			return json.RawMessage(`{"allThreadsContinued":true}`), nil
		case "next", "stepIn", "stepOut", "pause":
			f.emit("stopped", map[string]any{"reason": "step", "threadId": 1})
			return json.RawMessage(`{}`), nil
		case "stackTrace":
			return json.RawMessage(`{"totalFrames":2,"stackFrames":[{"id":7,"name":"main.main","source":{"name":"main.go","path":"/proj/main.go"},"line":12,"column":2},{"id":8,"name":"main.run","source":{"name":"main.go","path":"/proj/main.go"},"line":20,"column":1}]}`), nil
		case "scopes":
			return json.RawMessage(`{"scopes":[{"name":"Locals","variablesReference":2},{"name":"Globals","variablesReference":3,"expensive":true}]}`), nil
		case "variables":
			return json.RawMessage(`{"variables":[{"name":"x","value":"42","type":"int"},{"name":"s","value":"hello","type":"string"}]}`), nil
		case "evaluate":
			return json.RawMessage(`{"result":"42","type":"int","variablesReference":0}`), nil
		case "configurationDone", "terminate", "disconnect":
			return json.RawMessage(`{}`), nil
		}
		return nil, fmt.Errorf("unsupported request %s", command)
	}
}

// withOverride replaces one command's answer in base.
func withOverride(base responder, command string, over responder) responder {
	return func(cmd string, args json.RawMessage) (json.RawMessage, error) {
		if cmd == command {
			return over(cmd, args)
		}
		return base(cmd, args)
	}
}

// TestMain lets the test binary act as a debug adapter subprocess: the
// process-level tests point an adapter at os.Args[0] with
// XDEV_DAP_FAKE_ADAPTER set, which exercises the real spawn and teardown path.
func TestMain(m *testing.M) {
	if os.Getenv("XDEV_DAP_FAKE_ADAPTER") != "" {
		if err := serveHelperAdapter(); err != nil {
			fmt.Fprintln(os.Stderr, "fake adapter:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperTransport picks the fake adapter's DAP channel: stdio, or — when the
// launcher passed the socket flag — a dial-back to xdev's listener, which is
// how a socket adapter (dlv) is wired.
func helperTransport() (io.Reader, io.Writer, func(), error) {
	for _, a := range os.Args[1:] {
		addr, ok := strings.CutPrefix(a, clientAddrFlag+"=")
		if !ok {
			continue
		}
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, nil, nil, err
		}
		return conn, conn, func() { _ = conn.Close() }, nil
	}
	return os.Stdin, os.Stdout, func() {}, nil
}

// serveHelperAdapter is a minimal adapter: the initialize handshake,
// launch/attach, threads, and everything else answered with an empty success.
// It exits on disconnect (or EOF), which is what the teardown tests observe.
func serveHelperAdapter() error {
	r, w, closer, err := helperTransport()
	if err != nil {
		return err
	}
	defer closer()
	br := bufio.NewReader(r)
	seq := 0
	send := func(msg *message) bool {
		seq++
		msg.Seq, msg.Type = seq, typeResponse
		return writeFrame(w, msg) == nil
	}
	for {
		body, err := readFrame(br)
		if err != nil {
			return nil
		}
		var msg message
		if err := json.Unmarshal(body, &msg); err != nil || msg.Type != typeRequest {
			continue
		}
		reply := &message{RequestSeq: msg.Seq, Command: msg.Command, Success: true}
		switch msg.Command {
		case "initialize":
			seq++
			if writeFrame(w, &message{Seq: seq, Type: typeEvent, Event: "initialized", Body: json.RawMessage(`{}`)}) != nil {
				return nil
			}
		case "threads":
			reply.Body = json.RawMessage(`{"threads":[{"id":1,"name":"fake"}]}`)
		case "disconnect":
			send(reply)
			return nil
		}
		if !send(reply) {
			return nil
		}
	}
}
