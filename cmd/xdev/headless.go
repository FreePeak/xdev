package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
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
	// The lifecycle frames only this transport carries (the RPC contract has
	// no compaction or turn-end frame yet).
	h.notify = headlessDriver{s: srv, h: h}

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
	// s is set only on the notifier instance (the same server, so the two
	// copies share one subscriber set); the handler copy carries no s.
	s *serve.Headless
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

// NotifyCompaction maps the compaction hook onto the SSE stream. A remote
// client has no local peer to read for this: the transcript it just streamed
// is being replaced by a summary plus the kept tail, and it must be able to
// say so (the TUI paints a system block for the same event).
func (d headlessDriver) NotifyCompaction(before int64) {
	if d.s == nil {
		return
	}
	d.s.Broadcast("session_compact", map[string]any{"tokensBefore": before})
}

// NotifyTurnEnd reports the terminal reason of a run, so a client can tell
// "the model finished" from "the stream dropped" — the case where a terminal
// response frame never arrives (an abort).
func (d headlessDriver) NotifyTurnEnd(reason ai.StopReason, err error) {
	if d.s == nil {
		return
	}
	payload := map[string]any{"stopReason": string(reason)}
	if err != nil {
		payload["error"] = err.Error()
	}
	d.s.Broadcast("turn_end", payload)
}

// sseFrame is one parsed SSE frame from the server's /event stream.
type sseFrame struct {
	event string
	data  string
}

// readSSE parses frames from an SSE body, sending them on out. It is the
// client-side twin of the reader in internal/serve's tests.
func readSSE(body io.Reader, out chan<- sseFrame) {
	sc := bufio.NewScanner(body)
	f := sseFrame{}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if f.event != "" {
				out <- f
				f = sseFrame{}
			}
		case strings.HasPrefix(line, "event:"):
			f.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			f.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	close(out)
}

// runHeadlessClient is the client half: discover the server by registration
// file, subscribe to its event stream, and drive one prompt. It is what makes
// the service usable from a shell one-liner, and it is the exact code a
// `--server`-style TUI attach would reuse.
//
// Flags:
//
//	--print      print the registration as JSON (or the error) and exit
//	--stream N   how long to stay attached after the turn ends (0 = immediate)
//	<text>       the prompt
func runHeadlessClient(args []string) int {
	fs := flag.NewFlagSet("xdev attach", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	reg := fs.Bool("print", false, "print the discovered registration (url, pid, version) and exit")
	quiet := fs.Bool("q", false, "print only the model's answer")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: xdev attach [flags] [prompt]

Attaches to a running `+"`xdev serve headless`"+` server, discovered by
reading <data dir>/serve/headless.json: subscribes to its event stream, sends
the prompt, and prints the answer as it arrives. With no prompt it just prints
the server's state and exits.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dataDir := config.DataDir()
	found, ok := serve.DiscoverHeadless(dataDir)
	if !ok {
		fmt.Fprintln(os.Stderr, "xdev attach: no server registered (is `xdev serve headless` running?)")
		return 1
	}
	if *reg {
		out := map[string]any{"url": found.URL, "pid": found.PID, "version": found.Version}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
		return 0
	}
	base := strings.TrimRight(found.URL, "/")
	if fs.NArg() == 0 {
		// No prompt: state only.
		req, _ := http.NewRequest(http.MethodGet, base+"/state", nil)
		req.Header.Set("Authorization", "Bearer "+found.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "xdev attach:", err)
			return 1
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		fmt.Println(strings.TrimSpace(string(body)))
		return 0
	}
	text := strings.Join(fs.Args(), " ")

	// Subscribe FIRST: a frame emitted before the stream opens is lost, which
	// is the volatility contract — the first thing the server sends is the
	// connected notice, so seeing it means we are not racing the turn.
	req, _ := http.NewRequest(http.MethodGet, base+"/event?token="+url.QueryEscape(found.Token), nil)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev attach:", err)
		return 1
	}
	defer stream.Body.Close()
	frames := make(chan sseFrame, 64)
	go readSSE(stream.Body, frames)
	connected := make(chan struct{})
	go func() {
		for f := range frames {
			if f.event == "server.connected" {
				close(connected)
				return
			}
		}
	}()
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "xdev attach: the server never said it was connected")
		return 1
	}

	// Drive the prompt.
	id := "attach"
	body, _ := json.Marshal(map[string]string{"id": id, "text": text})
	preq, _ := http.NewRequest(http.MethodPost, base+"/prompt", bytes.NewReader(body))
	preq.Header.Set("Authorization", "Bearer "+found.Token)
	pres, err := http.DefaultClient.Do(preq)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev attach:", err)
		return 1
	}
	_ = pres.Body.Close()
	if pres.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "xdev attach: POST /prompt → %s\n", pres.Status)
		return 1
	}

	// Print deltas as they arrive; stop on the terminal response for this id.
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case f, ok := <-frames:
			if !ok {
				fmt.Fprintln(os.Stderr, "xdev attach: the stream closed before the turn ended")
				return 1
			}
			switch f.event {
			case "event":
				var ev struct {
					ID    string `json:"id"`
					Event struct {
						Type   string `json:"type"`
						Delta  string `json:"delta"`
						Text   string `json:"text"`
						Reason string `json:"reason"`
					} `json:"event"`
				}
				if json.Unmarshal([]byte(f.data), &ev) != nil || ev.ID != id {
					continue
				}
				if ev.Event.Delta != "" && !*quiet {
					fmt.Print(ev.Event.Delta)
				}
			case "response":
				var resp struct {
					ID       string `json:"id"`
					Response struct {
						Ok         bool   `json:"ok"`
						Text       string `json:"text"`
						StopReason string `json:"stopReason"`
						Err        string `json:"error"`
					} `json:"response"`
				}
				if json.Unmarshal([]byte(f.data), &resp) != nil || resp.ID != id {
					continue
				}
				if !resp.Response.Ok {
					fmt.Fprintln(os.Stderr, "xdev attach: "+resp.Response.Err)
					return 1
				}
				if *quiet {
					fmt.Println(resp.Response.Text)
				} else if resp.Response.Text != "" {
					fmt.Println()
				}
				return 0
			case "session_compact":
				if !*quiet {
					fmt.Fprintf(os.Stderr, "\n· context is being compacted\n")
				}
			case "server_overflow":
				fmt.Fprintln(os.Stderr, "xdev attach: fell behind the stream and lost frames")
				return 1
			}
		case <-time.After(60 * time.Millisecond):
		}
	}
	fmt.Fprintln(os.Stderr, "xdev attach: the turn did not finish in time")
	return 1
}
