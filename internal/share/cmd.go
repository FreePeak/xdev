package share

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// Publish renders the session, seals it under a fresh AES-256-GCM key, and
// serves the viewer + ciphertext over loopback. The caller owns the returned
// Server: Close it when the link should stop working.
func Publish(store *session.Store, opts Options, port int) (*Server, error) {
	html, err := Render(FromStore(store, opts))
	if err != nil {
		return nil, err
	}
	blob, key, err := Seal([]byte(html))
	if err != nil {
		return nil, err
	}
	srv := NewServer(shortID(store.ID()), blob, key)
	if err := srv.Start(port); err != nil {
		return nil, err
	}
	return srv, nil
}

// shortID is the path segment a snapshot is served under.
func shortID(id string) string {
	if id == "" {
		return "session"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Run implements `xdev share [-port N] [-export file] [id|path]`: export one
// session, seal it, serve it on loopback, print the view-only link, and block
// until interrupted.
func Run(args []string) error {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	port := fs.Int("port", 0, "loopback port to serve on (0 picks a free one)")
	out := fs.String("export", "", "also write the exported HTML to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// flag stops at the first positional, so a flag after the reference would
	// be silently ignored — refuse it instead of serving the wrong thing.
	if fs.NArg() > 1 {
		return fmt.Errorf("share: unexpected %q — usage: xdev share [-port N] [-export file] [id|path] (flags first)",
			strings.Join(fs.Args()[1:], " "))
	}
	store, err := OpenRef(fs.Arg(0))
	if err != nil {
		return err
	}
	defer store.Close()

	if *out != "" {
		path, err := Export(store, Options{}, *out)
		if err != nil {
			return err
		}
		fmt.Println("exported: " + path)
	}
	srv, err := Publish(store, Options{}, *port)
	if err != nil {
		return err
	}
	defer srv.Close()
	fmt.Println("share link (view-only): " + srv.Link())
	fmt.Println("serving " + srv.Addr() + " — Ctrl-C to stop")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	return nil
}

// OpenRef resolves a session reference: "" is the newest session of this
// directory, an existing file path is that file, and anything else is an id
// prefix (this directory's sessions first, then the whole store).
func OpenRef(ref string) (*session.Store, error) {
	if ref != "" {
		if st, err := os.Stat(ref); err == nil && !st.IsDir() {
			return session.Open(ref)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	metas, err := session.List(config.DataDir())
	if err != nil {
		return nil, fmt.Errorf("share: list sessions: %w", err)
	}
	q := strings.ToLower(ref)
	if q == "" {
		for _, m := range metas {
			if m.CWD != cwd || m.TitleSource == session.TitleSourceSubagent {
				continue // subagent children are never user sessions
			}
			return session.Open(m.Path)
		}
		return nil, fmt.Errorf("share: no session in %s to share", cwd)
	}
	for _, m := range metas {
		if strings.HasPrefix(strings.ToLower(m.ID), q) {
			return session.Open(m.Path)
		}
	}
	return nil, fmt.Errorf("share: no session matching %q", ref)
}
