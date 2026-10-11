package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/FreePeak/xdev/internal/protocol"
	"github.com/FreePeak/xdev/internal/serve"
)

// runHeadless serves one session over HTTP + SSE, so a client in ANOTHER
// process (a TUI, a script, a widget) drives turns against a session that
// outlives it. The session core is the same one `xdev rpc` builds — this file
// is only the transport.
//
// Discovery is by file, never by a guessed port: the server publishes
// <data dir>/serve/headless.json (0600) with its url, pid, version and token,
// and a client reads it. That is OpenCode's registration contract, and it is
// what makes `xdev serve headless` usable from a shell one-liner.
func runHeadless(args []string, version string) int {
	opts, code, err := parseHeadlessFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev serve headless:", err)
		return code
	}

	h, cleanup, err := newSessionCore(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev: "+err.Error())
		return 2
	}
	defer cleanup()

	srv, err := serve.NewHeadless(serve.HeadlessOptions{
		Options: serve.Options{Listen: headlessListen, Token: headlessToken, Version: version},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev serve headless:", err)
		return 1
	}
	srv.SetHandler(&headlessDriver{h: h, srv: srv})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "xdev serve headless:", err)
		return 1
	}
	return 0
}

// headlessListen / headlessToken are the parsed flag values, package-scoped the
// way the other launch flags are (see launchflags.go).
var (
	headlessListen string
	headlessToken  string
)

// headlessUsage is printed by the serve dispatcher's help.
const headlessUsage = `  xdev serve headless [--listen 127.0.0.1:7717] [--token T]

An HTTP + SSE face on one live agent session, so a client in another process
drives it: /info (open) reports the identity, POST /prompt runs a turn,
GET /event streams the events, POST /steer and POST /abort reach the running
turn, GET /state reports the session. The server publishes its url, pid,
version and token to <data dir>/serve/headless.json (0600) for discovery.
`

// parseHeadlessFlags parses the subcommand's flags into printOptions.
func parseHeadlessFlags(args []string) (printOptions, int, error) {
	fs := flag.NewFlagSet("xdev serve headless", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&headlessListen, "listen", "", "bind address (default 127.0.0.1:7717; port 0 picks a free one)")
	fs.StringVar(&headlessToken, "token", "", "bearer token (default: per-install token file)")
	model := fs.String("model", "", "model ref (default: the resolved default)")
	cont := fs.Bool("continue", false, "continue the most recent session in this directory")
	resume := fs.String("resume", "", "resume a session by id prefix")
	maxTurns := fs.Int("max-turns", 0, "cap turns per run (0 = unbounded)")
	maxTokens := fs.Int("max-tokens", 0, "cap assistant output tokens (0 = provider default)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: xdev serve headless [flags]

An HTTP + SSE face on one live agent session, so a client in another process
drives it:

  GET  /info     identity (open — the probe a client runs before it has a token)
  POST /prompt   {"id":"p1","text":"..."} — returns at once; the turn streams
  GET  /event    SSE: server.connected first, then every event and the response
  POST /steer    {"text":"..."} — queues into the running turn
  POST /abort    cancels the running turn
  GET  /state    the session snapshot

Discovery is by file: the server publishes its url, pid, version and token to
<data dir>/serve/headless.json (0600) on start and removes it on shutdown.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return printOptions{}, 2, err
	}
	opts := printOptions{
		Model:        *model,
		ContinueLast: *cont,
		ResumePrefix: *resume,
		MaxTurns:     *maxTurns,
		MaxTokens:    *maxTokens,
	}
	return opts, 0, nil
}

// headlessDriver adapts the session core to the serve package's handler
// contract, translating agent events into the SSE frames a client reads.
type headlessDriver struct {
	h   *rpcHandler
	srv *serve.Headless
}

// Prompt forwards one prompt. The core's handler already runs the turn on its
// own goroutine and answers through the sink, so this must not block.
func (d *headlessDriver) Prompt(id, text string) {
	d.h.mu.Lock()
	d.h.srv = d.sink()
	d.h.mu.Unlock()
	d.h.Prompt(id, text)
}

// sink is the headless transport's commandSink: responses and events become
// SSE frames.
func (d *headlessDriver) sink() commandSink { return headlessSink{d.srv} }

func (d *headlessDriver) Steer(text string) error { return d.h.Steer(text) }
func (d *headlessDriver) Abort() error            { return d.h.Abort() }

// State is the session snapshot /state answers with. The map shape keeps the
// serve package free of the protocol type (it must not import the agent).
func (d *headlessDriver) State() map[string]any {
	st := d.h.State()
	return map[string]any{
		"sessionId": st.SessionID,
		"title":     st.Title,
		"model":     st.Model,
		"cwd":       st.CWD,
		"running":   st.Running,
	}
}

// headlessSink carries responses and events onto the SSE stream.
type headlessSink struct{ srv *serve.Headless }

func (s headlessSink) Respond(id string, resp protocol.Response) {
	s.srv.Respond(id, resp)
}

func (s headlessSink) SendEvent(id string, ev protocol.Event) {
	s.srv.Broadcast("event", map[string]any{"id": id, "event": ev})
}
