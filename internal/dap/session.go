package dap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

// One debug session per process (issue #66): two adapters would fight over the
// same process group, the same inferior and the same tool state.
//
// ponytail: a package-level claim instead of a session registry. Ceiling: no
// parallel/multi-target debug sessions; the upgrade path is a registry keyed
// by session id if remote attach ever needs two targets at once.
var (
	activeMu        sync.Mutex
	activeClient    *Client
	activeDesc      string
	activeLaunching bool
)

// initializedWait bounds the (optional) wait for the adapter's `initialized`
// event. dlv never sends one, so this is a courtesy wait, not a gate.
const initializedWait = 2 * time.Second

// dialInWait bounds the wait for a socket adapter to dial back to xdev's
// listener.
const dialInWait = 15 * time.Second

// clientAddrFlag is the flag a socket adapter takes to find xdev's listener.
const clientAddrFlag = "--client-addr"

// claimLaunch reserves the process-wide session slot; a second launch/attach
// is refused with the active session named.
func claimLaunch(desc string) error {
	activeMu.Lock()
	defer activeMu.Unlock()
	if activeClient != nil || activeLaunching {
		who := activeDesc
		if who == "" {
			who = "initializing"
		}
		return fmt.Errorf("debug: a debug session is already active (%s) — end it with op=terminate before starting another", who)
	}
	activeLaunching, activeDesc = true, desc
	return nil
}

func abortLaunch() {
	activeMu.Lock()
	activeLaunching, activeDesc = false, ""
	activeMu.Unlock()
}

func commitLaunch(c *Client, desc string) {
	activeMu.Lock()
	defer activeMu.Unlock()
	activeLaunching = false
	if !c.alive() { // the adapter died during the handshake
		activeClient, activeDesc = nil, ""
		return
	}
	activeClient, activeDesc = c, desc
}

// clearActive frees the slot when c owns it (idempotent; also runs when the
// adapter exits on its own).
func clearActive(c *Client) {
	activeMu.Lock()
	if activeClient == c {
		activeClient, activeDesc = nil, ""
	}
	activeMu.Unlock()
}

// activeSession returns the process-wide session (nil when none).
func activeSession() (*Client, string) {
	activeMu.Lock()
	defer activeMu.Unlock()
	return activeClient, activeDesc
}

// startSession claims the slot, launches the adapter and performs the
// initialize + launch/attach handshake. Every failure path releases the slot.
// spawn starts the adapter process; nil means the real subprocess launcher
// (tests substitute an in-process fake).
func startSession(ctx context.Context, spawn func(name string, spec AdapterSpec, root string) (*Client, error), name string, spec AdapterSpec, root, desc, command string, args map[string]any) (*Client, error) {
	if err := claimLaunch(desc); err != nil {
		return nil, err
	}
	if spawn == nil {
		spawn = startAdapter
	}
	c, err := spawn(name, spec, root)
	if err != nil {
		abortLaunch()
		return nil, err
	}
	if err := handshake(ctx, c, name, command, args); err != nil {
		_ = c.Close()
		abortLaunch()
		return nil, err
	}
	commitLaunch(c, desc)
	return c, nil
}

// startAdapter launches one adapter process and connects its DAP transport:
// stdio for lldb-dap/debugpy, a loopback TCP dial-in for socket adapters
// (dlv's `--client-addr` mode). A missing binary is an actionable error
// naming the setting that points at it, never a crash.
func startAdapter(name string, spec AdapterSpec, root string) (*Client, error) {
	bin, err := exec.LookPath(spec.Command)
	if err != nil {
		return nil, fmt.Errorf(
			"debug/%s: adapter %q not found on PATH — install it, or point debug.adapters.%s.command at another binary",
			name, spec.Command, name)
	}
	args := spec.Args
	var ln net.Listener
	if spec.Socket {
		// xdev listens and the adapter dials back, so the port is ours and
		// no "listening at" banner has to be parsed.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("debug/%s: %w", name, err)
		}
		defer func() { _ = ln.Close() }()
		args = append(append([]string(nil), spec.Args...), clientAddrFlag+"="+ln.Addr().String())
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	prepareProcessGroup(cmd)
	tail := newTailWriter(4 << 10)
	var stdin io.WriteCloser
	var stdout io.Reader
	if ln != nil {
		cmd.Stdout = tail // diagnostics; DAP runs on the dial-in socket
	} else {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return nil, fmt.Errorf("debug/%s: %w", name, err)
		}
		if stdout, err = cmd.StdoutPipe(); err != nil {
			return nil, fmt.Errorf("debug/%s: %w", name, err)
		}
	}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("debug/%s: start %s: %w", name, bin, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	if ln != nil {
		conn, err := acceptDialIn(ln, name)
		if err != nil {
			killProcessGroup(cmd.Process.Pid)
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
			}
			return nil, err
		}
		stdin, stdout = conn, conn
	}
	c := newClient(stdin, stdout, name)
	c.proc = cmd.Process
	c.exited = exited
	c.stderr = tail
	c.start()
	return c, nil
}

// acceptDialIn waits, bounded, for a socket adapter to dial back.
func acceptDialIn(ln net.Listener, name string) (net.Conn, error) {
	if tl, ok := ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(dialInWait))
	}
	conn, err := ln.Accept()
	if err != nil {
		return nil, fmt.Errorf("debug/%s: the adapter never dialed back to %s: %w (it must accept %s)", name, ln.Addr(), err, clientAddrFlag)
	}
	return conn, nil
}

// handshake performs the DAP initialize exchange and then sends command
// ("launch" or "attach") with args.
func handshake(ctx context.Context, c *Client, name, command string, args map[string]any) error {
	caps, err := c.Initialize(ctx, name)
	if err != nil {
		return err
	}
	// The spec has the adapter emit `initialized` after the initialize
	// response, and dlv never does — so the wait is short and its absence is
	// not an error. The event only orders the client's configuration requests;
	// leaving it unread would otherwise sit in the event queue.
	if _, err := c.WaitEvent(ctx, initializedWait, "initialized"); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, err := c.Call(ctx, command, args); err != nil {
		return err
	}
	// An adapter that declares supportsConfigurationDoneRequest holds the
	// debuggee until configurationDone arrives: without it the program never
	// starts and the first stop never comes (dlv, lldb-dap, debugpy all
	// declare it). Breakpoints set after launch still apply to the running
	// debuggee; stop_on_entry is the reliable way to break on the first line.
	if caps.SupportsConfigurationDoneRequest {
		if _, err := c.Call(ctx, "configurationDone", map[string]any{}); err != nil {
			return err
		}
	}
	return nil
}
