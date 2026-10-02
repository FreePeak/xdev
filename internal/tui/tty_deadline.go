//go:build !windows

package tui

import (
	"os"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
)

// A frame write must not be able to stop the UI loop.
//
// tcell flushes a frame with ONE blocking write on the tty (tScreen.draw →
// buf.WriteTo(t.tty)). The process above the pane can stop draining it: an
// unfocused pane whose terminal nobody is reading, a slow resize, a pane whose
// child is suspended. The pty buffer fills, that write blocks, and the loop
// stops beating — keys, Ctrl+C and output all stop while the process stays
// alive. ~/.xdev/agent/dumps holds 138 files and 12 of them are exactly that
// stack (goroutine 1 [syscall] → devTty.Write, tty_unix.go:53) with silences
// from 5s to 17m; only the stall watchdog ended those sessions, by restoring
// the terminal and exiting — which is what the user sees, a session that died
// mid-turn.
//
// So the write is bounded instead of trusted. *os.File honours a write
// deadline on a pty slave — measured on this machine with the master never
// read: an unbounded 4KB write never returns, the same write with a 200ms
// deadline returns i/o timeout — and tcell discards the error at every flush
// site, so a timeout IS the drop. The loop keeps running and repairs what
// reached the terminal on the next frame (repairDroppedFrame).
//
// ponytail: a deadline, not a second writer goroutine. The upgrade path is a
// per-frame write queue, which would let a pane paint while its reader lags
// instead of losing frames. Ceiling: a terminal that cannot keep up loses one
// frame per frameWriteTimeout, and each lost frame costs a full repaint, so a
// wedged reader turns the 30fps tick into ~4fps until it drains.
const frameWriteTimeout = 250 * time.Millisecond

// writeDeadliner is the part of *os.File Write needs, so the deadline path is
// testable without a real pty.
type writeDeadliner interface {
	Write(p []byte) (int, error)
	SetWriteDeadline(time.Time) error
}

// deadlineTty is tcell's Tty with the write side re-routed through a second
// handle on the same terminal. tcell's devTty keeps that fd unexported, and it
// already opens a second /dev/tty copy of its own (the macOS pty workaround in
// tty_unix.go Start), so one more is the same shape. Raw mode is a property of
// the terminal, not of an fd, so tcell's MakeRaw governs this handle too.
type deadlineTty struct {
	tcell.Tty
	w writeDeadliner
	// dropped is set when a frame write ran out of time. App reads it after
	// every flush to repaint in full: tcell marks each cell clean BEFORE the
	// write, so a dropped frame leaves a half-painted terminal with nothing
	// left marked dirty.
	dropped atomic.Bool
}

// NewDeadlineScreen returns a screen whose frame writes are bounded by
// frameWriteTimeout, plus the flag recording a dropped frame. The Tty is built
// here because NewTerminfoScreen only reaches for /dev/tty when handed a nil
// Tty — passing our own is tcell's supported seam.
func NewDeadlineScreen() (tcell.Screen, *atomic.Bool, error) {
	tty, err := tcell.NewDevTty()
	if err != nil {
		return nil, nil, err
	}
	w, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	d := &deadlineTty{Tty: tty, w: w}
	scr, err := tcell.NewTerminfoScreenFromTty(d)
	if err != nil {
		_ = w.Close()
		return nil, nil, err
	}
	return scr, &d.dropped, nil
}

// Write is the fix: a deadline on the frame write. The error is returned, not
// swallowed, because tcell ignores it at every flush site and a caller that
// wants to know the terminal is behind reads dropped instead.
func (d *deadlineTty) Write(p []byte) (int, error) {
	if err := d.w.SetWriteDeadline(time.Now().Add(frameWriteTimeout)); err != nil {
		return 0, err
	}
	n, err := d.w.Write(p)
	if err != nil {
		d.dropped.Store(true)
	}
	return n, err
}

// Close releases our handle too; tcell closes only the Tty it was given.
func (d *deadlineTty) Close() error {
	err := d.Tty.Close()
	if c, ok := d.w.(interface{ Close() error }); ok {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
