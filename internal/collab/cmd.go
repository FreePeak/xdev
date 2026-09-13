package collab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// Run implements the `xdev join <link>` subcommand: join a shared session as
// a guest and mirror it on stdout. With a full link and an interactive stdin,
// typed lines are sent as prompts for the host to run.
func Run(args []string) error {
	fs := flag.NewFlagSet("xdev join", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("name", DefaultName(), "display name shown to other participants")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `xdev join "<link>" — mirror a shared session as a guest

  <link>   the link printed by /collab, e.g.
           ws://127.0.0.1:7575/r/<roomId>.<secret>

A 48-byte secret (key + write token) prompts and interrupts; a 32-byte secret
is view-only. The replica is written under ~/.xdev/collab/.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("collab: join needs exactly one link")
	}
	link, err := ParseLink(fs.Arg(0))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var out sync.Mutex
	print := func(lines ...string) {
		if len(lines) == 0 {
			return
		}
		out.Lock()
		defer out.Unlock()
		for _, l := range lines {
			fmt.Println(l)
		}
	}

	// welcomed distinguishes the handshake from the host's write-permission
	// confirmation (which arrives as a second welcome frame). Both run on
	// the read goroutine, so a plain bool is enough.
	welcomed := false
	g, err := Join(ctx, GuestConfig{
		Link: link,
		Name: *name,
		OnWelcome: func(f Frame) {
			host := f.Name
			if host == "" {
				host = "host"
			}
			// The first welcome frame always says Writable:false: the write
			// token lives in the URL fragment, which a client never sends
			// over HTTP, so capability is proven afterwards by FrameHello.
			// Trusting that frame made every full link announce itself as
			// view-only and then correct itself two lines later. The link
			// parsed locally is the authority on what was handed to us.
			switch {
			case !welcomed:
				welcomed = true
				mode := "view-only (ask the host for the full link to prompt)"
				if link.Full() {
					mode = "full control (you can prompt and interrupt)"
				}
				print("· joined " + host + " in room " + link.RoomID + " — " + mode)
				if !link.Full() {
					print("· view-only link: typed lines are not sent")
				}
			case f.Writable && !link.Full():
				print("· write permission granted")
			}
		},
		OnSnapshot: func(data []byte) {
			msgs := Messages(data)
			if len(msgs) == 0 {
				print("· (empty transcript)")
				return
			}
			print(MirrorLines(msgs)...)
		},
		OnEntry: func(line []byte) {
			print(MirrorLines(Messages(line))...)
		},
		OnEvent: func(raw json.RawMessage) {
			if t := NoticeText(raw); t != "" {
				print("· " + t)
			}
		},
	})
	if err != nil {
		return err
	}
	defer g.Close()
	switch {
	case g.Writable() && stdinIsTerminal():
		go forwardStdin(ctx, g)
	}
	runErr := g.Run(ctx)
	switch {
	case runErr != nil:
		return runErr
	case ctx.Err() != nil:
		print("· left the room")
	default:
		print("· the host stopped sharing")
	}
	return nil
}

// forwardStdin sends typed lines to the host as prompts (full links only).
func forwardStdin(ctx context.Context, g *Guest) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := g.Prompt(line); err != nil {
			fmt.Fprintln(os.Stderr, "collab:", err)
		}
	}
}

// stdinIsTerminal reports whether stdin is an interactive TTY.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
