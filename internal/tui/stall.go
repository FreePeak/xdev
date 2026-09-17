package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The UI loop is single-goroutine by design: Run calls handleKey and draw
// inline (app.go). That is what makes the TUI cheap to reason about, and it is
// also why any operation that fails to return there is indistinguishable from
// a dead terminal — keys stop, Ctrl+C stops, output stops, but the process is
// alive. A report of "the whole TUI froze" cannot be diagnosed from the
// outside after the fact, so the app diagnoses itself.
//
// The watchdog runs on its own goroutine and never takes App.mu: if it did, a
// loop stuck while holding that mutex would block the very thing meant to
// record it. It only reads an atomic heartbeat.
const (
	// stallAfter is how long one loop iteration may take before it is
	// considered hung. Normal draws are sub-millisecond; a turn's work runs
	// on the agent goroutine, so a slow iteration here is a real stall, not
	// just a busy frame. Generous on purpose: a false alarm in a frozen-UI
	// file is worse than a five-second delay in writing the true one.
	stallAfter = 5 * time.Second
	// stallCheck is the watchdog's poll interval.
	stallCheck = time.Second
	// stallStackCap bounds the dump. Every goroutine's stack can exceed a
	// megabyte on a busy session; the head is where the stuck loop is, and a
	// truncated tail is still worth more than no file.
	stallStackCap = 1 << 20
)

// SetStallDumpDir enables the UI-loop stall dump. dir is where
// tui-stall-<timestamp>.txt files are written (the same <dataDir>/dumps tree
// `xdev dump` uses, so `xdev gc` already knows to collect them). Empty dir
// disables the watchdog. Call before Run.
func (a *App) SetStallDumpDir(dir string) {
	a.stallDir = dir
}

// beat records that the UI loop is making progress. Called from Run once per
// iteration, so a hung handleKey/draw simply stops updating it.
func (a *App) beat() { a.loopBeat.Store(time.Now().UnixNano()) }

// watchStall is the watchdog: it watches for the loop to stop beating and
// writes the goroutine stacks that explain why. The thresholds and the stop
// channel are arguments rather than package state, so the goroutine reads
// nothing mutable — a test (and a future caller) can run one at a different
// cadence without racing another. It exits when stop closes.
func (a *App) watchStall(after, check time.Duration, stop <-chan struct{}) {
	if a.stallDir == "" {
		return
	}
	tick := time.NewTicker(check)
	defer tick.Stop()
	// dumpedForBeat identifies the stall episode: it holds the beat timestamp
	// the current dump was written for, so one episode yields one file (the
	// loop may never recover, and a file per second would bury the useful
	// first one). A NEW episode is a beat that has advanced past it, i.e. the
	// loop did recover and hung again.
	dumpedForBeat := int64(-1)
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			beat := a.loopBeat.Load()
			if now.Sub(time.Unix(0, beat)) < after {
				continue
			}
			if beat == dumpedForBeat {
				continue
			}
			dumpedForBeat = beat
			// stderr, never the screen: the loop is stuck so nothing can be
			// drawn, and raw-mode output still lands where it can be read.
			fmt.Fprintf(os.Stderr, "\nxdev: UI loop stalled for %s (pid %d)\n",
				now.Sub(time.Unix(0, beat)).Truncate(time.Second), os.Getpid())
			if path, err := a.dumpStall(now, beat); err != nil {
				fmt.Fprintf(os.Stderr, "xdev: stall dump failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "xdev: stall dump written to %s\n", path)
			}
		}
	}
}

// dumpStall captures every goroutine and writes it under the dumps dir.
func (a *App) dumpStall(at time.Time, beat int64) (string, error) {
	buf := make([]byte, stallStackCap)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	if err := os.MkdirAll(a.stallDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(a.stallDir, fmt.Sprintf("tui-stall-%s.txt", at.Format("20060102-150405")))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// The header deliberately avoids App state: reading model/session needs
	// the mutex the stuck loop may be holding.
	head := fmt.Sprintf("xdev UI loop stall\n"+
		"written: %s\npid: %d\nlast loop beat: %s (%s ago)\n"+
		"--- goroutine stacks ---\n",
		at.Format(time.RFC3339), os.Getpid(),
		time.Unix(0, beat).Format(time.RFC3339Nano),
		at.Sub(time.Unix(0, beat)).Truncate(time.Millisecond))
	if _, err := f.WriteString(head); err != nil {
		return "", err
	}
	if _, err := f.Write(buf); err != nil {
		return "", err
	}
	return path, nil
}

// startStallWatchdog arms the watchdog with the shipped thresholds.
func (a *App) startStallWatchdog() { go a.watchStall(stallAfter, stallCheck, a.quitCh) }
